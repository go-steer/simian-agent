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

package loop

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/executor"
	"github.com/go-steer/simian-agent/pkg/llm/stub"
	"github.com/go-steer/simian-agent/pkg/planner"
	"github.com/go-steer/simian-agent/pkg/simian"
	"github.com/go-steer/simian-agent/pkg/sut"
	"github.com/go-steer/simian-agent/pkg/topology"
)

// recordingExecutor implements simian.FaultExecutor and remembers Apply calls.
type recordingExecutor struct {
	mu      sync.Mutex
	applied []simian.FaultManifest
	err     error
	delay   time.Duration
}

func (r *recordingExecutor) Apply(_ context.Context, m simian.FaultManifest) (string, error) {
	if r.delay > 0 {
		time.Sleep(r.delay)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.applied = append(r.applied, m)
	if r.err != nil {
		return "", r.err
	}
	return "f-" + m.ResourceKind, nil
}

func (r *recordingExecutor) Clear(_ context.Context, _ string) error { return nil }
func (r *recordingExecutor) ListActive(_ context.Context, _ string) ([]simian.ActiveFault, error) {
	return nil, nil
}

func (r *recordingExecutor) AppliedCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.applied)
}

type recordingAuditor struct {
	mu     sync.Mutex
	events []simian.AuditEvent
}

func (a *recordingAuditor) Emit(_ context.Context, e simian.AuditEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, e)
}

func (a *recordingAuditor) Has(event string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.events {
		if e.Event == event {
			return true
		}
	}
	return false
}

type catalogStub struct{}

func (catalogStub) gather(_ context.Context) ([]simian.CatalogEntry, error) {
	return []simian.CatalogEntry{{
		Engine: simian.EngineChaosMesh, ResourceKind: "PodChaos",
		APIVersion: "chaos-mesh.org/v1alpha1", BlastRadiusTier: simian.TierNamespace,
	}}, nil
}

type fakeRecents struct{}

func (fakeRecents) Recent(_ string, _ int) []executor.RecentFault { return nil }

type alwaysHealthy struct{}

func (alwaysHealthy) Check(_ context.Context, _ string) error { return nil }

type alwaysUnhealthy struct{ msg string }

func (u alwaysUnhealthy) Check(_ context.Context, _ string) error { return errors.New(u.msg) }

