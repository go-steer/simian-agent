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

//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/simian-agent/internal/testutil"
	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/catalog"
	"github.com/go-steer/simian-agent/pkg/driver/chaosmesh"
	"github.com/go-steer/simian-agent/pkg/executor"
	"github.com/go-steer/simian-agent/pkg/lease"
	"github.com/go-steer/simian-agent/pkg/simian"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// outageFixture is two Deployments spread evenly over the kind workers'
// zones (dev/kind/cluster.yaml labels them zone-a and zone-b). The readiness
// probe execs a shell, which Chaos Mesh's pod-failure — it swaps the image for
// pause — takes away, so a failed pod goes NotReady. loadgen is excluded.
func outageFixture(ns string) string {
	deploy := func(name string) string {
		return fmt.Sprintf(`---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  replicas: 4
  selector:
    matchLabels: {app: %[1]s}
  template:
    metadata:
      labels: {app: %[1]s}
    spec:
      terminationGracePeriodSeconds: 1
      topologySpreadConstraints:
        - maxSkew: 1
          topologyKey: topology.kubernetes.io/zone
          whenUnsatisfiable: DoNotSchedule
          labelSelector:
            matchLabels: {app: %[1]s}
      containers:
        - name: main
          image: busybox:1.36
          command: ["sh", "-c", "touch /tmp/ready && sleep 3600"]
          readinessProbe:
            exec: {command: ["test", "-f", "/tmp/ready"]}
            periodSeconds: 2
            failureThreshold: 1
`, name, ns)
	}
	return fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %s
  annotations:
    simian.chaos/eligible: "true"
    simian.chaos/exclude-workloads: loadgen
`, ns) + deploy("web") + deploy("loadgen")
}

// TestZoneOutageTakesDownExactlyTheZonesArenaPods runs ZoneOutage through
// the executor and the real driver against Chaos Mesh: zone-a's web pods go
// NotReady and zone-b's do not, the excluded workload is untouched, a
// restarted controller finds the outage as one fault, and Clear brings the
// pods back.
func TestZoneOutageTakesDownExactlyTheZonesArenaPods(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	c := cluster(t)

	const ns = "simian-e2e-outage"
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, _ = c.Kubectl(cleanupCtx, "delete", "namespace", ns, "--ignore-not-found", "--wait=false")
	})
	if _, err := c.Kubectl(ctx, "delete", "namespace", ns, "--ignore-not-found"); err != nil {
		t.Fatalf("pre-clean namespace: %v", err)
	}
	if _, err := c.Apply(ctx, outageFixture(ns)); err != nil {
		t.Fatalf("apply fixture: %v", err)
	}
	for _, d := range []string{"web", "loadgen"} {
		if _, err := c.Kubectl(ctx, "-n", ns, "rollout", "status", "deployment/"+d, "--timeout=180s"); err != nil {
			t.Fatalf("%s not ready: %v", d, err)
		}
	}

	cfg := restConfigFor(t, c.Kubeconfig, c.Context)
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	zoneOf := nodeZones(ctx, t, kube)
	before := podsByZone(ctx, t, kube, ns, zoneOf)
	if len(before["web"]["zone-a"]) == 0 || len(before["web"]["zone-b"]) == 0 {
		t.Fatalf("web is not spread over both zones: %v", before["web"])
	}

	driver := driverFor(t, c.Kubeconfig, c.Context).WithKubernetes(kube).WithConfirmTimeout(90 * time.Second)
	auditor := &testutil.FakeAuditor{}
	registry := lease.NewRegistry("e2e")
	exec := executor.New(executor.DefaultConfig(),
		map[simian.Engine]simian.ChaosDriver{simian.EngineChaosMesh: driver},
		registry, auditor,
		&executor.StaticEligibility{Eligible: map[string]bool{ns: true}, Exclusions: map[string][]string{ns: {"loadgen"}}})

	uid, err := exec.Apply(ctx, simian.FaultManifest{
		Source: simian.SourceDirected, Engine: simian.EngineChaosMesh, APIVersion: chaosmesh.APIGroup + "/v1alpha1",
		ResourceKind: catalog.ChaosMeshZoneOutage, Spec: map[string]any{"zone": "zone-a"},
		Targets: []simian.TargetRef{{Namespace: ns}}, Duration: 4 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Apply: %v\naudit: %v", err, auditor.EventNames())
	}
	cleared := false
	t.Cleanup(func() {
		if !cleared {
			_ = exec.Clear(context.Background(), uid)
		}
	})

	applied, _ := auditor.FindEvent(audit.EventDriverApplied)
	detail, _ := applied.Payload["outage"].(map[string]any)
	if detail == nil || detail["zone"] != "zone-a" || detail["excluded_skipped"] == nil {
		t.Errorf("driver.applied outage detail = %v", applied.Payload["outage"])
	}
	t.Logf("driver.applied outage: %v", detail)

	// zone-a's web pods go NotReady; zone-b's, and every loadgen pod, stay up.
	waitFor(ctx, t, "zone-a's web pods NotReady", func() (bool, string) {
		now := podsByZone(ctx, t, kube, ns, zoneOf)
		return allReady(now["web"]["zone-a"], false) && allReady(now["web"]["zone-b"], true) &&
			allReady(now["loadgen"]["zone-a"], true) && allReady(now["loadgen"]["zone-b"], true), fmt.Sprint(now)
	})
	// And they stay that way: the outage is not a one-off kill.
	time.Sleep(10 * time.Second)
	now := podsByZone(ctx, t, kube, ns, zoneOf)
	if !allReady(now["web"]["zone-a"], false) || !allReady(now["web"]["zone-b"], true) || !allReady(now["loadgen"]["zone-a"], true) {
		t.Errorf("10s later: %v", now)
	}

	// A controller restarted now adopts the outage once, as a ZoneOutage.
	restarted := driverFor(t, c.Kubeconfig, c.Context).WithKubernetes(kube)
	fresh := lease.NewRegistry("e2e-restarted")
	if n := lease.AdoptLive(ctx, fresh, map[simian.Engine]simian.ChaosDriver{simian.EngineChaosMesh: restarted},
		[]string{ns}, nil, time.Now()); n != 1 {
		t.Errorf("a restarted controller adopted %d faults, want 1: %+v", n, fresh.List(""))
	}
	if live := fresh.List(""); len(live) == 1 {
		if live[0].FaultUID != uid || live[0].Manifest.ResourceKind != catalog.ChaosMeshZoneOutage {
			t.Errorf("adopted %s as %s, want %s as ZoneOutage", live[0].FaultUID, live[0].Manifest.ResourceKind, uid)
		}
	}

	if err := exec.Clear(ctx, uid); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	cleared = true
	if out, _ := c.Kubectl(ctx, "-n", ns, "get", "podchaos", "-l", chaosmesh.BundleLabel, "-o", "name"); strings.TrimSpace(out) != "" {
		// Deletion waits on Chaos Mesh's finalizer; give it a moment.
		waitFor(ctx, t, "the outage's PodChaos gone", func() (bool, string) {
			out, _ := c.Kubectl(ctx, "-n", ns, "get", "podchaos", "-l", chaosmesh.BundleLabel, "-o", "name")
			return strings.TrimSpace(out) == "", out
		})
	}
	waitFor(ctx, t, "every pod Ready again after Clear", func() (bool, string) {
		now := podsByZone(ctx, t, kube, ns, zoneOf)
		for _, byZone := range now {
			for _, pods := range byZone {
				if !allReady(pods, true) {
					return false, fmt.Sprint(now)
				}
			}
		}
		return true, ""
	})
}

// podReady is a pod's readiness by name.
type podReady map[string]bool

func allReady(pods podReady, want bool) bool {
	if len(pods) == 0 {
		return false
	}
	for _, r := range pods {
		if r != want {
			return false
		}
	}
	return true
}

func nodeZones(ctx context.Context, t *testing.T, kube kubernetes.Interface) map[string]string {
	t.Helper()
	nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	out := map[string]string{}
	var zones []string
	for _, n := range nodes.Items {
		if z := n.Labels[catalog.ZoneLabel]; z != "" {
			out[n.Name] = z
			zones = append(zones, z)
		}
	}
	sort.Strings(zones)
	if strings.Join(zones, ",") != "zone-a,zone-b" {
		t.Fatalf("node zones = %v; the cluster predates the zone labels in dev/kind/cluster.yaml — recreate it with `make cluster-down cluster`", out)
	}
	return out
}

// podsByZone returns app -> zone -> pod -> ready.
func podsByZone(ctx context.Context, t *testing.T, kube kubernetes.Interface, ns string, zoneOf map[string]string) map[string]map[string]podReady {
	t.Helper()
	pods, err := kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	out := map[string]map[string]podReady{}
	for _, p := range pods.Items {
		if p.DeletionTimestamp != nil {
			continue
		}
		app := p.Labels["app"]
		if out[app] == nil {
			out[app] = map[string]podReady{}
		}
		zone := zoneOf[p.Spec.NodeName]
		if out[app][zone] == nil {
			out[app][zone] = podReady{}
		}
		ready := false
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady {
				ready = c.Status == corev1.ConditionTrue
			}
		}
		out[app][zone][p.Name] = ready
	}
	return out
}

func waitFor(ctx context.Context, t *testing.T, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	last := ""
	for time.Now().Before(deadline) && ctx.Err() == nil {
		ok, state := cond()
		if ok {
			return
		}
		last = state
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("timed out waiting for %s; last: %s", what, last)
}
