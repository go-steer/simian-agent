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

package topology

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func intptr(i int32) *int32 { return &i }

func TestSnapshot_PopulatesWorkloadsServicesAndEdges(t *testing.T) {
	ns := "boutique"
	objs := []any{
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "frontend", Namespace: ns},
			Spec: appsv1.DeploymentSpec{
				Replicas: intptr(2),
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "frontend"}},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{
						Name:  "server",
						Image: "frontend:1.0",
						Env: []corev1.EnvVar{
							{Name: "CART_SERVICE_ADDR", Value: "cartservice:7070"},
						},
					}}},
				},
			},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "cartservice", Namespace: ns},
			Spec: appsv1.DeploymentSpec{
				Replicas: intptr(1),
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "cartservice"}},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{
						Name: "server", Image: "cartservice:1.0",
					}}},
				},
			},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "frontend", Namespace: ns},
			Spec: corev1.ServiceSpec{
				Selector: map[string]string{"app": "frontend"},
				Ports:    []corev1.ServicePort{{Name: "http", Port: 80}},
			},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "cartservice", Namespace: ns},
			Spec: corev1.ServiceSpec{
				Selector: map[string]string{"app": "cartservice"},
				Ports:    []corev1.ServicePort{{Name: "grpc", Port: 7070}},
			},
		},
		&netv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "allow-frontend-to-cart", Namespace: ns},
			Spec: netv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "cartservice"}},
				Ingress: []netv1.NetworkPolicyIngressRule{{
					From: []netv1.NetworkPolicyPeer{{
						PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "frontend"}},
					}},
				}},
			},
		},
	}

	client := fake.NewClientset(toRuntimeObjects(objs)...)
	d := New(client, 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d.Start()
	if !d.WaitForSync(ctx) {
		t.Fatal("WaitForSync returned false")
	}
	defer d.Stop()

	snap, err := d.Snapshot(ctx, ns)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if snap.Namespace != ns {
		t.Errorf("Namespace = %q, want %q", snap.Namespace, ns)
	}
	if len(snap.Workloads) != 2 {
		t.Errorf("Workloads count = %d, want 2", len(snap.Workloads))
	}
	if len(snap.Services) != 2 {
		t.Errorf("Services count = %d, want 2", len(snap.Services))
	}
	if got := snap.ReplicaMap["frontend"]; got != 2 {
		t.Errorf("ReplicaMap[frontend] = %d, want 2", got)
	}

	// Both edge sources should resolve frontend → cartservice.
	if got := snap.DependencyGraph["frontend"]; len(got) != 1 || got[0] != "cartservice" {
		t.Errorf("DependencyGraph[frontend] = %v, want [cartservice]", got)
	}
	prov := snap.EdgeProvenance["frontend->cartservice"]
	if !contains(prov, "networkpolicy") || !contains(prov, "envvar") {
		t.Errorf("EdgeProvenance[frontend->cartservice] = %v, want both networkpolicy and envvar", prov)
	}
}

func TestSnapshot_EmptyNamespaceErrors(t *testing.T) {
	d := New(fake.NewClientset(), 0)
	if _, err := d.Snapshot(context.Background(), ""); err == nil {
		t.Fatal("expected error on empty namespace")
	}
}

func TestSnapshot_NoWorkloadsReturnsEmpty(t *testing.T) {
	client := fake.NewClientset()
	d := New(client, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	d.Start()
	d.WaitForSync(ctx)
	defer d.Stop()
	snap, err := d.Snapshot(ctx, "empty")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.Workloads) != 0 || len(snap.Services) != 0 {
		t.Errorf("expected empty snapshot, got %+v", snap)
	}
	if snap.DependencyGraph == nil || snap.PodStatus == nil {
		t.Errorf("expected initialized maps, got nil")
	}
}

func TestSnapshot_PodGroupedByDeployment(t *testing.T) {
	ns := "boutique"
	objs := []any{
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "frontend", Namespace: ns},
			Spec: appsv1.DeploymentSpec{
				Replicas: intptr(1),
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "frontend"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "x"}}},
				},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "frontend-7d9f-abc12",
				Namespace: ns,
				OwnerReferences: []metav1.OwnerReference{
					{Kind: "ReplicaSet", Name: "frontend-7d9f"},
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				Conditions: []corev1.PodCondition{
					{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				},
			},
		},
	}
	client := fake.NewClientset(toRuntimeObjects(objs)...)
	d := New(client, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	d.Start()
	d.WaitForSync(ctx)
	defer d.Stop()
	snap, err := d.Snapshot(ctx, ns)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	pods := snap.PodStatus["frontend"]
	if len(pods) != 1 || !pods[0].Ready {
		t.Errorf("expected one ready pod under 'frontend', got %+v", snap.PodStatus)
	}
}

func TestSnapshot_FlagsEnvoyInjectedFromAnnotation(t *testing.T) {
	ns := "boutique"
	objs := []any{
		// Deployment with the annotation → EnvoyInjected=true.
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "frontend", Namespace: ns},
			Spec: appsv1.DeploymentSpec{
				Replicas: intptr(1),
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels:      map[string]string{"app": "frontend"},
						Annotations: map[string]string{"simian.chaos/envoy-injected": "true"},
					},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "x"}}},
				},
			},
		},
		// Deployment without the annotation → EnvoyInjected=false.
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "loadgenerator", Namespace: ns},
			Spec: appsv1.DeploymentSpec{
				Replicas: intptr(1),
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "loadgenerator"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "x"}}},
				},
			},
		},
	}
	client := fake.NewClientset(toRuntimeObjects(objs)...)
	d := New(client, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	d.Start()
	d.WaitForSync(ctx)
	defer d.Stop()
	snap, err := d.Snapshot(ctx, ns)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	flags := map[string]bool{}
	for _, w := range snap.Workloads {
		flags[w.Name] = w.EnvoyInjected
	}
	if !flags["frontend"] {
		t.Error("frontend should be EnvoyInjected=true (has the annotation)")
	}
	if flags["loadgenerator"] {
		t.Error("loadgenerator should be EnvoyInjected=false (no annotation)")
	}
}

func toRuntimeObjects(objs []any) []runtime.Object {
	out := make([]runtime.Object, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.(runtime.Object))
	}
	return out
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
