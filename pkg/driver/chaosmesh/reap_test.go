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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/simian-agent/pkg/simian"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clienttesting "k8s.io/client-go/testing"
)

var _ simian.OrphanReaper = (*Driver)(nil)

func seedChaos(t *testing.T, d *Driver, ns, name string, labels, annotations map[string]string, created time.Time, duration string) {
	t.Helper()
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": APIGroup + "/v1alpha1",
		"kind":       "HTTPChaos",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"spec":       map[string]any{},
	}}
	if duration != "" {
		obj.Object["spec"] = map[string]any{"duration": duration}
	}
	obj.SetLabels(labels)
	obj.SetAnnotations(annotations)
	obj.SetCreationTimestamp(metav1.NewTime(created))
	if _, err := d.dyn.Resource(httpChaosGVR).Namespace(ns).Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed %s/%s: %v", ns, name, err)
	}
}

func remaining(t *testing.T, d *Driver, ns string) []string {
	t.Helper()
	list, err := d.dyn.Resource(httpChaosGVR).Namespace(ns).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var out []string
	for _, o := range list.Items {
		out = append(out, o.GetName())
	}
	slices.Sort(out)
	return out
}

// #129: after a crash, a Chaos Mesh fault recovers on time but its object
// stays in the arena, spec and all, until someone deletes it.
func TestReapExpiredDeletesOnlyManagedObjectsProvablyPastTheirDeadline(t *testing.T) {
	d, _ := newTestDriver(t)
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	managed := map[string]string{"simian.chaos/managed": "true"}
	expiresAt := func(t time.Time) map[string]string {
		return map[string]string{ExpiryAnnotation: t.Format(time.RFC3339)}
	}

	seedChaos(t, d, "bank", "expired", managed, expiresAt(now.Add(-10*time.Minute)), now.Add(-time.Hour), "")
	seedChaos(t, d, "bank", "in-grace", managed, expiresAt(now.Add(-30*time.Second)), now.Add(-time.Hour), "")
	seedChaos(t, d, "bank", "running", managed, expiresAt(now.Add(5*time.Minute)), now.Add(-time.Minute), "")
	// Created before Apply stamped a deadline: creation time plus duration.
	seedChaos(t, d, "bank", "old-expired", managed, nil, now.Add(-time.Hour), "10m")
	seedChaos(t, d, "bank", "old-running", managed, nil, now.Add(-5*time.Minute), "10m")
	seedChaos(t, d, "bank", "no-deadline", managed, nil, now.Add(-time.Hour), "")
	seedChaos(t, d, "bank", "garbled", managed, map[string]string{ExpiryAnnotation: "soon"}, now.Add(-time.Hour), "1m")
	seedChaos(t, d, "bank", "not-ours", nil, expiresAt(now.Add(-time.Hour)), now.Add(-2*time.Hour), "")
	seedChaos(t, d, "other", "elsewhere", managed, expiresAt(now.Add(-time.Hour)), now.Add(-2*time.Hour), "")

	cleared, err := d.ReapExpired(context.Background(), []string{"bank"}, now)
	if err != nil {
		t.Fatalf("ReapExpired: %v", err)
	}
	slices.Sort(cleared)
	want := []string{engineUID("bank", "expired", httpChaosGVR), engineUID("bank", "old-expired", httpChaosGVR)}
	if !slices.Equal(cleared, want) {
		t.Errorf("cleared = %v, want %v", cleared, want)
	}
	if got := remaining(t, d, "bank"); !slices.Equal(got, []string{"garbled", "in-grace", "no-deadline", "not-ours", "old-running", "running"}) {
		t.Errorf("left in bank = %v", got)
	}
	if got := remaining(t, d, "other"); len(got) != 1 {
		t.Errorf("a namespace not asked about was swept: %v", got)
	}
}

