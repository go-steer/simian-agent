package loop

import (
	"context"
	"errors"
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
