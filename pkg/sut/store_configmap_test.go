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

package sut

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// arenas is the namespace resolver List needs — in production it is the same
// eligible-arena resolver the orphan reaper uses.
func arenas(ns ...string) func(context.Context) ([]string, error) {
	return func(context.Context) ([]string, error) { return ns, nil }
}

func sampleBaseline(ns string) Baseline {
	return Baseline{
		Namespace:       ns,
		SUT:             "online-boutique",
		EstablishedAt:   time.Date(2026, 5, 15, 14, 0, 0, 0, time.UTC),
		StabilityWindow: 30 * time.Second,
		Workloads: []WorkloadStatus{
			{Kind: "Deployment", Name: "frontend", DesiredReplicas: 1, ReadyReplicas: 1},
			{Kind: "Deployment", Name: "cartservice", DesiredReplicas: 2, ReadyReplicas: 2},
		},
	}
}

func TestConfigMapStoreSaveLoadRoundTrip(t *testing.T) {
	client := fake.NewClientset()
	store := NewConfigMapStore(client, arenas("ns-a"))
	ctx := context.Background()
	bl := sampleBaseline("boutique-m3")

	if err := store.Save(ctx, bl); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, ok, err := store.Load(ctx, "boutique-m3")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !ok {
		t.Fatalf("Load: ok=false, expected true")
	}
	if got.Namespace != bl.Namespace || got.SUT != bl.SUT {
		t.Errorf("identity mismatch: got=%+v want=%+v", got, bl)
	}
	if !got.EstablishedAt.Equal(bl.EstablishedAt) {
		t.Errorf("EstablishedAt: got=%v want=%v", got.EstablishedAt, bl.EstablishedAt)
	}
	if got.StabilityWindow != bl.StabilityWindow {
		t.Errorf("StabilityWindow: got=%v want=%v", got.StabilityWindow, bl.StabilityWindow)
	}
	if len(got.Workloads) != len(bl.Workloads) {
		t.Fatalf("Workloads count: got=%d want=%d", len(got.Workloads), len(bl.Workloads))
	}
	for i := range got.Workloads {
		if got.Workloads[i] != bl.Workloads[i] {
			t.Errorf("Workloads[%d]: got=%+v want=%+v", i, got.Workloads[i], bl.Workloads[i])
		}
	}
}

func TestConfigMapStoreSaveIsUpsert(t *testing.T) {
	client := fake.NewClientset()
	store := NewConfigMapStore(client, arenas("ns-a"))
	ctx := context.Background()

	bl := sampleBaseline("boutique-m3")
	if err := store.Save(ctx, bl); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	bl.Workloads[0].ReadyReplicas = 99
	if err := store.Save(ctx, bl); err != nil {
		t.Fatalf("second Save (upsert): %v", err)
	}
	got, _, err := store.Load(ctx, "boutique-m3")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Workloads[0].ReadyReplicas != 99 {
		t.Errorf("Workloads[0].ReadyReplicas: got=%d want=99 (upsert should overwrite)", got.Workloads[0].ReadyReplicas)
	}
}

func TestConfigMapStoreLoadMissingReturnsNotPresent(t *testing.T) {
	client := fake.NewClientset()
	store := NewConfigMapStore(client, arenas("ns-a"))
	bl, ok, err := store.Load(context.Background(), "no-such-namespace")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ok {
		t.Errorf("Load on missing ns: ok=true (got %+v), want false", bl)
	}
}