func TestApplyStampsTheDeadlineTheReaperReads(t *testing.T) {
	d, _ := newTestDriver(t)
	before := time.Now()
	uid, err := d.Apply(context.Background(), simian.FaultManifest{
		UID: "u-1", Engine: simian.EngineChaosMesh, APIVersion: APIGroup + "/v1alpha1", ResourceKind: "HTTPChaos",
		Duration: 10 * time.Minute, Targets: []simian.TargetRef{{Namespace: "bank"}},
		Spec: map[string]any{"target": "Request"},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ns, name, _, _ := decodeEngineUID(uid)
	obj, err := d.dyn.Resource(httpChaosGVR).Namespace(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	deadline, ok := deadlineOf(obj)
	if !ok || deadline.Before(before.Add(10*time.Minute).Truncate(time.Second)) || deadline.After(time.Now().Add(10*time.Minute)) {
		t.Errorf("deadline = %v (ok=%v), want ~10m from now", deadline, ok)
	}
	if obj.GetAnnotations()[simian.TargetNamespacesAnnotation] != "bank" {
		t.Errorf("target-namespace annotation lost: %v", obj.GetAnnotations())
	}
}

// seedTerminating seeds an expired managed object that has been deleting
// since since and is held by Chaos Mesh's finalizer, the state a failed
// IOChaos was left in on the 2026-09-30 trial.
func seedTerminating(t *testing.T, d *Driver, ns, name string, since time.Time) {
	t.Helper()
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": APIGroup + "/v1alpha1",
		"kind":       "HTTPChaos",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"spec":       map[string]any{},
	}}
	obj.SetLabels(map[string]string{"simian.chaos/managed": "true"})
	obj.SetAnnotations(map[string]string{ExpiryAnnotation: since.Add(-30 * time.Second).Format(time.RFC3339)})
	obj.SetCreationTimestamp(metav1.NewTime(since.Add(-time.Minute)))
	del := metav1.NewTime(since)
	obj.SetDeletionTimestamp(&del)
	obj.SetFinalizers([]string{"chaos-mesh/records"})
	if _, err := d.dyn.Resource(httpChaosGVR).Namespace(ns).Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed %s/%s: %v", ns, name, err)
	}
}

func deletes(fake *clienttesting.Fake) int {
	n := 0
	for _, a := range fake.Actions() {
		if a.GetVerb() == "delete" {
			n++
		}
	}
	return n
}

// A Chaos Mesh object whose injection failed keeps its chaos-mesh/records
// finalizer: it gets a deletionTimestamp and never goes away. The reaper
// deleted it again on every tick — a no-op that succeeds — and reported each
// one as a reap: 1,561 orphan-reaped events for three objects over five hours
// of the 2026-09-30 trial (#157). Something already being deleted is not the
// reaper's to delete, and not a reap.
func TestAnObjectAlreadyBeingDeletedIsNotDeletedOrReportedAgain(t *testing.T) {
	d, fake := newTestDriver(t)
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.UTC)
	seedTerminating(t, d, "boutique", "simian-8jxr9", now.Add(-time.Minute))

	cleared, err := d.ReapExpired(context.Background(), []string{"boutique"}, now)
	if err != nil {
		t.Fatalf("ReapExpired: %v", err)
	}
	if len(cleared) != 0 {
		t.Errorf("cleared = %v; an object already terminating is not a reap", cleared)
	}
	if n := deletes(fake); n != 0 {
		t.Errorf("issued %d deletes for an object already being deleted", n)
	}
}

