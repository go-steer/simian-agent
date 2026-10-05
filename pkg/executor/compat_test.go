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

package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/go-steer/simian-agent/internal/testutil"
	"github.com/go-steer/simian-agent/pkg/lease"
	"github.com/go-steer/simian-agent/pkg/simian"
)

func compatPod(name string, readOnly bool, mounts ...string) *corev1.Pod {
	c := corev1.Container{Name: "server", SecurityContext: &corev1.SecurityContext{ReadOnlyRootFilesystem: &readOnly}}
	for _, m := range mounts {
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "v", MountPath: m})
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "online-boutique", Labels: map[string]string{"app": name}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{c}},
	}
}

func newCompatExecutor(t *testing.T, pods ...*corev1.Pod) (*Executor, *testutil.FakeDriver) {
	t.Helper()
	cs := fake.NewClientset()
	for _, p := range pods {
		if _, err := cs.CoreV1().Pods(p.Namespace).Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	driver := &testutil.FakeDriver{EngineName: simian.EngineChaosMesh}
	elig := &StaticEligibility{Eligible: map[string]bool{"online-boutique": true}}
	exec := New(DefaultConfig(), map[simian.Engine]simian.ChaosDriver{simian.EngineChaosMesh: driver},
		lease.NewRegistry("test-holder"), &testutil.FakeAuditor{}, elig,
		WithTargetPods(KubernetesTargetPods{Client: cs}))
	return exec, driver
}

func compatManifest(kind, app string, spec map[string]any) simian.FaultManifest {
	return simian.FaultManifest{
		Source:       simian.SourceAutonomous,
		Engine:       simian.EngineChaosMesh,
		APIVersion:   "chaos-mesh.org/v1alpha1",
		ResourceKind: kind,
		Spec:         spec,
		Targets:      []simian.TargetRef{{Namespace: "online-boutique", Name: app, Labels: map[string]string{"app": app}}},
		Duration:     time.Minute,
	}
}

// Every IOChaos against Online Boutique on the 2026-09-30 trial failed with
// "Read-only file system (os error 30)", and so did a DNSChaos copying
// /etc/resolv.conf aside (#163). The pods said so before anything was applied.
func TestFaultsThatWriteTheRootFilesystemAreRefusedOnAReadOnlyOne(t *testing.T) {
	for _, tc := range []struct {
		kind string
		spec map[string]any
	}{
		{"IOChaos", map[string]any{"action": "latency", "volumePath": "/data", "delay": "100ms"}},
		{"DNSChaos", map[string]any{"action": "error", "patterns": []any{"cartservice.online-boutique.svc.cluster.local"}}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			exec, driver := newCompatExecutor(t, compatPod("redis-cart", true, "/data"))
			_, err := exec.Apply(context.Background(), compatManifest(tc.kind, "redis-cart", tc.spec))
			var ee *simian.ExecutorError
			if !errors.As(err, &ee) || ee.Reason != simian.ReasonTargetIncompatible {
				t.Fatalf("err = %v, want reason %q", err, simian.ReasonTargetIncompatible)
			}
			if !strings.Contains(err.Error(), `container "server" has a read-only root filesystem`) {
				t.Errorf("err = %v, want it to name the container and why", err)
			}
			if n := len(driver.AppliedCopy()); n != 0 {
				t.Errorf("driver applied %d, want 0", n)
			}
		})
	}
}

// ledger-db-0 on the same trial: "No such file or directory (os error 2)",
// because the IOChaos volumePath was not one of its mounts.
func TestAnIOChaosVolumePathMustBeAMountPoint(t *testing.T) {
	exec, driver := newCompatExecutor(t, compatPod("ledger-db", false, "/var/lib/postgresql/data"))
	_, err := exec.Apply(context.Background(), compatManifest("IOChaos", "ledger-db",
		map[string]any{"action": "latency", "volumePath": "/var/lib/postgresql", "delay": "100ms"}))
	var ee *simian.ExecutorError
	if !errors.As(err, &ee) || ee.Reason != simian.ReasonTargetIncompatible || !strings.Contains(err.Error(), "/var/lib/postgresql/data") {
		t.Fatalf("err = %v, want target-incompatible listing the real mounts", err)
	}
	if n := len(driver.AppliedCopy()); n != 0 {
		t.Errorf("driver applied %d, want 0", n)
	}

	exec, driver = newCompatExecutor(t, compatPod("ledger-db", false, "/var/lib/postgresql/data"))
	if _, err := exec.Apply(context.Background(), compatManifest("IOChaos", "ledger-db",
		map[string]any{"action": "latency", "volumePath": "/var/lib/postgresql/data/", "delay": "100ms"})); err != nil {
		t.Fatalf("Apply on the mount point: %v", err)
	}
	if n := len(driver.AppliedCopy()); n != 1 {
		t.Errorf("driver applied %d, want 1", n)
	}
}

func TestOnlyTheNamedContainersAndKindsAreJudged(t *testing.T) {
	pod := compatPod("frontend", true)
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: "sidecar", VolumeMounts: []corev1.VolumeMount{{Name: "v", MountPath: "/cache"}}})

	exec, _ := newCompatExecutor(t, pod)
	if _, err := exec.Apply(context.Background(), compatManifest("IOChaos", "frontend",
		map[string]any{"action": "latency", "volumePath": "/cache", "delay": "1s", "containerNames": []any{"sidecar"}})); err != nil {
		t.Errorf("IOChaos on the writable sidecar refused: %v", err)
	}

	exec, _ = newCompatExecutor(t, compatPod("frontend", true))
	if _, err := exec.Apply(context.Background(), compatManifest("PodChaos", "frontend",
		map[string]any{"action": "pod-kill", "mode": "one"})); err != nil {
		t.Errorf("PodChaos refused on a read-only root filesystem it never touches: %v", err)
	}
}

