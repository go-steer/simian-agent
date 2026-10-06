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

package metrics

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	internaltest "github.com/go-steer/simian-agent/internal/testutil"
	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/executor"
	"github.com/go-steer/simian-agent/pkg/lease"
	"github.com/go-steer/simian-agent/pkg/simian"
)

func manifest(kind, ns, name string) simian.FaultManifest {
	return simian.FaultManifest{
		Source: simian.SourceAutonomous, Engine: simian.EngineChaosMesh, APIVersion: "chaos-mesh.org/v1alpha1",
		ResourceKind: kind, Duration: time.Minute,
		Spec:    map[string]any{"action": "pod-kill", "mode": "one", "selector": map[string]any{"labelSelectors": map[string]any{"app": name}}},
		Targets: []simian.TargetRef{{Namespace: ns, Name: name, Labels: map[string]string{"app": name}}},
	}
}

// The metrics are counted from what the executor really emits, so a real
// Apply is the test of the event shapes, not a hand-built payload.
func TestTheExecutorsEventsAreCounted(t *testing.T) {
	r := New("v-test")
	reg := lease.NewRegistry("h")
	exec := executor.New(executor.DefaultConfig(),
		map[simian.Engine]simian.ChaosDriver{simian.EngineChaosMesh: &internaltest.FakeDriver{EngineName: simian.EngineChaosMesh}},
		reg, r, &executor.StaticEligibility{Eligible: map[string]bool{"boutique": true}})
	r.WatchActive(exec)

	uid, err := exec.Apply(context.Background(), manifest("PodChaos", "boutique", "cartservice"))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := exec.Apply(context.Background(), manifest("PodChaos", "kube-system", "coredns")); err == nil {
		t.Fatal("a fault outside the arena was accepted")
	}
	if got := testutil.ToFloat64(r.applied.WithLabelValues("boutique", "chaos-mesh", "PodChaos", "autonomous")); got != 1 {
		t.Errorf("applied = %v, want 1", got)
	}
	if got := testutil.ToFloat64(r.ended.WithLabelValues("kube-system", "PodChaos", "refused", string(simian.ReasonNamespaceNotEligible))); got != 1 {
		t.Errorf("refused = %v, want 1", got)
	}

	// The lease ends at its deadline and the workload is checked.
	r.Emit(context.Background(), simian.AuditEvent{Event: audit.EventLeaseExpired, FaultUID: uid, Reason: "deadline-reached"})
	r.Emit(context.Background(), simian.AuditEvent{Event: audit.EventFaultRecovered, FaultUID: uid, Payload: map[string]any{"passed": false}})
	if got := testutil.ToFloat64(r.ended.WithLabelValues("boutique", "PodChaos", "expired", "deadline-reached")); got != 1 {
		t.Errorf("expired = %v, want 1", got)
	}
	if got := testutil.ToFloat64(r.recovered.WithLabelValues("boutique", "PodChaos", "false")); got != 1 {
		t.Errorf("recovery failures = %v, want 1", got)
	}
}

func TestCyclesAndTheEndpoint(t *testing.T) {
	r := New("v-test")
	r.WatchActive(fakeActive{{Manifest: manifest("NetworkChaos", "bank", "frontend")}})
	emit := func(e simian.AuditEvent) { r.Emit(context.Background(), e) }
	emit(simian.AuditEvent{Event: audit.EventCycleCompleted, Payload: map[string]any{"namespace": "bank"}})
	emit(simian.AuditEvent{Event: audit.EventCycleSkipped, Reason: "health-gate", Payload: map[string]any{"namespace": "bank"}})
	emit(simian.AuditEvent{Event: audit.EventCycleSkipped, Reason: "health-gate", Payload: map[string]any{"namespace": "bank"}})
	emit(simian.AuditEvent{Event: audit.EventLeaseCleared, FaultUID: "f-x", Reason: "driver-clear-failed"}) // still in the cluster: not an end

	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	for _, want := range []string{
		`simian_cycles_total{namespace="bank",outcome="completed",reason=""} 1`,
		`simian_cycles_total{namespace="bank",outcome="skipped",reason="health-gate"} 2`,
		`simian_active_faults{namespace="bank"} 1`,
		`simian_build_info{version="v-test"} 1`,
		`go_goroutines`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("/metrics lacks %s", want)
		}
	}
	if strings.Contains(string(body), "simian_faults_ended_total{") {
		t.Error("a clear that left the fault in the cluster was counted as an end")
	}
}

type fakeActive []simian.ActiveFault

func (f fakeActive) ListActive(context.Context, string) ([]simian.ActiveFault, error) { return f, nil }

// A fault the previous controller applied is known to this one only through
// adoption; its end and recovery are still counted against its arena and kind.
func TestAnAdoptedFaultIsCountedWithItsLabels(t *testing.T) {
	r := New("v-test")
	r.Remember(simian.ActiveFault{FaultUID: "f-old", Manifest: manifest("PodChaos", "boutique", "productcatalogservice")})
	r.Emit(context.Background(), simian.AuditEvent{Event: audit.EventLeaseExpired, FaultUID: "f-old", Reason: "deadline-reached"})
	r.Emit(context.Background(), simian.AuditEvent{Event: audit.EventFaultRecovered, FaultUID: "f-old", Payload: map[string]any{"passed": true}})
	if got := testutil.ToFloat64(r.ended.WithLabelValues("boutique", "PodChaos", "expired", "deadline-reached")); got != 1 {
		t.Errorf("expired = %v, want 1 under boutique/PodChaos", got)
	}
	if got := testutil.ToFloat64(r.recovered.WithLabelValues("boutique", "PodChaos", "true")); got != 1 {
		t.Errorf("recovered = %v, want 1 under boutique/PodChaos", got)
	}
}
