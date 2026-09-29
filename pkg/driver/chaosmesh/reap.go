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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ExpiryAnnotation carries a fault's deadline on the object Apply creates,
// the same key the network-policy engine uses.
const ExpiryAnnotation = "simian.chaos/expires-at"

// reapGrace is how long past its deadline an object is left before
// ReapExpired deletes it. Chaos Mesh has already recovered the fault by then,
// so nothing is waiting on the delete; the grace leaves this process's own
// faults to the lease registry, which ends them with their fault UID attached.
const reapGrace = time.Minute

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
		cleared []string
		errs    []error
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
	return cleared, errors.Join(errs...)
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