// Still terminating well past the point Chaos Mesh would have let it go, it
// is held by a finalizer nothing will remove, and someone has to look. Said
// once, with what holds it and how to end it — not once per tick.
func TestAnObjectStuckDeletingIsReportedOnceWithWhatHoldsIt(t *testing.T) {
	d, fake := newTestDriver(t)
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.UTC)
	seedTerminating(t, d, "boutique", "simian-8jxr9", now.Add(-25*time.Minute))

	_, err := d.ReapExpired(context.Background(), []string{"boutique"}, now)
	if err == nil {
		t.Fatal("a stuck object went unreported")
	}
	for _, want := range []string{"boutique/simian-8jxr9", "chaos-mesh/records", "25m0s", "kubectl -n boutique patch httpchaos simian-8jxr9"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("report %q does not say %q", err, want)
		}
	}

	for tick := 1; tick <= 3; tick++ {
		if _, err := d.ReapExpired(context.Background(), []string{"boutique"}, now.Add(time.Duration(tick)*30*time.Second)); err != nil {
			t.Errorf("tick %d reported it again: %v", tick, err)
		}
	}
	if n := deletes(fake); n != 0 {
		t.Errorf("issued %d deletes for a stuck object", n)
	}

	// Once it is gone it is forgotten, so the set holds only what is stuck.
	if err := d.dyn.Resource(httpChaosGVR).Namespace("boutique").Delete(context.Background(), "simian-8jxr9", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ReapExpired(context.Background(), []string{"boutique"}, now.Add(5*time.Minute)); err != nil {
		t.Fatalf("ReapExpired: %v", err)
	}
	if len(d.stuckReported) != 0 {
		t.Errorf("still remembering %v after it went away", d.stuckReported)
	}
}

var _ simian.LiveFaultLister = (*Driver)(nil)

// #177: the Chaos Mesh objects still running, as leases a restarted
// controller adopts, acting where their marks say.
func TestListLiveReportsOnlyManagedObjectsStillRunning(t *testing.T) {
	d, _ := newTestDriver(t)
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	ours := func(uid string) map[string]string {
		return map[string]string{"simian.chaos/managed": "true", "simian.chaos/fault-uid": uid}
	}
	expiresAt := func(t time.Time, extra ...string) map[string]string {
		m := map[string]string{ExpiryAnnotation: t.Format(time.RFC3339)}
		for i := 0; i+1 < len(extra); i += 2 {
			m[extra[i]] = extra[i+1]
		}
		return m
	}
	seedChaos(t, d, "bank", "running", ours("f-running"), expiresAt(now.Add(5*time.Minute), simian.TargetNamespacesAnnotation, "bank,shop"), now.Add(-time.Minute), "")
	seedChaos(t, d, "bank", "old-running", ours("f-old"), nil, now.Add(-5*time.Minute), "10m")
	seedChaos(t, d, "bank", "expired", ours("f-expired"), expiresAt(now.Add(-time.Minute)), now.Add(-time.Hour), "")
	seedChaos(t, d, "bank", "no-deadline", ours("f-none"), nil, now.Add(-time.Hour), "")
	seedChaos(t, d, "bank", "no-uid", map[string]string{"simian.chaos/managed": "true"}, expiresAt(now.Add(time.Minute)), now, "")
	seedChaos(t, d, "bank", "not-ours", nil, expiresAt(now.Add(time.Minute)), now, "")

	live, err := d.ListLive(context.Background(), []string{"bank"}, now)
	if err != nil {
		t.Fatalf("ListLive: %v", err)
	}
	byUID := map[string]simian.ActiveFault{}
	for _, af := range live {
		byUID[af.FaultUID] = af
	}
	if len(byUID) != 2 || byUID["f-running"].FaultUID == "" || byUID["f-old"].FaultUID == "" {
		t.Fatalf("live = %+v, want f-running and f-old", live)
	}
	r := byUID["f-running"]
	if r.EngineUID != engineUID("bank", "running", httpChaosGVR) || r.Manifest.ResourceKind != "HTTPChaos" ||
		!r.Deadline.Equal(now.Add(5*time.Minute)) || len(r.Manifest.Targets) != 2 || r.Manifest.Targets[1].Namespace != "shop" {
		t.Errorf("f-running = %+v", r)
	}
	if !byUID["f-old"].Deadline.Equal(now.Add(5 * time.Minute)) {
		t.Errorf("f-old deadline = %v, want creation plus duration", byUID["f-old"].Deadline)
	}
}