func planJSON(stepCount int) string {
	out := `{"hypothesis":"x","steps":[`
	for i := 1; i <= stepCount; i++ {
		if i > 1 {
			out += ","
		}
		out += `{"order":` + itoa(i) + `,"manifest":{"engine":"chaos-mesh","api_version":"chaos-mesh.org/v1alpha1","resource_kind":"PodChaos","spec":{"action":"pod-kill"},"targets":[{"namespace":"boutique"}],"duration":"30s","blast_radius_tier":"namespace"}}`
	}
	out += `]}`
	return out
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

func newLoopUnderTest(t *testing.T, planJSON string, exec simian.FaultExecutor, budget planner.Budget) (*Loop, *recordingAuditor) {
	t.Helper()
	llm := stub.New("stub")
	llm.AddRule(stub.ResponseRule{
		Match:    func(simian.CompletionRequest) bool { return true },
		Response: simian.CompletionResponse{Text: planJSON},
	})
	au := &recordingAuditor{}
	return &Loop{
		Namespaces: []string{"boutique"},
		Interval:   time.Second,
		Generator:  planner.NewGenerator(llm),
		Executor:   exec,
		Topology:   &fakeTopology{snap: goodSnapshot()},
		Baselines:  &fakeBaselines{bl: goodBaseline(), ok: true},
		Recents:    fakeRecents{},
		Catalog:    catalogStub{}.gather,
		Health:     alwaysHealthy{},
		Budget:     budget,
		Auditor:    au,
	}, au
}

func TestRunOnce_HappyPathAppliesAllSteps(t *testing.T) {
	exec := &recordingExecutor{}
	l, au := newLoopUnderTest(t, planJSON(3), exec, planner.Budget{
		MaxFaultsPerCycle: 5, MaxConcurrentFaults: 3, MaxSeverityPerCycle: simian.TierNamespace,
	})
	plan, applied, err := l.RunOnce(context.Background(), "boutique")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(plan.Steps) != 3 {
		t.Errorf("plan steps=%d, want 3", len(plan.Steps))
	}
	if len(applied) != 3 || exec.AppliedCount() != 3 {
		t.Errorf("applied=%d exec=%d, want 3 each", len(applied), exec.AppliedCount())
	}
	for _, ev := range []string{audit.EventCycleStarted, audit.EventPlanGenerated, audit.EventCycleCompleted} {
		if !au.Has(ev) {
			t.Errorf("missing audit event %q", ev)
		}
	}
}

// A step that is never submitted leaves no executor record, so plan.generated
// is the only place the whole plan is written down (#142).
func TestRunOnce_PlanGeneratedCarriesTheSteps(t *testing.T) {
	l, au := newLoopUnderTest(t, planJSON(2), &recordingExecutor{}, planner.Budget{
		MaxFaultsPerCycle: 5, MaxConcurrentFaults: 3, MaxSeverityPerCycle: simian.TierNamespace,
	})
	if _, _, err := l.RunOnce(context.Background(), "boutique"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	var steps []any
	for _, e := range au.events {
		if e.Event == audit.EventPlanGenerated {
			steps, _ = e.Payload["steps"].([]any)
		}
	}
	if len(steps) != 2 {
		t.Fatalf("plan.generated steps = %d, want 2", len(steps))
	}
	step := steps[1].(map[string]any)
	fault, _ := step["fault"].(map[string]any)
	if step["order"] != 2 || fault["kind"] != "PodChaos" || fault["spec"].(map[string]any)["action"] != "pod-kill" {
		t.Errorf("step = %v", step)
	}
	if targets, _ := fault["targets"].([]any); len(targets) != 1 || targets[0].(map[string]any)["namespace"] != "boutique" {
		t.Errorf("step targets = %v", fault["targets"])
	}
}

func TestRunOnce_HealthGateSkips(t *testing.T) {
	exec := &recordingExecutor{}
	l, au := newLoopUnderTest(t, planJSON(2), exec, planner.Budget{MaxFaultsPerCycle: 5, MaxConcurrentFaults: 1})
	l.Health = alwaysUnhealthy{msg: "no baseline"}
	plan, applied, err := l.RunOnce(context.Background(), "boutique")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(plan.Steps) != 0 || applied != nil {
		t.Errorf("expected empty plan + nil applied, got plan=%v applied=%v", plan, applied)
	}
	if exec.AppliedCount() != 0 {
		t.Errorf("executor called %d times despite gate fail", exec.AppliedCount())
	}
	if !au.Has(audit.EventHealthGateFailed) || !au.Has(audit.EventCycleSkipped) {
		t.Errorf("missing gate-fail audit events")
	}
}

func TestRunOnce_LLMUnavailableSkipsCleanly(t *testing.T) {
	exec := &recordingExecutor{}
	au := &recordingAuditor{}
	l := &Loop{
		Namespaces: []string{"boutique"},
		Interval:   time.Second,
		Generator:  planner.NewGenerator(stubFailing{}),
		Executor:   exec,
		Topology:   &fakeTopology{snap: goodSnapshot()},
		Baselines:  &fakeBaselines{bl: goodBaseline(), ok: true},
		Recents:    fakeRecents{},
		Catalog:    catalogStub{}.gather,
		Health:     alwaysHealthy{},
		Budget:     planner.Budget{MaxFaultsPerCycle: 3, MaxConcurrentFaults: 1},
		Auditor:    au,
	}
	plan, applied, err := l.RunOnce(context.Background(), "boutique")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(plan.Steps) != 0 || applied != nil || exec.AppliedCount() != 0 {
		t.Errorf("LLM-down should result in no apply; got plan=%v applied=%v exec=%d", plan, applied, exec.AppliedCount())
	}
	if !au.Has(audit.EventLLMUnavailable) || !au.Has(audit.EventCycleSkipped) {
		t.Errorf("missing llm-unavailable / cycle-skipped audit events")
	}
}

func TestRunOnce_CycleBudgetTruncates(t *testing.T) {
	exec := &recordingExecutor{}
	l, _ := newLoopUnderTest(t, planJSON(5), exec, planner.Budget{
		MaxFaultsPerCycle: 2, MaxConcurrentFaults: 5, MaxSeverityPerCycle: simian.TierNamespace,
	})
	_, applied, err := l.RunOnce(context.Background(), "boutique")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(applied) != 2 {
		t.Errorf("applied=%d, want 2 (truncated by MaxFaultsPerCycle)", len(applied))
	}
}

func TestRunOnce_SeverityCapSkipsHigherTier(t *testing.T) {
	plan := `{
		"hypothesis":"x",
		"steps":[
			{"order":1,"manifest":{"engine":"chaos-mesh","api_version":"v","resource_kind":"PodChaos","spec":{"a":1},"targets":[{"namespace":"boutique"}],"duration":"30s","blast_radius_tier":"namespace"}},
			{"order":2,"manifest":{"engine":"chaos-mesh","api_version":"v","resource_kind":"KernelChaos","spec":{"a":1},"targets":[{"namespace":"boutique"}],"duration":"30s","blast_radius_tier":"node"}}
		]
	}`
	exec := &recordingExecutor{}
	l, au := newLoopUnderTest(t, plan, exec, planner.Budget{
		MaxFaultsPerCycle: 5, MaxConcurrentFaults: 5, MaxSeverityPerCycle: simian.TierNamespace,
	})
	_, applied, err := l.RunOnce(context.Background(), "boutique")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(applied) != 1 || exec.applied[0].ResourceKind != "PodChaos" {
		t.Errorf("expected only PodChaos applied, got %v", applied)
	}
	if !au.Has(audit.EventStepSkipped) {
		t.Errorf("missing step-skipped audit event for severity-cap")
	}
}

func TestRunOnce_ConcurrencyOneSerializes(t *testing.T) {
	exec := &recordingExecutor{delay: 50 * time.Millisecond}
	plan := `{
		"hypothesis":"x",
		"steps":[
			{"order":1,"manifest":{"engine":"chaos-mesh","api_version":"v","resource_kind":"PodChaos","spec":{"a":1},"targets":[{"namespace":"boutique"}],"duration":"30s","blast_radius_tier":"namespace"}},
			{"order":2,"manifest":{"engine":"chaos-mesh","api_version":"v","resource_kind":"NetworkChaos","spec":{"a":1},"targets":[{"namespace":"boutique"}],"duration":"30s","blast_radius_tier":"namespace"}},
			{"order":3,"manifest":{"engine":"chaos-mesh","api_version":"v","resource_kind":"StressChaos","spec":{"a":1},"targets":[{"namespace":"boutique"}],"duration":"30s","blast_radius_tier":"namespace"}}
		]
	}`
	l, _ := newLoopUnderTest(t, plan, exec, planner.Budget{
		MaxFaultsPerCycle: 5, MaxConcurrentFaults: 1, MaxSeverityPerCycle: simian.TierNamespace,
	})
	start := time.Now()
	_, applied, err := l.RunOnce(context.Background(), "boutique")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(applied) != 3 {
		t.Errorf("applied=%d, want 3", len(applied))
	}
	// Three serial 50ms calls should take >= 150ms; parallel would be ~50ms.
	if time.Since(start) < 140*time.Millisecond {
		t.Errorf("steps appear to have run in parallel; elapsed=%s", time.Since(start))
	}
}

func TestRunOnce_StepFailureDoesNotAbortSiblings(t *testing.T) {
	exec := &recordingExecutor{err: errors.New("simulated reject")}
	l, au := newLoopUnderTest(t, planJSON(2), exec, planner.Budget{
		MaxFaultsPerCycle: 5, MaxConcurrentFaults: 5, MaxSeverityPerCycle: simian.TierNamespace,
	})
	_, applied, err := l.RunOnce(context.Background(), "boutique")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("applied=%d, want 0 (executor errored on every step)", len(applied))
	}
	if exec.AppliedCount() != 2 {
		t.Errorf("executor saw %d calls, want 2 (both attempted)", exec.AppliedCount())
	}
	if !au.Has(audit.EventStepSkipped) {
		t.Errorf("missing step-skipped audit event for executor reject")
	}
}

// stubFailing always returns an error from Complete, simulating an outage.
type stubFailing struct{}

func (stubFailing) Name() string { return "failing" }
func (stubFailing) Complete(_ context.Context, _ simian.CompletionRequest) (simian.CompletionResponse, error) {
	return simian.CompletionResponse{}, errors.New("provider unreachable")
}

// Compile-time interface satisfaction sanity (catches refactors that drift the contract).
var _ simian.FaultExecutor = (*recordingExecutor)(nil)
var _ BaselineLookup = (*fakeBaselines)(nil)
var _ TopologySnapshotter = (*fakeTopology)(nil)
var _ ActiveFaultsLookup = (*fakeActive)(nil)
var _ RecentLookup = fakeRecents{}
var _ HealthGate = alwaysHealthy{}
var _ HealthGate = alwaysUnhealthy{}

// Ensure baseline status type is reachable; protects against accidental
// removal of sut.WorkloadStatus during refactors.
var _ = sut.WorkloadStatus{}
var _ = topology.Workload{}

func TestTierExceedsFailsClosedOnBothSides(t *testing.T) {
	// A tier neither side recognises used to sort as "namespace", the least
	// severe thing there is, so an unrecognised manifest sailed under every
	// cap. Fail-closed here means the loop skips what it cannot bound rather
	// than applying it.
	tests := []struct {
		name string
		have simian.BlastRadiusTier
		max  simian.BlastRadiusTier
		want bool
	}{
		{"namespace under a namespace cap", simian.TierNamespace, simian.TierNamespace, false},
		{"node over a namespace cap", simian.TierNode, simian.TierNamespace, true},
		{"node under a node cap", simian.TierNode, simian.TierNode, false},
		{"external over a node cap", simian.TierExternal, simian.TierNode, true},
		{"namespace under an external cap", simian.TierNamespace, simian.TierExternal, false},

		// A blast radius we cannot classify is not a small one.
		{"an unrecognised tier exceeds even the widest cap", "planet-scale", simian.TierExternal, true},
		{"an empty tier exceeds even the widest cap", "", simian.TierExternal, true},
		// And a cap we cannot parse was an operator narrowing something.
		{"an unparseable cap permits nothing", simian.TierNamespace, "namesapce", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tierExceeds(tt.have, tt.max); got != tt.want {
				t.Errorf("tierExceeds(%q, %q) = %v, want %v", tt.have, tt.max, got, tt.want)
			}
		})
	}
}

