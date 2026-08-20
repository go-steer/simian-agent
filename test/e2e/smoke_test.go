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

// Package e2e holds tests that need a live cluster. They are behind the `e2e`
// build tag so `go test ./...` stays hermetic — a unit suite that silently
// needs a cluster is a unit suite nobody can run.
//
// Stand the cluster up with `make cluster`, then `make e2e`.
//
// What lives here right now is the rig's own control: assertions about the
// cluster, not about Simian. They exist because every later measurement is
// read through this environment, and an environment that quietly fails to
// deliver a fault would show up as a subject-under-test scoring badly.
package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/simian-agent/internal/kindcluster"
)

// cluster attaches to the cluster `make cluster` created. It does not create
// one: a test that stands up its own three-node cluster per run turns a
// 30-second suite into a 5-minute one, and the lifecycle is already covered by
// kindctl.
func cluster(t *testing.T) *kindcluster.Cluster {
	t.Helper()
	kubeconfig := os.Getenv("SIMIAN_E2E_KUBECONFIG")
	if kubeconfig == "" {
		root, err := os.Getwd()
		if err != nil {
			t.Fatalf("getwd: %v", err)
		}
		kubeconfig = filepath.Join(root, "..", "..", ".kube", "e2e.yaml")
	}
	if _, err := os.Stat(kubeconfig); err != nil {
		t.Fatalf("no e2e kubeconfig at %s — run `make cluster` first (%v)", kubeconfig, err)
	}
	name := os.Getenv("SIMIAN_E2E_CLUSTER")
	if name == "" {
		name = kindcluster.DefaultName
	}
	return &kindcluster.Cluster{
		Name:       name,
		Context:    kindcluster.ContextPrefix + name,
		Kubeconfig: kubeconfig,
	}
}

func TestClusterIsReady(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c := cluster(t)

	if err := c.WaitForNodes(ctx, 2*time.Minute); err != nil {
		t.Fatalf("nodes not Ready: %v", err)
	}
	nodes, err := c.Nodes(ctx)
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	// Two workers is a shape the node-level scenarios depend on (#58); catching
	// a drifted cluster.yaml here is cheaper than debugging why a node fault
	// took the whole cluster down.
	if len(nodes) != 3 {
		t.Errorf("cluster has %d nodes (%v), want 3 — see dev/kind/cluster.yaml", len(nodes), nodes)
	}
}

func TestChaosMeshIsInstalled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c := cluster(t)

	if err := c.VerifyChaosMesh(ctx); err != nil {
		t.Fatalf("chaos-mesh not usable: %v", err)
	}
}

// TestCNIEnforcesNetworkPolicy is the assertion that makes the CNI choice in
// dev/kind/cluster.yaml real rather than aspirational.
//
// Simian's network-policy engine causes partitions by creating NetworkPolicy
// objects. kind's default CNI accepts those objects and ignores them, so the
// engine would report success and block nothing — a fault that never lands,
// with no error anywhere to say so. That is the single worst failure mode for
// an eval rig, because it is scored as the subject missing an incident that
// did not actually happen.
//
// So: prove connectivity, then deny it, and require the deny to take.
func TestCNIEnforcesNetworkPolicy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	c := cluster(t)

	const ns = "simian-e2e-netpol"
	t.Cleanup(func() {
		// Fresh context: the test's may already be cancelled, and a leaked
		// deny-all policy would break the next run in a confusing way.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, _ = c.Kubectl(cleanupCtx, "delete", "namespace", ns, "--ignore-not-found", "--wait=false")
	})
	if _, err := c.Kubectl(ctx, "delete", "namespace", ns, "--ignore-not-found"); err != nil {
		t.Fatalf("pre-clean namespace: %v", err)
	}
	if _, err := c.Apply(ctx, fixture(ns)); err != nil {
		t.Fatalf("apply fixture: %v", err)
	}
	if _, err := c.Kubectl(ctx, "-n", ns, "wait", "--for=condition=Ready",
		"pod/server", "pod/client", "--timeout=180s"); err != nil {
		t.Fatalf("fixture pods not Ready: %v", err)
	}

	ip, err := c.Kubectl(ctx, "-n", ns, "get", "pod", "server",
		"-o", "jsonpath={.status.podIP}")
	if err != nil || strings.TrimSpace(ip) == "" {
		t.Fatalf("server pod IP: %q %v", ip, err)
	}
	ip = strings.TrimSpace(ip)

	// Baseline. If this fails the CNI is broken outright and the deny result
	// below would be meaningless — a partition you cannot distinguish from a
	// cluster that never had connectivity proves nothing.
	if !reachable(ctx, t, c, ns, ip) {
		t.Fatalf("server unreachable before any NetworkPolicy — the CNI is not passing traffic")
	}

	if _, err := c.Apply(ctx, denyAll(ns)); err != nil {
		t.Fatalf("apply deny-all: %v", err)
	}

	// Policy programming is not instant; poll rather than sleeping a guess.
	deadline := time.Now().Add(90 * time.Second)
	for {
		if !reachable(ctx, t, c, ns, ip) {
			return // denied — Calico is enforcing.
		}
		if time.Now().After(deadline) {
			t.Fatal("server still reachable 90s after a deny-all NetworkPolicy — " +
				"the CNI accepts NetworkPolicy but does not enforce it. " +
				"Simian's network-policy engine would silently no-op on this cluster; " +
				"check disableDefaultCNI and the Calico install (dev/kind/cluster.yaml).")
		}
		time.Sleep(3 * time.Second)
	}
}

// reachable reports whether the client pod can fetch from the server.
// A non-zero exit is the signal we want, so the error is deliberately not
// surfaced as a test failure here.
func reachable(ctx context.Context, t *testing.T, c *kindcluster.Cluster, ns, ip string) bool {
	t.Helper()
	out, err := c.Kubectl(ctx, "-n", ns, "exec", "client", "--",
		"wget", "-T", "3", "-q", "-O-", "http://"+ip+":8080/")
	return err == nil && strings.Contains(out, "ok")
}

func fixture(ns string) string {
	return `
apiVersion: v1
kind: Namespace
metadata:
  name: ` + ns + `
---
apiVersion: v1
kind: Pod
metadata:
  name: server
  namespace: ` + ns + `
  labels: {app: server}
spec:
  containers:
    - name: httpd
      image: busybox:1.36
      command: ["sh", "-c", "mkdir -p /www && echo ok > /www/index.html && httpd -f -p 8080 -h /www"]
      ports:
        - containerPort: 8080
      readinessProbe:
        tcpSocket: {port: 8080}
        initialDelaySeconds: 1
---
apiVersion: v1
kind: Pod
metadata:
  name: client
  namespace: ` + ns + `
  labels: {app: client}
spec:
  containers:
    - name: shell
      image: busybox:1.36
      command: ["sleep", "3600"]
`
}

func denyAll(ns string) string {
	return `
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: deny-all-ingress
  namespace: ` + ns + `
spec:
  podSelector:
    matchLabels: {app: server}
  policyTypes: [Ingress]
`
}
