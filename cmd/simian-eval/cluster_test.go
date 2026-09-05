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

package main

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	apiversion "k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/go-steer/simian-agent/pkg/arena"
	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/executor"
)

const kubeconfigYAML = `apiVersion: v1
kind: Config
current-context: eval
clusters:
  - name: eval
    cluster:
      server: https://127.0.0.1:6443
contexts:
  - name: eval
    context:
      cluster: eval
      user: eval
users:
  - name: eval
    user:
      token: not-a-real-token
`

func writeKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(kubeconfigYAML), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// --cluster current is the default because the cluster people want this
// pointed at is one they already have — and a run must never delete it.
func TestClusterCurrentUsesTheKubeconfigAndPutsNothingBack(t *testing.T) {
	o := defaultOptions()
	o.kubeconfig = writeKubeconfig(t)

	cfg, cleanup, err := resolveCluster(t.Context(), o, "run-1", &bytes.Buffer{})
	if err != nil {
		t.Fatalf("resolveCluster: %v", err)
	}
	if cfg.Host != "https://127.0.0.1:6443" {
		t.Errorf("host = %q, want the kubeconfig's server", cfg.Host)
	}
	if cleanup == nil {
		t.Fatal("cleanup is nil; runEval defers it unconditionally")
	}
	cleanup(t.Context()) // a cluster this run did not create is left standing
}

func TestAnUnreadableKubeconfigIsRefused(t *testing.T) {
	o := defaultOptions()
	o.kubeconfig = filepath.Join(t.TempDir(), "there-is-no-kubeconfig")

	_, _, err := resolveCluster(t.Context(), o, "run-1", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "kubeconfig") {
		t.Fatalf("resolveCluster = %v, want a kubeconfig error", err)
	}
}

func TestBuildPlaneWiresTheSameEnginesTheOperatorRuns(t *testing.T) {
	cfg, _, err := resolveCluster(t.Context(), &options{kubeconfig: writeKubeconfig(t)}, "run-1", &bytes.Buffer{})
	if err != nil {
		t.Fatalf("resolveCluster: %v", err)
	}
	o := defaultOptions()
	o.durationCap = time.Hour

	p, err := buildPlane(cfg, o, audit.New(discardLogger()), discardLogger())
	if err != nil {
		t.Fatalf("buildPlane: %v", err)
	}
	if p.k8s == nil || p.executor == nil || p.arenas == nil || p.reaper == nil {
		t.Fatalf("plane = %+v, want every piece wired", p)
	}
	if p.reaper.Interval != o.reapInterval {
		t.Errorf("reaper interval = %s, want %s", p.reaper.Interval, o.reapInterval)
	}
	// The orphan scan deletes chaos this process did not apply. A rig pointed
	// at somebody's real cluster has no business doing that, so the reaper here
	// is only the backstop for leases this run took out and failed to release.
	if p.reaper.Namespaces != nil {
		t.Error("the reaper has a namespace resolver; the orphan scan must be off")
	}
	// Destroy consults the dynamic client before it deletes a namespace: an
	// arena with live chaos in it is a refusal, not a cascade delete.
	if p.arenas.Dyn == nil {
		t.Error("the arena manager has no dynamic client; Destroy cannot tell whether chaos is still live")
	}
}

// Annotation by default, because the harness annotates the arenas it creates
// and that is the whole gate. --eligible-namespace is the tighter setting: a
// fixed list regardless of what anyone annotated, for a cluster with other
// tenants on it.
func TestEligibilityIsByAnnotationUnlessAListIsGiven(t *testing.T) {
	k8s := fake.NewClientset()

	byAnnotation := buildEligibility(k8s, nil, discardLogger())
	if _, ok := byAnnotation.(*executor.StaticEligibility); ok {
		t.Error("the default fenced to a static list; nothing would be eligible")
	}
	if byAnnotation == nil {
		t.Fatal("eligibility is nil; the executor would have nothing to ask")
	}

	static, ok := buildEligibility(k8s, []string{"shop", "pay"}, discardLogger()).(*executor.StaticEligibility)
	if !ok {
		t.Fatalf("--eligible-namespace did not produce a static allowlist")
	}
	if !static.Eligible["shop"] || static.Eligible["kube-system"] {
		t.Errorf("allowlist = %v, want exactly the namespaces asked for", static.Eligible)
	}
	// The annotation constant is what the KubeArena writes, and the two have to
	// be the same string or nothing is ever eligible.
	if arena.EligibilityAnnotation == "" {
		t.Error("the eligibility annotation is empty")
	}
}

// One connection error up front beats the same error once per scenario, with a
// scorecard full of inject failures on the end of it.
func TestCheckReachableAsksTheClusterBeforeAnythingIsCreated(t *testing.T) {
	k8s := fake.NewClientset()
	k8s.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &apiversion.Info{GitVersion: "v1.31.0"}

	got, err := checkReachable(k8s)
	if err != nil {
		t.Fatalf("checkReachable: %v", err)
	}
	if got != "v1.31.0" {
		t.Errorf("version = %q, want v1.31.0", got)
	}
}

func TestAnUnreachableClusterStopsTheRun(t *testing.T) {
	k8s := fake.NewClientset()
	k8s.PrependReactor("get", "version", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("dial tcp: connection refused")
	})

	if _, err := checkReachable(k8s); err == nil || !strings.Contains(err.Error(), "not reachable") {
		t.Fatalf("checkReachable = %v, want it to say the cluster is not reachable", err)
	}
}