func probedPod(name string, liveness, readiness *corev1.Probe, ports ...corev1.ContainerPort) *corev1.Pod {
	p := compatPod(name, false)
	p.Spec.Containers[0].LivenessProbe = liveness
	p.Spec.Containers[0].ReadinessProbe = readiness
	p.Spec.Containers[0].Ports = ports
	return p
}

func httpProbe(port intstr.IntOrString, path string) *corev1.Probe {
	return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Port: port, Path: path}}}
}

// On the 2026-10-03 soak an HTTPChaos abort on GET :8080/* hit bank's
// frontend liveness probe, the kubelet restarted the container, Chaos Mesh's
// proxy died with it, and its redirect rules left port 8080 a black hole
// long after the fault ended.
func TestAnHTTPChaosThatWouldFailALivenessProbeIsRefused(t *testing.T) {
	ready := httpProbe(intstr.FromInt(8080), "/ready")
	named := httpProbe(intstr.FromString("http"), "/healthz")
	exec, driver := newCompatExecutor(t,
		probedPod("frontend", ready, ready),
		probedPod("named", named, nil, corev1.ContainerPort{Name: "http", ContainerPort: 9000}),
		probedPod("readiness-only", nil, ready),
	)
	for name, tc := range map[string]struct {
		app     string
		spec    map[string]any
		refused bool
	}{
		"abort on every path of the probe's port": {"frontend", map[string]any{"abort": true, "port": float64(8080), "path": "/*", "target": "Request"}, true},
		"no path selector at all":                 {"frontend", map[string]any{"abort": true, "port": float64(8080), "target": "Request"}, true},
		"named probe port resolved":               {"named", map[string]any{"abort": true, "port": float64(9000), "path": "/health*", "target": "Request"}, true},
		"a path the probe does not use":           {"frontend", map[string]any{"abort": true, "port": float64(8080), "path": "/api/*", "target": "Request"}, false},
		"another port":                            {"frontend", map[string]any{"abort": true, "port": float64(9090), "path": "/*", "target": "Request"}, false},
		"POST only: probes are GETs":              {"frontend", map[string]any{"abort": true, "port": float64(8080), "method": "POST", "target": "Request"}, false},
		"readiness alone restarts nothing":        {"readiness-only", map[string]any{"abort": true, "port": float64(8080), "path": "/*", "target": "Request"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			before := len(driver.Applied)
			_, err := exec.Apply(context.Background(), compatManifest("HTTPChaos", tc.app, tc.spec))
			if tc.refused {
				ee := asExecutorError(t, err)
				if ee.Reason != simian.ReasonTargetIncompatible || !strings.Contains(err.Error(), "liveness probe") {
					t.Fatalf("err = %v, want target-incompatible naming the liveness probe", err)
				}
				if len(driver.Applied) != before {
					t.Error("the driver was called for a refused fault")
				}
				return
			}
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
		})
	}
}

// The 2026-10-04 soak: an HTTPChaos abort on paymentservice's port 50051,
// which Online Boutique declares unnamed and probes with grpc. Twelve hours
// unreachable, past a check that only looked at HTTP probes.
func TestAnHTTPChaosOnAGRPCPortIsRefused(t *testing.T) {
	grpcProbe := &corev1.Probe{ProbeHandler: corev1.ProbeHandler{GRPC: &corev1.GRPCAction{Port: 50051}}}
	payment := probedPod("paymentservice", grpcProbe, grpcProbe, corev1.ContainerPort{ContainerPort: 50051})
	named := probedPod("named-grpc", nil, nil, corev1.ContainerPort{Name: "grpc-api", ContainerPort: 9000})
	exec, driver := newCompatExecutor(t, payment, named)
	for name, tc := range map[string]struct {
		app  string
		spec map[string]any
	}{
		"the soak's fault":             {"paymentservice", map[string]any{"abort": true, "mode": "all", "path": "/*", "port": float64(50051), "target": "Request"}},
		"POST, as gRPC requests are":   {"paymentservice", map[string]any{"abort": true, "port": float64(50051), "method": "POST", "target": "Request"}},
		"a port named grpc, no probes": {"named-grpc", map[string]any{"delay": "200ms", "port": float64(9000), "target": "Request"}},
	} {
		t.Run(name, func(t *testing.T) {
			before := len(driver.Applied)
			_, err := exec.Apply(context.Background(), compatManifest("HTTPChaos", tc.app, tc.spec))
			if ee := asExecutorError(t, err); ee.Reason != simian.ReasonTargetIncompatible || !strings.Contains(err.Error(), "serves gRPC") {
				t.Fatalf("err = %v, want target-incompatible naming gRPC", err)
			}
			if len(driver.Applied) != before {
				t.Error("the driver was called for a refused fault")
			}
		})
	}
	// Another port on the same pod is not gRPC.
	if _, err := exec.Apply(context.Background(), compatManifest("HTTPChaos", "paymentservice",
		map[string]any{"abort": true, "port": float64(8080), "path": "/*", "target": "Request"})); err != nil {
		t.Errorf("HTTPChaos on a non-gRPC port was refused: %v", err)
	}
}