func TestConfigMapStoreDeleteRemovesEntry(t *testing.T) {
	client := fake.NewClientset()
	store := NewConfigMapStore(client, arenas("ns-a"))
	ctx := context.Background()
	if err := store.Save(ctx, sampleBaseline("boutique-m3")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Delete(ctx, "boutique-m3"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, ok, err := store.Load(ctx, "boutique-m3")
	if err != nil {
		t.Fatalf("Load after delete: %v", err)
	}
	if ok {
		t.Errorf("Load after delete: ok=true, want false")
	}
}

func TestConfigMapStoreDeleteMissingIsNoError(t *testing.T) {
	client := fake.NewClientset()
	store := NewConfigMapStore(client, arenas("ns-a"))
	if err := store.Delete(context.Background(), "no-such-namespace"); err != nil {
		t.Errorf("Delete on missing ns: got error %v, want nil", err)
	}
}

func TestConfigMapStoreListReturnsAllPersistedBaselines(t *testing.T) {
	client := fake.NewClientset()
	store := NewConfigMapStore(client, arenas("ns-a", "ns-b"))
	ctx := context.Background()

	if err := store.Save(ctx, sampleBaseline("ns-a")); err != nil {
		t.Fatalf("Save ns-a: %v", err)
	}
	if err := store.Save(ctx, sampleBaseline("ns-b")); err != nil {
		t.Fatalf("Save ns-b: %v", err)
	}
	all, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("List count: got %d want 2", len(all))
	}
	seen := map[string]bool{}
	for _, bl := range all {
		seen[bl.Namespace] = true
	}
	if !seen["ns-a"] || !seen["ns-b"] {
		t.Errorf("List returned wrong namespaces: %+v", seen)
	}
}

func TestConfigMapStoreListIgnoresUnrelatedConfigMaps(t *testing.T) {
	// A ConfigMap in a SUT namespace that has neither our managed-by label
	// nor our component label must be ignored by List — we shouldn't
	// accidentally try to JSON-decode arbitrary user data.
	client := fake.NewClientset(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "user-config",
				Namespace: "ns-a",
				// No managed-by / component labels.
			},
			Data: map[string]string{"baseline.json": "this is not JSON at all"},
		},
	)
	store := NewConfigMapStore(client, arenas("ns-a", "ns-b"))
	if err := store.Save(context.Background(), sampleBaseline("ns-b")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	all, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 1 || all[0].Namespace != "ns-b" {
		t.Fatalf("List should ignore unlabeled CM; got %+v", all)
	}
}

func TestManagerLoadCachedBaselinesWarmsInMemory(t *testing.T) {
	// Persist a baseline to the store, then simulate a fresh Manager (no
	// in-memory cache) and prove LoadCachedBaselines warms it.
	client := fake.NewClientset()
	store := NewConfigMapStore(client, arenas("warm-me"))
	ctx := context.Background()
	if err := store.Save(ctx, sampleBaseline("warm-me")); err != nil {
		t.Fatalf("Save: %v", err)
	}

	m := &Manager{
		Store:     store,
		baselines: map[string]Baseline{},
	}
	if _, ok := m.Baseline("warm-me"); ok {
		t.Fatalf("baseline should not be in cache before LoadCachedBaselines")
	}
	n, err := m.LoadCachedBaselines(ctx)
	if err != nil {
		t.Fatalf("LoadCachedBaselines: %v", err)
	}
	if n != 1 {
		t.Errorf("loaded count: got %d want 1", n)
	}
	bl, ok := m.Baseline("warm-me")
	if !ok {
		t.Fatalf("baseline should be in cache after LoadCachedBaselines")
	}
	if bl.SUT != "online-boutique" {
		t.Errorf("warmed baseline content mismatch: %+v", bl)
	}
}

func TestNoopStoreImplementsInterface(t *testing.T) {
	// Compile-time check that noopStore satisfies BaselineStore.
	var _ BaselineStore = noopStore{}
	// And that calls don't panic.
	ns := noopStore{}
	if err := ns.Save(context.Background(), Baseline{}); err != nil {
		t.Errorf("noopStore.Save: %v", err)
	}
	_, ok, err := ns.Load(context.Background(), "ns")
	if err != nil || ok {
		t.Errorf("noopStore.Load: %v ok=%v", err, ok)
	}
	if err := ns.Delete(context.Background(), "ns"); err != nil {
		t.Errorf("noopStore.Delete: %v", err)
	}
	got, err := ns.List(context.Background())
	if err != nil || got != nil {
		t.Errorf("noopStore.List: %v len=%d", err, len(got))
	}
}

