// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package chaosmesh

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/go-steer/simian-agent/pkg/simian"
)

// ExpiryAnnotation carries a fault's deadline on the object Apply creates,
// the same key the network-policy engine uses.
const ExpiryAnnotation = "simian.chaos/expires-at"

// reapGrace is how long past its deadline an object is left before
// ReapExpired deletes it. Chaos Mesh has already recovered the fault by then,
// so nothing is waiting on the delete; the grace leaves this process's own
// faults to the lease registry, which ends them with their fault UID attached.
const reapGrace = time.Minute

// stuckAfter is how long an object may sit with a deletionTimestamp before
// ReapExpired reports it. Chaos Mesh's own recovery normally lets the delete
// through in seconds; one still terminating after this is held by a finalizer
// nothing is going to remove.
const stuckAfter = 5 * time.Minute

// ReapExpired implements simian.OrphanReaper by deleting Simian-managed Chaos
// Mesh objects whose duration has passed.
//
// The fault itself does not need this: the chaos-controller-manager honours
// spec.duration and recovers on time whether or not Simian is alive. The
// object does. After a crash nothing deletes it, so it stays in the arena —
// managed label, full spec and all — until someone does it by hand, which in
// the 39-hour GKE trial is what happened. That leaves the answer to a finished
// scenario readable in the namespace the next one investigates, and counts as
// an active fault in the arena's pre-destroy check.
//
// Only objects that prove they are expired are deleted: the deadline Apply
// stamped, or for objects created before it did, creation time plus
// spec.duration. An object with neither is left alone.
func (d *Driver) ReapExpired(ctx context.Context, namespaces []string, now time.Time) ([]string, error) {
	version, resources, err := d.faultResources()
	if err != nil {
		return nil, fmt.Errorf("chaos-mesh reap: %w", err)
	}
	gv, err := schema.ParseGroupVersion(version)
	if err != nil && version != "" {
		return nil, fmt.Errorf("chaos-mesh reap: %w", err)
	}
	var (
		cleared   []string
		errs      []error
		seenStuck = map[string]bool{}
	)
	for _, r := range resources {
		if !r.Namespaced {
			continue
		}
		gvr := gv.WithResource(r.Name)
		for _, ns := range namespaces {
			list, err := d.dyn.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{
				LabelSelector: "simian.chaos/managed=true",
			})
			if err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				errs = append(errs, fmt.Errorf("chaos-mesh reap: list %s in %s: %w", r.Name, ns, err))
				continue
			}
			for i := range list.Items {
				obj := &list.Items[i]
				if del := obj.GetDeletionTimestamp(); del != nil {
					// Already being deleted. Deleting it again is a no-op that
					// succeeds, and reporting that as a reap is what put 1,561
					// orphan-reaped events in the trail for three objects (#157).
					uid := engineUID(ns, obj.GetName(), gvr)
					seenStuck[uid] = true
					if now.Sub(del.Time) >= stuckAfter && d.firstStuckReport(uid) {
						errs = append(errs, stuckError(r.Kind, ns, obj, now.Sub(del.Time)))
					}
					continue
				}
				deadline, ok := deadlineOf(obj)
				if !ok || !deadline.Add(reapGrace).Before(now) {
					continue
				}
				err := d.dyn.Resource(gvr).Namespace(ns).Delete(ctx, obj.GetName(), metav1.DeleteOptions{})
				if err != nil && !apierrors.IsNotFound(err) {
					errs = append(errs, fmt.Errorf("chaos-mesh reap: delete %s %s/%s: %w", r.Kind, ns, obj.GetName(), err))
					continue
				}
				cleared = append(cleared, engineUID(ns, obj.GetName(), gvr))
			}
		}
	}
	d.forgetStuckExcept(seenStuck)
	return cleared, errors.Join(errs...)
}

