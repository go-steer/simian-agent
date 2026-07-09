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
	"errors"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// fakeSUT is a tiny inline SUT for unit tests.
type fakeSUT struct {
	name      string
	manifests string
	workloads []WorkloadRef
	cfg       BaselineConfig
}

func (f *fakeSUT) Name() string                     { return f.name }
func (f *fakeSUT) Description() string              { return "fake sut for tests" }
func (f *fakeSUT) Manifests() []byte                { return []byte(f.manifests) }
func (f *fakeSUT) ExpectedWorkloads() []WorkloadRef { return f.workloads }
func (f *fakeSUT) BaselineConfig() BaselineConfig   { return f.cfg }

func newDeployment(ns, name string, desired, ready int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       appsv1.DeploymentSpec{Replicas: &desired},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: ready, Replicas: desired},
	}
}

func TestSplitYAMLSeparatesMultipleDocs(t *testing.T) {
	in := []byte(`apiVersion: v1
kind: ConfigMap
metadata: {name: a}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: b}
---
`)
	docs, err := splitYAML(in)
	if err != nil {
		t.Fatalf("splitYAML: %v", err)
	}
	if got := len(docs); got != 2 {
		t.Fatalf("expected 2 docs, got %d", got)
	}
	if docs[0].GetName() != "a" || docs[1].GetName() != "b" {
		t.Errorf("doc names: %s, %s", docs[0].GetName(), docs[1].GetName())
	}
}

func TestSplitYAMLSkipsEmptyDocs(t *testing.T) {
	in := []byte(`---
---
apiVersion: v1
kind: ConfigMap
metadata: {name: only}
---
---
`)
	docs, err := splitYAML(in)
	if err != nil {
		t.Fatalf("splitYAML: %v", err)
	}
	if got := len(docs); got != 1 {
		t.Fatalf("expected 1 doc, got %d", got)
	}
}

func TestWorkloadStatusesAllReady(t *testing.T) {
	ctx := context.Background()
	k8s := fake.NewClientset(
		newDeployment("ns-a", "frontend", 2, 2),
		newDeployment("ns-a", "backend", 1, 1),
	)
	m := &Manager{K8s: k8s}
	statuses, allReady, err := m.workloadStatuses(ctx, "ns-a", []WorkloadRef{
		{Kind: "Deployment", Name: "frontend"},
		{Kind: "Deployment", Name: "backend"},
	})
	if err != nil {
		t.Fatalf("workloadStatuses: %v", err)
	}
	if !allReady {
		t.Fatalf("expected allReady=true, got statuses=%+v", statuses)
	}
	if got := len(statuses); got != 2 {
		t.Fatalf("expected 2 statuses, got %d", got)
	}
}

func TestWorkloadStatusesNotReadyWhenReplicasShort(t *testing.T) {
	ctx := context.Background()
	k8s := fake.NewClientset(
		newDeployment("ns-a", "frontend", 2, 1), // 1/2 ready
	)
	m := &Manager{K8s: k8s}
	_, allReady, err := m.workloadStatuses(ctx, "ns-a", []WorkloadRef{
		{Kind: "Deployment", Name: "frontend"},
	})
	if err != nil {
		t.Fatalf("workloadStatuses: %v", err)
	}
	if allReady {
		t.Error("expected allReady=false")
	}
}

func TestWorkloadStatusesNotReadyWhenWorkloadMissing(t *testing.T) {
	ctx := context.Background()
	k8s := fake.NewClientset()
	m := &Manager{K8s: k8s}
	statuses, allReady, err := m.workloadStatuses(ctx, "ns-a", []WorkloadRef{
		{Kind: "Deployment", Name: "missing"},
	})
	if err != nil {
		t.Fatalf("workloadStatuses: %v", err)
	}
	if allReady {
		t.Error("expected allReady=false for missing deployment")
	}
	if len(statuses) != 1 || statuses[0].Name != "missing" {
		t.Errorf("statuses=%+v", statuses)
	}
}

func TestWaitForBaselineSucceedsImmediatelyWhenReady(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	k8s := fake.NewClientset(
		newDeployment("ns-a", "frontend", 1, 1),
	)
	m := &Manager{K8s: k8s}
	s := &fakeSUT{
		name:      "test",
		workloads: []WorkloadRef{{Kind: "Deployment", Name: "frontend"}},
		cfg: BaselineConfig{
			ReadyTimeout:    2 * time.Second,
			StabilityWindow: 0,
			PollInterval:    50 * time.Millisecond,
		},
	}
	bl, err := m.waitForBaseline(ctx, "ns-a", s, s.cfg)
	if err != nil {
		t.Fatalf("waitForBaseline: %v", err)
	}
	if bl == nil {
		t.Fatal("expected baseline, got nil")
	}
	if bl.SUT != "test" || bl.Namespace != "ns-a" {
		t.Errorf("baseline metadata wrong: %+v", bl)
	}
	if len(bl.Workloads) != 1 || bl.Workloads[0].ReadyReplicas != 1 {
		t.Errorf("baseline workloads=%+v", bl.Workloads)
	}
}