// The bug this replaced: List used a cluster-wide list, which RBAC cannot
// scope by label, so it needed read on every ConfigMap in the cluster. It must
// now look only where it was told to look — both because the permission is not
// there in a deployed cluster, and because a chaos tool should not be reading
// ConfigMaps in namespaces nobody opted in.
func TestConfigMapStoreListLooksOnlyInTheNamespacesItWasGiven(t *testing.T) {
	client := fake.NewClientset()
	ctx := context.Background()

	// Two arenas and one namespace that is not an arena. All three hold a
	// perfectly valid, correctly-labelled baseline.
	seed := NewConfigMapStore(client, arenas("arena-a", "arena-b", "not-an-arena"))
	for _, ns := range []string{"arena-a", "arena-b", "not-an-arena"} {
		if err := seed.Save(ctx, sampleBaseline(ns)); err != nil {
			t.Fatalf("Save %s: %v", ns, err)
		}
	}

	store := NewConfigMapStore(client, arenas("arena-a", "arena-b"))
	all, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := map[string]bool{}
	for _, bl := range all {
		got[bl.Namespace] = true
	}
	if got["not-an-arena"] {
		t.Error("List reached into a namespace that was not an arena")
	}
	if !got["arena-a"] || !got["arena-b"] || len(got) != 2 {
		t.Errorf("List = %v, want exactly arena-a and arena-b", got)
	}

	// And it must actually be scoping the request, not filtering afterwards:
	// a cluster-wide list would show up as a request with no namespace.
	for _, a := range client.Actions() {
		if a.Matches("list", "configmaps") && a.GetNamespace() == "" {
			t.Error("List issued a cluster-wide configmaps list; that is the permission we cannot have")
		}
	}
}

// A single unreadable arena must not cost us every other arena's baseline —
// that would turn one bad namespace into a cluster-wide cold cache.
func TestConfigMapStoreListReturnsWhatItCouldReadAlongsideTheError(t *testing.T) {
	client := fake.NewClientset()
	ctx := context.Background()
	seed := NewConfigMapStore(client, arenas("ok-1", "ok-2"))
	for _, ns := range []string{"ok-1", "ok-2"} {
		if err := seed.Save(ctx, sampleBaseline(ns)); err != nil {
			t.Fatalf("Save %s: %v", ns, err)
		}
	}
	client.PrependReactor("list", "configmaps", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() == "forbidden" {
			return true, nil, apierrors.NewForbidden(
				schema.GroupResource{Resource: "configmaps"}, "", context.DeadlineExceeded)
		}
		return false, nil, nil
	})

	store := NewConfigMapStore(client, arenas("ok-1", "forbidden", "ok-2"))
	all, err := store.List(ctx)

	if err == nil {
		t.Fatal("an unreadable namespace must be reported, not swallowed")
	}
	if !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("error should name the namespace that failed: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("got %d baselines, want the 2 readable ones despite the failure", len(all))
	}
}

// The resolver is a constructor argument precisely so it cannot be forgotten.
// If one ever is, say so plainly instead of silently reporting no baselines.
func TestConfigMapStoreListWithoutAResolverSaysSo(t *testing.T) {
	_, err := NewConfigMapStore(fake.NewClientset(), nil).List(context.Background())
	if err == nil || !strings.Contains(err.Error(), "namespace resolver") {
		t.Errorf("expected a clear no-resolver error, got %v", err)
	}
}

// The warm path has to keep partial results too, or the split above is undone
// one layer up.
func TestLoadCachedBaselinesWarmsWhatItCouldReadDespiteAnError(t *testing.T) {
	client := fake.NewClientset()
	ctx := context.Background()
	seed := NewConfigMapStore(client, arenas("good"))
	if err := seed.Save(ctx, sampleBaseline("good")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	client.PrependReactor("list", "configmaps", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() == "bad" {
			return true, nil, apierrors.NewForbidden(
				schema.GroupResource{Resource: "configmaps"}, "", context.DeadlineExceeded)
		}
		return false, nil, nil
	})

	m := &Manager{
		baselines: map[string]Baseline{},
		Store:     NewConfigMapStore(client, arenas("good", "bad")),
	}
	n, err := m.LoadCachedBaselines(ctx)
	if err == nil {
		t.Error("the unreadable namespace must still be reported")
	}
	if n != 1 {
		t.Errorf("loaded %d, want 1", n)
	}
	if _, ok := m.Baseline("good"); !ok {
		t.Error("the readable namespace's baseline was discarded because another failed")
	}
}
