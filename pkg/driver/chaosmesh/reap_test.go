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
	"testing"
	"time"

	"github.com/go-steer/simian-agent/pkg/simian"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
