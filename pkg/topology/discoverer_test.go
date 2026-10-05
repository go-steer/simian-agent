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
	"reflect"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
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

func TestRunShutsTheInformersDownWhenItsContextEnds(t *testing.T) {
	// Run used to return on ctx.Done and leave seven reflectors watching. A
	// long-lived controller exits shortly afterwards so it never showed; an
	// eval runner that builds a Discoverer per scenario leaks a set each time.
	//
	// Both exit paths are covered, because the early one leaked too: a Run
	// whose cache sync is cancelled has already called factory.Start.
	tests := []struct {
		name        string
		waitForSync bool
	}{
		{"cancelled after the caches sync", true},
		{"cancelled while the caches are still syncing", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := New(fake.NewSimpleClientset(), time.Millisecond)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- d.Run(ctx) }()

			if tt.waitForSync && !d.WaitForSync(context.Background()) {
				t.Fatal("caches never synced")
			}
			cancel()
			<-done

			select {
			case <-d.stopCh:
			default:
				t.Fatal("Run returned with the informer stop channel still open")
			}
		})
	}
}

func TestStopIsIdempotentSoRunCanBeBelledTwice(t *testing.T) {
	// Run defers Stop, and callers wired before that change still call Stop
	// themselves. Closing a closed channel would panic.
	d := New(fake.NewSimpleClientset(), time.Millisecond)
	d.Stop()
	d.Stop()
}

// The planner needs to know which containers IOChaos and DNSChaos cannot
// inject into, and where IOChaos can, or it picks them anyway (#163).
func TestContainerSummaryRecordsTheRootFilesystemAndMounts(t *testing.T) {
	readOnly := true
	got := containerSummary(corev1.Container{
		Name:            "redis",
		SecurityContext: &corev1.SecurityContext{ReadOnlyRootFilesystem: &readOnly},
		VolumeMounts:    []corev1.VolumeMount{{Name: "data", MountPath: "/data"}},
	})
	if !got.ReadOnlyRootFS || len(got.MountPaths) != 1 || got.MountPaths[0] != "/data" {
		t.Errorf("summary = %+v, want read-only root and /data mounted", got)
	}
	if plain := containerSummary(corev1.Container{Name: "x"}); plain.ReadOnlyRootFS || len(plain.MountPaths) != 0 {
		t.Errorf("summary of a plain container = %+v", plain)
	}
}

// The planner is shown which probes restart a container, numbered, so it can
// keep an HTTPChaos off them; readiness restarts nothing and is left out.
func TestContainerSummaryRecordsTheProbesThatRestartIt(t *testing.T) {
	get := func(port intstr.IntOrString, path string) *corev1.Probe {
		return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Port: port, Path: path}}}
	}
	got := containerSummary(corev1.Container{
		Name:           "front",
		Ports:          []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}},
		LivenessProbe:  get(intstr.FromString("http"), "/ready"),
		StartupProbe:   get(intstr.FromInt(9090), "/started"),
		ReadinessProbe: get(intstr.FromInt(8080), "/ready"),
	})
	want := []HTTPProbe{{Port: 8080, Path: "/ready"}, {Port: 9090, Path: "/started"}}
	if !reflect.DeepEqual(got.RestartProbes, want) {
		t.Errorf("RestartProbes = %+v, want %+v", got.RestartProbes, want)
	}
	tcp := containerSummary(corev1.Container{LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(5432)}}}})
	if len(tcp.RestartProbes) != 0 {
		t.Errorf("a TCP probe was recorded: %+v", tcp.RestartProbes)
	}
}

func TestContainerSummaryRecordsGRPCPorts(t *testing.T) {
	grpcProbe := &corev1.Probe{ProbeHandler: corev1.ProbeHandler{GRPC: &corev1.GRPCAction{Port: 50051}}}
	got := containerSummary(corev1.Container{
		Name:           "server",
		Ports:          []corev1.ContainerPort{{ContainerPort: 50051}, {Name: "grpc-admin", ContainerPort: 9000}, {Name: "http", ContainerPort: 8080}},
		ReadinessProbe: grpcProbe,
		LivenessProbe:  grpcProbe,
	})
	if want := []int32{9000, 50051}; !reflect.DeepEqual(got.GRPCPorts, want) {
		t.Errorf("GRPCPorts = %v, want %v", got.GRPCPorts, want)
	}
}
