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
	"sync"
	"testing"

	"github.com/go-steer/simian-agent/internal/testutil"
	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/lease"
	"github.com/go-steer/simian-agent/pkg/probe"
	"github.com/go-steer/simian-agent/pkg/simian"
)

// fakeProber returns canned results in order and records what it was asked.
type fakeProber struct {
	mu      sync.Mutex
	results []probe.Result
	calls   []simian.ProbeSpec
	nsSeen  []string
}

func (f *fakeProber) Run(_ context.Context, p simian.ProbeSpec, ns string) probe.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, p)
	f.nsSeen = append(f.nsSeen, ns)
	if len(f.results) == 0 {
		return probe.Result{Name: p.Name, Type: p.Type, Passed: true}
	}
	r := f.results[0]
	f.results = f.results[1:]
	r.Name, r.Type = p.Name, p.Type
	return r
}

func (f *fakeProber) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// settleProbe is a Settle-mode ProbeSpec with the given name.
func settleProbe(name string) simian.ProbeSpec {
	return simian.ProbeSpec{Name: name, Type: simian.ProbeTypeK8s, Mode: simian.ProbeModeSettle}
}

// newProbedExecutor builds an executor whose only arena is "online-boutique",
// wiring prober unless it is nil.
func newProbedExecutor(t *testing.T, prober probe.Prober) (*Executor, *testutil.FakeDriver, *testutil.FakeAuditor, *lease.Registry) {
	t.Helper()
	driver := &testutil.FakeDriver{EngineName: simian.EngineChaosMesh}
	registry := lease.NewRegistry("test-holder")
	auditor := &testutil.FakeAuditor{}
	elig := &StaticEligibility{Eligible: map[string]bool{"online-boutique": true}}
	opts := []Option{WithHistory(NewHistory(10))}
	if prober != nil {
		opts = append(opts, WithProber(prober))
	}
	exec := New(DefaultConfig(), map[simian.Engine]simian.ChaosDriver{simian.EngineChaosMesh: driver},
		registry, auditor, elig, opts...)
	return exec, driver, auditor, registry
}

func asExecutorError(t *testing.T, err error) *simian.ExecutorError {
	t.Helper()
	var ee *simian.ExecutorError
	if !errors.As(err, &ee) {
		t.Fatalf("error is not *simian.ExecutorError: %T %v", err, err)
	}
	return ee
}

func TestApplyWaitsForTheSettleProbeBeforeReportingSuccess(t *testing.T) {
	prober := &fakeProber{results: []probe.Result{
		{Passed: true, Observed: "CrashLoopBackOff", Attempts: 4},
	}}
	exec, _, auditor, registry := newProbedExecutor(t, prober)

	m := goodManifest()
	m.Probes = []simian.ProbeSpec{settleProbe("crashloop")}
	uid, err := exec.Apply(context.Background(), m)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if prober.callCount() != 1 {
		t.Fatalf("prober calls=%d, want 1 — the gate did not run", prober.callCount())
	}
	if _, ok := registry.Get(uid); !ok {
		t.Fatal("fault not registered after a passing gate")
	}
	if got := len(exec.Recent("", 10)); got != 1 {
		t.Fatalf("history entries=%d, want 1", got)
	}

	ev, ok := auditor.FindEvent(audit.EventFaultEfficacy)
	if !ok {
		t.Fatal("no fault.efficacy event emitted")
	}
	if ev.Payload["passed"] != true {
		t.Errorf("payload passed=%v, want true", ev.Payload["passed"])
	}
	// The observed value is the point: a boolean alone cannot be debugged
	// once the arena is gone.
	if got := ev.Payload["observed"]; got != "CrashLoopBackOff" {
		t.Errorf("payload observed=%q, want %q", got, "CrashLoopBackOff")
	}
	if got := ev.Payload["attempts"]; got != 4 {
		t.Errorf("payload attempts=%v, want 4", got)
	}
	if ev.FaultUID != uid {
		t.Errorf("efficacy event FaultUID=%q, want %q", ev.FaultUID, uid)
	}
}

func TestApplyFailsDistinguishablyWhenAProbeNeverPasses(t *testing.T) {
	prober := &fakeProber{results: []probe.Result{
		{Passed: false, Observed: "Running", Expected: `"CrashLoopBackOff" in output`, Attempts: 45},
	}}
	exec, driver, auditor, registry := newProbedExecutor(t, prober)

	m := goodManifest()
	m.Probes = []simian.ProbeSpec{settleProbe("crashloop")}
	uid, err := exec.Apply(context.Background(), m)
	if err == nil {
		t.Fatal("Apply succeeded on a probe that never passed")
	}
	if uid != "" {
		t.Errorf("uid=%q, want empty on failure", uid)
	}

	ee := asExecutorError(t, err)
	if ee.Stage != simian.StageProbe {
		t.Errorf("Stage=%q, want %q", ee.Stage, simian.StageProbe)
	}
	// Distinguishable from a driver failure: the cluster accepted this fault,
	// it just did nothing.
	if ee.Reason != simian.ReasonProbeFailed {
		t.Errorf("Reason=%q, want %q", ee.Reason, simian.ReasonProbeFailed)
	}
	if !strings.Contains(ee.Error(), "crashloop") {
		t.Errorf("error does not name the probe that timed out: %v", ee)
	}
	if !strings.Contains(ee.Error(), "Running") {
		t.Errorf("error does not report what was last seen: %v", ee)
	}

	if got := len(driver.Cleared); got != 1 {
		t.Fatalf("driver.Cleared=%d, want 1 — an unmanifested fault must be backed out", got)
	}
	if _, ok := registry.Get(m.UID); ok {
		t.Error("lease still registered after a successful rollback")
	}
	if got := len(exec.Recent("", 10)); got != 0 {
		t.Errorf("history entries=%d, want 0 — a fault that never landed is not a data point", got)
	}
	ev, ok := auditor.FindEvent(audit.EventFaultEfficacy)
	if !ok {
		t.Fatal("no fault.efficacy event on failure")
	}
	if ev.Payload["passed"] != false {
		t.Errorf("payload passed=%v, want false", ev.Payload["passed"])
	}
	if got := ev.Payload["observed"]; got != "Running" {
		t.Errorf("payload observed=%q, want %q", got, "Running")
	}
}