type fakeRefusals struct {
	fakeRecents
	refused []executor.RefusedFault
}

func (f fakeRefusals) Refused(_ string, _ int) []executor.RefusedFault { return f.refused }

// capturingLLM answers with a fixed plan and keeps the last prompt it saw.
type capturingLLM struct {
	mu   sync.Mutex
	plan string
	user string
}

func (c *capturingLLM) Name() string { return "capturing" }
func (c *capturingLLM) Complete(_ context.Context, req simian.CompletionRequest) (simian.CompletionResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range req.Messages {
		c.user = m.Content
	}
	return simian.CompletionResponse{Text: c.plan}, nil
}

func TestRunOnce_ThePlannerHearsWhatTheExecutorRefused(t *testing.T) {
	llm := &capturingLLM{plan: planJSON(1)}
	l, _ := newLoopUnderTest(t, planJSON(1), &recordingExecutor{}, planner.Budget{
		MaxFaultsPerCycle: 5, MaxConcurrentFaults: 5, MaxSeverityPerCycle: simian.TierNamespace,
	})
	l.Generator = planner.NewGenerator(llm)
	l.Recents = fakeRefusals{refused: []executor.RefusedFault{{
		Manifest:  simian.FaultManifest{ResourceKind: "HTTPChaos", Targets: []simian.TargetRef{{Namespace: "boutique", Name: "frontend"}}},
		RefusedAt: time.Now(),
		Reason:    simian.ReasonSchemaInvalid,
		Error:     "spec.port: required",
	}}}
	if _, _, err := l.RunOnce(context.Background(), "boutique"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	llm.mu.Lock()
	defer llm.mu.Unlock()
	if !strings.Contains(llm.user, "spec.port: required") {
		t.Errorf("planner prompt does not carry the refusal:\n%s", llm.user)
	}
}

// When the planner proposes a refused step anyway, the loop stops submitting
// it after repeatedRefusalLimit refusals rather than asking forever.
func TestRunOnce_ARepeatedlyRefusedStepStopsBeingSubmitted(t *testing.T) {
	exec := &recordingExecutor{err: &simian.ExecutorError{Stage: simian.StageSchema, Reason: simian.ReasonSchemaInvalid, Message: "spec.port: required"}}
	l, au := newLoopUnderTest(t, planJSON(1), exec, planner.Budget{
		MaxFaultsPerCycle: 5, MaxConcurrentFaults: 5, MaxSeverityPerCycle: simian.TierNamespace,
	})
	for range repeatedRefusalLimit + 2 {
		if _, _, err := l.RunOnce(context.Background(), "boutique"); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
	}
	if got := exec.AppliedCount(); got != repeatedRefusalLimit {
		t.Errorf("executor asked %d times, want %d", got, repeatedRefusalLimit)
	}
	au.mu.Lock()
	defer au.mu.Unlock()
	skipped := 0
	for _, e := range au.events {
		if e.Event == audit.EventStepSkipped && e.Reason == "repeated-refusal" {
			skipped++
		}
	}
	if skipped != 2 {
		t.Errorf("repeated-refusal skips = %d, want 2", skipped)
	}
}

func TestRunOnce_BudgetRefusalsDoNotCountTowardsBackoff(t *testing.T) {
	exec := &recordingExecutor{err: &simian.ExecutorError{Stage: simian.StageSafety, Reason: simian.ReasonBudgetExceeded, Message: "cap"}}
	l, _ := newLoopUnderTest(t, planJSON(1), exec, planner.Budget{
		MaxFaultsPerCycle: 5, MaxConcurrentFaults: 5, MaxSeverityPerCycle: simian.TierNamespace,
	})
	for range repeatedRefusalLimit + 2 {
		if _, _, err := l.RunOnce(context.Background(), "boutique"); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
	}
	if got := exec.AppliedCount(); got != repeatedRefusalLimit+2 {
		t.Errorf("executor asked %d times, want %d", got, repeatedRefusalLimit+2)
	}
}

// busyExecutor holds the only fault slot until freeAt, with a lease that
// ends at deadline.
type busyExecutor struct {
	recordingExecutor
	freeAt   time.Time
	deadline time.Time
}

func (b *busyExecutor) ListActive(_ context.Context, _ string) ([]simian.ActiveFault, error) {
	if time.Now().After(b.freeAt) {
		return nil, nil
	}
	return []simian.ActiveFault{{FaultUID: "f-boutique", Deadline: b.deadline}}, nil
}

type countingLLM struct {
	mu    sync.Mutex
	calls int
	text  string
}

func (c *countingLLM) Name() string { return "counting" }
func (c *countingLLM) Complete(context.Context, simian.CompletionRequest) (simian.CompletionResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return simian.CompletionResponse{Text: c.text}, nil
}

func (a *recordingAuditor) find(event, reason string) (simian.AuditEvent, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.events {
		if e.Event == event && e.Reason == reason {
			return e, true
		}
	}
	return simian.AuditEvent{}, false
}

// On the 2026-09-30 trial every one of bank's 46 refusals cost a planning
// call, made while boutique's fault held the only slot (#161).
func TestANamespaceIsSkippedBeforePlanningWhenNoSlotCanOpen(t *testing.T) {
	exec := &busyExecutor{freeAt: time.Now().Add(time.Hour), deadline: time.Now().Add(time.Hour)}
	l, au := newLoopUnderTest(t, planJSON(1), exec, planner.Budget{MaxFaultsPerCycle: 1, MaxConcurrentFaults: 1})
	llm := &countingLLM{text: planJSON(1)}
	l.Generator = planner.NewGenerator(llm)
	l.Namespaces = []string{"boutique", "bank"}
	l.Interval = 5 * time.Minute

	start := time.Now()
	if _, _, err := l.RunOnce(context.Background(), "bank"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Errorf("waited %s for a slot held past the window; should skip at once", waited)
	}
	if llm.calls != 0 || exec.AppliedCount() != 0 {
		t.Errorf("planner calls=%d applies=%d, want 0 each", llm.calls, exec.AppliedCount())
	}
	ev, ok := au.find(audit.EventCycleSkipped, "budget-full")
	if !ok {
		t.Fatal("no cycle.skipped reason=budget-full")
	}
	if ev.Payload["namespace"] != "bank" || ev.Payload["active_faults"] != 1 || ev.Payload["max_concurrent_faults"] != 1 {
		t.Errorf("payload = %v", ev.Payload)
	}
}

func TestANamespaceWaitsForASlotThatFreesWithinItsShareOfTheCycle(t *testing.T) {
	exec := &busyExecutor{freeAt: time.Now().Add(60 * time.Millisecond), deadline: time.Now().Add(60 * time.Millisecond)}
	l, au := newLoopUnderTest(t, planJSON(1), exec, planner.Budget{MaxFaultsPerCycle: 1, MaxConcurrentFaults: 1})
	l.Namespaces = []string{"boutique", "bank"}
	l.Interval = 10 * time.Second
	l.slotPoll = 10 * time.Millisecond

	if _, _, err := l.RunOnce(context.Background(), "bank"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if exec.AppliedCount() != 1 {
		t.Errorf("applies = %d, want 1 once boutique's fault ended", exec.AppliedCount())
	}
	if _, skipped := au.find(audit.EventCycleSkipped, "budget-full"); skipped {
		t.Error("skipped as budget-full although the slot freed in time")
	}
}

func TestEachCycleStartsWithTheNextNamespace(t *testing.T) {
	l := &Loop{Namespaces: []string{"boutique", "bank", "shop"}}
	for cycle, want := range [][]string{
		{"boutique", "bank", "shop"},
		{"bank", "shop", "boutique"},
		{"shop", "boutique", "bank"},
		{"boutique", "bank", "shop"},
	} {
		if got := l.cycleOrder(cycle); !reflect.DeepEqual(got, want) {
			t.Errorf("cycle %d order = %v, want %v", cycle, got, want)
		}
	}
}

// #173: the history a restarted controller rebuilds from the audit trail
// counts towards the repeated-refusal limit, so a step refused before the
// restart is not submitted again straight after it.
func TestRefusalsFromBeforeARestartCountTowardsTheLimit(t *testing.T) {
	var plan simian.AttackPlan
	if err := json.Unmarshal([]byte(planJSON(1)), &plan); err != nil {
		t.Fatal(err)
	}
	refused := make([]executor.RefusedFault, 0, repeatedRefusalLimit+1)
	for i := range repeatedRefusalLimit {
		refused = append(refused, executor.RefusedFault{Manifest: plan.Steps[0].Manifest,
			RefusedAt: time.Now().Add(-time.Duration(i+1) * time.Minute), Reason: simian.ReasonTargetIncompatible})
	}
	// Newest first, as the executor lists them; a budget refusal never counts.
	refused = append(refused, executor.RefusedFault{Manifest: plan.Steps[0].Manifest, RefusedAt: time.Now().Add(-time.Hour / 2), Reason: simian.ReasonBudgetExceeded})

	exec := &recordingExecutor{}
	l, au := newLoopUnderTest(t, planJSON(1), exec, planner.Budget{
		MaxFaultsPerCycle: 5, MaxConcurrentFaults: 5, MaxSeverityPerCycle: simian.TierNamespace,
	})
	l.Recents = fakeRefusals{refused: refused}
	l.seedRefusals()
	if n := l.recentRefusals(stepKey(plan.Steps[0].Manifest)); n != repeatedRefusalLimit {
		t.Fatalf("seeded refusals = %d, want %d", n, repeatedRefusalLimit)
	}
	if _, _, err := l.RunOnce(context.Background(), "boutique"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := exec.AppliedCount(); got != 0 {
		t.Errorf("executor asked %d times for a step refused %d times before the restart", got, repeatedRefusalLimit)
	}
	if _, ok := au.find(audit.EventStepSkipped, "repeated-refusal"); !ok {
		t.Error("no step.skipped repeated-refusal")
	}
}

// The seeded history only holds the limit if a refusal read back from the
// audit trail keys the same as the step the planner sent. It is recorded at
// executor.received, before narrowing; anything lost in that round trip
// would silently reset the count on every restart.
func TestARefusalReadBackFromTheAuditTrailKeysAsTheStepDid(t *testing.T) {
	var plan simian.AttackPlan
	raw := `{"hypothesis":"x","steps":[{"order":1,"manifest":{"engine":"chaos-mesh","api_version":"chaos-mesh.org/v1alpha1","resource_kind":"IOChaos",` +
		`"spec":{"action":"latency","delay":"150ms","percent":100,"path":"/data/**","volumePath":"/data","selector":{"labelSelectors":{"app":"redis-cart"},"namespaces":["boutique"]}},` +
		`"targets":[{"namespace":"boutique","name":"redis-cart"}],"duration":"3m","blast_radius_tier":"namespace"}}]}`
	if err := json.Unmarshal([]byte(raw), &plan); err != nil {
		t.Fatal(err)
	}
	m := plan.Steps[0].Manifest
	m.UID, m.Source = "f-1", simian.SourceAutonomous

	// Through the file and back, as a restart reads it.
	var buf bytes.Buffer
	for _, e := range []simian.AuditEvent{
		{Event: audit.EventExecutorReceived, FaultUID: m.UID, Mode: m.Source, Payload: m.AuditRecord()},
		{Event: audit.EventExecutorRejected, FaultUID: m.UID, Mode: m.Source, Reason: string(simian.ReasonTargetIncompatible),
			Payload: map[string]any{"error": "read-only root filesystem"}},
	} {
		line, _ := json.Marshal(audit.Record{TS: time.Now(), Event: e.Event, FaultUID: e.FaultUID, Mode: string(e.Mode), Reason: e.Reason, Payload: e.Payload})
		buf.Write(append(line, '\n'))
	}
	recs, err := audit.ReadRecords(&buf)
	if err != nil {
		t.Fatal(err)
	}
	refused := executor.RefusalsFromAudit(audit.Faults(recs), time.Now().Add(-time.Hour))
	if len(refused) != 1 {
		t.Fatalf("refusals = %+v, want one", refused)
	}
	if got, want := stepKey(refused[0].Manifest), stepKey(plan.Steps[0].Manifest); got != want {
		t.Errorf("read back keys as\n%s\nwant\n%s", got, want)
	}
	if refused[0].Reason != simian.ReasonTargetIncompatible || refused[0].Error != "read-only root filesystem" {
		t.Errorf("refusal = %+v", refused[0])
	}
}

// blockingLLM answers once ctx is done, as a provider call cut off by
// shutdown does.
type blockingLLM struct{ entered chan struct{} }

func (b blockingLLM) Name() string { return "blocking" }
func (b blockingLLM) Complete(ctx context.Context, _ simian.CompletionRequest) (simian.CompletionResponse, error) {
	close(b.entered)
	<-ctx.Done()
	return simian.CompletionResponse{}, fmt.Errorf("doRequest: %w", ctx.Err())
}

// A shutdown mid-planning is not the LLM being unavailable. On the
// 2026-10-02 trial a rollout recorded it as llm-unavailable.
func TestAShutdownMidPlanningIsRecordedAsInterrupted(t *testing.T) {
	l, au := newLoopUnderTest(t, planJSON(1), &recordingExecutor{}, planner.Budget{
		MaxFaultsPerCycle: 5, MaxConcurrentFaults: 5, MaxSeverityPerCycle: simian.TierNamespace,
	})
	llm := blockingLLM{entered: make(chan struct{})}
	l.Generator = planner.NewGenerator(llm)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-llm.entered; cancel() }()
	if _, _, err := l.RunOnce(ctx, "boutique"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if _, ok := au.find(audit.EventCycleSkipped, string(simian.ReasonInterrupted)); !ok {
		t.Error("no cycle.skipped interrupted")
	}
	if _, ok := au.find(audit.EventCycleSkipped, "llm-unavailable"); ok {
		t.Error("a shutdown was recorded as llm-unavailable")
	}
	au.mu.Lock()
	defer au.mu.Unlock()
	for _, e := range au.events {
		if e.Event == audit.EventLLMUnavailable {
			t.Errorf("a shutdown was recorded as %s: %+v", e.Event, e)
		}
	}
}

// slowFirstLLM takes longer than the loop's interval on its first call only.
type slowFirstLLM struct {
	mu    sync.Mutex
	calls int
	delay time.Duration
	text  string
}

func (s *slowFirstLLM) Name() string { return "slow-first" }
func (s *slowFirstLLM) Complete(context.Context, simian.CompletionRequest) (simian.CompletionResponse, error) {
	s.mu.Lock()
	s.calls++
	first := s.calls == 1
	s.mu.Unlock()
	if first {
		time.Sleep(s.delay)
	}
	return simian.CompletionResponse{Text: s.text}, nil
}

// A cycle that outruns its interval does not start the next one the moment
// it ends, on the tick that fired meanwhile; it waits for the next tick.
func TestACycleThatOverrunsItsIntervalWaitsForTheNextTick(t *testing.T) {
	const interval = 200 * time.Millisecond
	l, au := newLoopUnderTest(t, planJSON(1), &recordingExecutor{}, planner.Budget{
		MaxFaultsPerCycle: 5, MaxConcurrentFaults: 5, MaxSeverityPerCycle: simian.TierNamespace,
	})
	l.Interval = interval
	l.Generator = planner.NewGenerator(&slowFirstLLM{delay: interval + interval/2, text: planJSON(1)})
	var (
		mu     sync.Mutex
		starts []time.Time
	)
	l.Auditor = auditFunc(func(e simian.AuditEvent) {
		au.Emit(context.Background(), e)
		if e.Event == audit.EventCycleStarted {
			mu.Lock()
			starts = append(starts, time.Now())
			mu.Unlock()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*interval)
	defer cancel()
	_ = l.Run(ctx)

	mu.Lock()
	defer mu.Unlock()
	if len(starts) < 2 {
		t.Fatalf("cycles started = %d, want 2", len(starts))
	}
	// The first cycle ends at 1.5 intervals; the stale tick at 1 is dropped
	// and the next cycle starts on the tick at 2.
	if gap := starts[1].Sub(starts[0]); gap < 2*interval-interval/4 {
		t.Errorf("second cycle started %s after the first, want about %s", gap, 2*interval)
	}
}

type auditFunc func(simian.AuditEvent)

func (f auditFunc) Emit(_ context.Context, e simian.AuditEvent) { f(e) }