// stuckError says which object will not go away, what holds it, and how to
// end it by hand.
//
// It is not removed automatically. Chaos Mesh's chaos-mesh/records finalizer
// is how it remembers to recover pods it injected; when injection failed
// outright there is nothing to recover, but a partial injection would be left
// in place for good. Which one this is takes looking at the target pods.
func stuckError(kind, ns string, obj *unstructured.Unstructured, age time.Duration) error {
	return fmt.Errorf("chaos-mesh reap: %s %s/%s has been deleting for %s, held by finalizers [%s]; "+
		"Chaos Mesh has not recovered it. Check its target pods, then: kubectl -n %s patch %s %s --type=merge -p '{\"metadata\":{\"finalizers\":null}}'",
		kind, ns, obj.GetName(), age.Truncate(time.Second), strings.Join(obj.GetFinalizers(), ", "),
		ns, strings.ToLower(kind), obj.GetName())
}

// firstStuckReport records that uid has been reported stuck and says whether
// this is the first time, so each stuck object is reported once per process
// rather than on every tick.
func (d *Driver) firstStuckReport(uid string) bool {
	d.stuckMu.Lock()
	defer d.stuckMu.Unlock()
	if d.stuckReported == nil {
		d.stuckReported = map[string]bool{}
	}
	if d.stuckReported[uid] {
		return false
	}
	d.stuckReported[uid] = true
	return true
}

// forgetStuckExcept drops reported objects that are no longer terminating, so
// the set stays as small as what is actually stuck.
func (d *Driver) forgetStuckExcept(seen map[string]bool) {
	d.stuckMu.Lock()
	defer d.stuckMu.Unlock()
	for uid := range d.stuckReported {
		if !seen[uid] {
			delete(d.stuckReported, uid)
		}
	}
}

// deadlineOf reads when a Chaos Mesh object's fault ends. An unparseable
// value is treated as absent: never delete on a guess.
func deadlineOf(obj *unstructured.Unstructured) (time.Time, bool) {
	if raw := obj.GetAnnotations()[ExpiryAnnotation]; raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		return t, err == nil
	}
	raw, _, _ := unstructured.NestedString(obj.Object, "spec", "duration")
	created := obj.GetCreationTimestamp()
	if raw == "" || created.IsZero() {
		return time.Time{}, false
	}
	dur, err := time.ParseDuration(raw)
	if err != nil || dur <= 0 {
		return time.Time{}, false
	}
	return created.Add(dur), true
}

// ListLive implements simian.LiveFaultLister: the Simian-managed Chaos Mesh
// objects in namespaces whose deadline is after now.
func (d *Driver) ListLive(ctx context.Context, namespaces []string, now time.Time) ([]simian.ActiveFault, error) {
	version, resources, err := d.faultResources()
	if err != nil {
		return nil, fmt.Errorf("chaos-mesh list live: %w", err)
	}
	gv, err := schema.ParseGroupVersion(version)
	if err != nil && version != "" {
		return nil, fmt.Errorf("chaos-mesh list live: %w", err)
	}
	var (
		out  []simian.ActiveFault
		errs []error
	)
	for _, r := range resources {
		if !r.Namespaced {
			continue
		}
		gvr := gv.WithResource(r.Name)
		for _, ns := range namespaces {
			list, err := d.dyn.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{
				LabelSelector: "simian.chaos/managed=true",
			})
			if err != nil {
				if !apierrors.IsNotFound(err) {
					errs = append(errs, fmt.Errorf("chaos-mesh list live: %s in %s: %w", r.Name, ns, err))
				}
				continue
			}
			for i := range list.Items {
				obj := &list.Items[i]
				faultUID := obj.GetLabels()["simian.chaos/fault-uid"]
				deadline, ok := deadlineOf(obj)
				if obj.GetDeletionTimestamp() != nil || faultUID == "" || !ok || !deadline.After(now) {
					continue
				}
				out = append(out, simian.ActiveFault{
					FaultUID:  faultUID,
					EngineUID: engineUID(ns, obj.GetName(), gvr),
					Manifest: simian.FaultManifest{
						UID:          faultUID,
						Engine:       simian.EngineChaosMesh,
						APIVersion:   obj.GetAPIVersion(),
						ResourceKind: r.Kind,
						Targets:      simian.ObjectTargets(obj.GetLabels(), obj.GetAnnotations(), ns),
					},
					AppliedAt: obj.GetCreationTimestamp().UTC(),
					Deadline:  deadline,
				})
			}
		}
	}
	return out, errors.Join(errs...)
}