func TestApplyLeavesTheLeaseForTheReaperWhenRollbackFails(t *testing.T) {
	prober := &fakeProber{results: []probe.Result{{Passed: false, Observed: "Running"}}}
	exec, driver, auditor, registry := newProbedExecutor(t, prober)
	driver.ClearFn = func(context.Context, string) error { return errors.New("apiserver down") }

	m := goodManifest()
	m.UID = "f-rollback"
	m.Probes = []simian.ProbeSpec{settleProbe("crashloop")}
	if _, err := exec.Apply(context.Background(), m); err == nil {
		t.Fatal("Apply succeeded on a failing probe")
	}
	// The fault is real and still in the cluster. Forgetting the lease here
	// would make it unreapable, so it must stay.
	if _, ok := registry.Get("f-rollback"); !ok {
		t.Fatal("lease dropped even though the clear failed — the fault is now orphaned")
	}
	ev, ok := auditor.FindEvent(audit.EventLeaseCleared)
	if !ok {
		t.Fatal("no lease.cleared event")
	}
	if ev.Payload["left_to_reaper"] != true {
		t.Errorf("payload left_to_reaper=%v, want true", ev.Payload["left_to_reaper"])
	}
	if _, ok := ev.Payload["clear_error"]; !ok {
		t.Error("payload does not record why the clear failed")
	}
}

func TestApplyRefusesSettleProbesWhenNoProberIsWired(t *testing.T) {
	exec, driver, _, _ := newProbedExecutor(t, nil)

	m := goodManifest()
	m.Probes = []simian.ProbeSpec{settleProbe("crashloop")}
	_, err := exec.Apply(context.Background(), m)
	if err == nil {
		t.Fatal("Apply accepted Settle probes with no prober configured — the gate was silently skipped")
	}
	ee := asExecutorError(t, err)
	if ee.Reason != simian.ReasonProbeNotConfigured {
		t.Errorf("Reason=%q, want %q", ee.Reason, simian.ReasonProbeNotConfigured)
	}
	if got := len(driver.Cleared); got != 1 {
		t.Errorf("driver.Cleared=%d, want 1 — the applied fault must still be backed out", got)
	}
}

func TestApplyIsUnchangedForAManifestWithNoSettleProbes(t *testing.T) {
	prober := &fakeProber{}
	exec, _, auditor, _ := newProbedExecutor(t, prober)

	m := goodManifest()
	// Non-Settle modes are carried but not scheduled by Simian.
	m.Probes = []simian.ProbeSpec{
		{Name: "continuous-http", Type: simian.ProbeTypeHTTP, Mode: simian.ProbeModeContinuous},
		{Name: "eot", Type: simian.ProbeTypeK8s, Mode: simian.ProbeModeEOT},
	}
	if _, err := exec.Apply(context.Background(), m); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if prober.callCount() != 0 {
		t.Errorf("prober ran %d time(s) for non-Settle probes, want 0", prober.callCount())
	}
	if _, ok := auditor.FindEvent(audit.EventFaultEfficacy); ok {
		t.Error("fault.efficacy emitted for a fault with no Settle probes")
	}
}

func TestApplyStopsAtTheFirstFailingProbe(t *testing.T) {
	prober := &fakeProber{results: []probe.Result{
		{Passed: true, Observed: "Pending"},
		{Passed: false, Observed: ""},
	}}
	exec, _, auditor, _ := newProbedExecutor(t, prober)

	m := goodManifest()
	m.Probes = []simian.ProbeSpec{settleProbe("pending"), settleProbe("unschedulable"), settleProbe("never-reached")}
	if _, err := exec.Apply(context.Background(), m); err == nil {
		t.Fatal("Apply succeeded despite a failing probe")
	}
	if prober.callCount() != 2 {
		t.Fatalf("prober calls=%d, want 2 — probes after the failure should not run", prober.callCount())
	}
	var efficacy int
	for _, ev := range auditor.Events {
		if ev.Event == audit.EventFaultEfficacy {
			efficacy++
		}
	}
	if efficacy != 2 {
		t.Errorf("fault.efficacy events=%d, want 2 (one per probe that ran)", efficacy)
	}
}

func TestSettleProbesGetTheFaultsOwnNamespace(t *testing.T) {
	prober := &fakeProber{}
	exec, _, _, _ := newProbedExecutor(t, prober)

	m := goodManifest()
	m.Probes = []simian.ProbeSpec{settleProbe("crashloop")}
	if _, err := exec.Apply(context.Background(), m); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := prober.nsSeen[0]; got != "online-boutique" {
		t.Errorf("probe default namespace=%q, want %q", got, "online-boutique")
	}
}

func TestSettleProbesFilterByMode(t *testing.T) {
	m := simian.FaultManifest{Probes: []simian.ProbeSpec{
		{Name: "a", Mode: simian.ProbeModeSettle},
		{Name: "b", Mode: simian.ProbeModeEOT},
		{Name: "c", Mode: simian.ProbeModeSettle},
		{Name: "d", Mode: ""},
	}}
	got := m.SettleProbes()
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "c" {
		t.Fatalf("SettleProbes()=%+v, want a and c in order", got)
	}
}
