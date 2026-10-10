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
	"strings"
	"testing"
	"time"

	"github.com/go-steer/simian-agent/pkg/driver/chaosmesh"
	"github.com/go-steer/simian-agent/pkg/simian"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// TestChaosMeshDriverKnowsWhenAFaultDidNotTake runs the real driver against
// the real controller. The unit tests read status shapes copied from this
// cluster; this is what keeps those copies honest when Chaos Mesh changes.
//
// The failing case is an IOChaos on a volume the pod does not have: Chaos Mesh
// accepts it, retries the injection every second, and never succeeds — the
// same shape as the DNSChaos that failed on every pod in the GKE trial and was
// recorded as having run.
func TestChaosMeshDriverKnowsWhenAFaultDidNotTake(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c := cluster(t)

	const ns = "simian-e2e-confirm"
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, _ = c.Kubectl(cleanupCtx, "delete", "namespace", ns, "--ignore-not-found", "--wait=false")
	})
	if _, err := c.Kubectl(ctx, "delete", "namespace", ns, "--ignore-not-found"); err != nil {
		t.Fatalf("pre-clean namespace: %v", err)
	}
	if _, err := c.Kubectl(ctx, "create", "namespace", ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	if _, err := c.Kubectl(ctx, "-n", ns, "create", "deployment", "web", "--image=busybox:1.36",
		"--replicas=2", "--", "sleep", "3600"); err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	if _, err := c.Kubectl(ctx, "-n", ns, "rollout", "status", "deployment/web", "--timeout=180s"); err != nil {
		t.Fatalf("deployment not ready: %v", err)
	}

	d := driverFor(t, c.Kubeconfig, c.Context).WithConfirmTimeout(20 * time.Second)
	selector := map[string]any{"namespaces": []any{ns}, "labelSelectors": map[string]any{"app": "web"}}

	for _, tc := range []struct {
		name     string
		kind     string
		spec     map[string]any
		wantErr  bool
		wantSays string
	}{
		{
			name:     "a pod kill takes",
			kind:     "PodChaos",
			spec:     map[string]any{"action": "pod-kill", "mode": "one", "selector": selector},
			wantSays: "AllInjected=True",
		},
		{
			name: "an IOChaos on a volume that is not there never does",
			kind: "IOChaos",
			spec: map[string]any{
				"action": "latency", "mode": "one", "delay": "100ms", "percent": int64(100),
				"volumePath": "/no/such/volume", "path": "/no/such/volume/**", "selector": selector,
			},
			wantErr:  true,
			wantSays: "No such file",
		},
		{
			name: "a selector that matches nothing never does",
			kind: "PodChaos",
			spec: map[string]any{"action": "pod-kill", "mode": "one", "selector": map[string]any{
				"namespaces": []any{ns}, "labelSelectors": map[string]any{"app": "nothing"},
			}},
			wantErr:  true,
			wantSays: "Selected=False",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uid, err := d.Apply(ctx, simian.FaultManifest{
				UID:          "f-e2e-confirm",
				Engine:       simian.EngineChaosMesh,
				APIVersion:   chaosmesh.APIGroup + "/v1alpha1",
				ResourceKind: tc.kind,
				Duration:     time.Minute,
				Targets:      []simian.TargetRef{{Namespace: ns, Labels: map[string]string{"app": "web"}}},
				Spec:         tc.spec,
			})
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			t.Cleanup(func() { _ = d.Clear(context.Background(), uid) })

			observed, err := d.ConfirmInjected(ctx, uid)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ConfirmInjected err = %v, wantErr %v (observed %q)", err, tc.wantErr, observed)
			}
			if !strings.Contains(observed, tc.wantSays) {
				t.Errorf("observed = %q, want it to contain %q", observed, tc.wantSays)
			}
		})
	}
}

func restConfigFor(t *testing.T, kubeconfig, context string) *rest.Config {
	t.Helper()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig},
		&clientcmd.ConfigOverrides{CurrentContext: context},
	).ClientConfig()
	if err != nil {
		t.Fatalf("kubeconfig: %v", err)
	}
	return cfg
}

func driverFor(t *testing.T, kubeconfig, context string) *chaosmesh.Driver {
	t.Helper()
	cfg := restConfigFor(t, kubeconfig, context)
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("dynamic client: %v", err)
	}
	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		t.Fatalf("discovery client: %v", err)
	}
	return chaosmesh.New(dyn, memory.NewMemCacheClient(disco), "simian-e2e-")
}