func TestWaitForBaselineTimesOutWhenNotReady(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	k8s := fake.NewClientset(
		newDeployment("ns-a", "frontend", 2, 1), // perpetually 1/2
	)
	m := &Manager{K8s: k8s}
	s := &fakeSUT{
		name:      "test",
		workloads: []WorkloadRef{{Kind: "Deployment", Name: "frontend"}},
		cfg: BaselineConfig{
			ReadyTimeout:    300 * time.Millisecond,
			StabilityWindow: 0,
			PollInterval:    50 * time.Millisecond,
		},
	}
	_, err := m.waitForBaseline(ctx, "ns-a", s, s.cfg)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	var bte *BaselineTimeoutError
	if !errors.As(err, &bte) {
		t.Fatalf("expected *BaselineTimeoutError, got %T: %v", err, err)
	}
	if len(bte.Statuses) != 1 || bte.Statuses[0].ReadyReplicas != 1 {
		t.Errorf("timeout error statuses=%+v", bte.Statuses)
	}
}

func newStatefulSet(ns, name string, desired, ready int32) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       appsv1.StatefulSetSpec{Replicas: &desired},
		Status:     appsv1.StatefulSetStatus{ReadyReplicas: ready, Replicas: desired},
	}
}

func TestListNamespaceWorkloadsEnumeratesBothKinds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	k8s := fake.NewClientset(
		newDeployment("payments", "api", 1, 1),
		newDeployment("payments", "worker", 2, 2),
		newDeployment("other-ns", "should-not-appear", 1, 1),
		newStatefulSet("payments", "queue", 3, 3),
	)
	m := &Manager{K8s: k8s, Store: noopStore{}, baselines: map[string]Baseline{}}
	refs, err := m.listNamespaceWorkloads(ctx, "payments")
	if err != nil {
		t.Fatalf("listNamespaceWorkloads: %v", err)
	}
	// Expect Deployment/api, Deployment/worker, StatefulSet/queue —
	// sorted by (Kind,Name).
	want := []WorkloadRef{
		{Kind: "Deployment", Name: "api"},
		{Kind: "Deployment", Name: "worker"},
		{Kind: "StatefulSet", Name: "queue"},
	}
	if len(refs) != len(want) {
		t.Fatalf("expected %d workloads, got %d: %+v", len(want), len(refs), refs)
	}
	for i := range want {
		if refs[i] != want[i] {
			t.Errorf("refs[%d] = %+v, want %+v", i, refs[i], want[i])
		}
	}
}

func TestEstablishBaselineFromTopologyCapturesWorkloads(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	k8s := fake.NewClientset(
		newDeployment("payments", "api", 1, 1),
		newStatefulSet("payments", "queue", 3, 3),
	)
	m := &Manager{K8s: k8s, Store: noopStore{}, baselines: map[string]Baseline{}}
	bl, err := m.EstablishBaselineFromTopology(ctx, "payments", BaselineConfig{
		ReadyTimeout:    2 * time.Second,
		StabilityWindow: 0,
		PollInterval:    50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("EstablishBaselineFromTopology: %v", err)
	}
	if bl.SUT != "" {
		t.Errorf("topology-derived baseline should have SUT=\"\", got %q", bl.SUT)
	}
	if bl.Namespace != "payments" {
		t.Errorf("baseline namespace = %q, want \"payments\"", bl.Namespace)
	}
	if len(bl.Workloads) != 2 {
		t.Fatalf("expected 2 workloads in baseline, got %d: %+v", len(bl.Workloads), bl.Workloads)
	}
	// Cached in-memory + retrievable via Baseline().
	cached, ok := m.Baseline("payments")
	if !ok {
		t.Fatal("baseline should have been cached")
	}
	if cached.SUT != "" || len(cached.Workloads) != 2 {
		t.Errorf("cached baseline mismatch: %+v", cached)
	}
}

func TestEstablishBaselineFromTopologyFailsWhenNamespaceEmpty(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	k8s := fake.NewClientset() // no workloads
	m := &Manager{K8s: k8s, Store: noopStore{}, baselines: map[string]Baseline{}}
	_, err := m.EstablishBaselineFromTopology(ctx, "empty-ns", DefaultBaselineConfig())
	if err == nil {
		t.Fatal("expected error when namespace has no Deployments or StatefulSets")
	}
}

func TestRegistryRoundTrip(t *testing.T) {
	r := NewMemoryRegistry()
	if err := r.Register(&fakeSUT{name: "a"}); err != nil {
		t.Fatalf("Register a: %v", err)
	}
	if err := r.Register(&fakeSUT{name: "b"}); err != nil {
		t.Fatalf("Register b: %v", err)
	}
	if err := r.Register(&fakeSUT{name: "a"}); err == nil {
		t.Error("expected duplicate register to error")
	}
	if _, ok := r.Get("a"); !ok {
		t.Error("Get(a) missed")
	}
	if _, ok := r.Get("nope"); ok {
		t.Error("Get(nope) should miss")
	}
	list := r.List()
	if got := len(list); got != 2 {
		t.Fatalf("List length=%d, want 2", got)
	}
	if list[0].Name() != "a" || list[1].Name() != "b" {
		t.Errorf("list order: %v", []string{list[0].Name(), list[1].Name()})
	}
}
