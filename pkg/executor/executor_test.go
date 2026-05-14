package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-steer/simian-agent/internal/testutil"
	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/lease"
	"github.com/go-steer/simian-agent/pkg/simian"
)

func newTestExecutor(t *testing.T, cfg Config, eligible map[string]bool, exclusions map[string][]string) (*Executor, *testutil.FakeDriver, *testutil.FakeAuditor) {
	t.Helper()
	driver := &testutil.FakeDriver{EngineName: simian.EngineChaosMesh}
	registry := lease.NewRegistry("test-holder")
	auditor := &testutil.FakeAuditor{}
	elig := &StaticEligibility{Eligible: eligible, Exclusions: exclusions}
	exec := New(cfg, map[simian.Engine]simian.ChaosDriver{simian.EngineChaosMesh: driver}, registry, auditor, elig)
	return exec, driver, auditor
}

func goodManifest() simian.FaultManifest {
	return simian.FaultManifest{
		Source:       simian.SourceDirected,
		Engine:       simian.EngineChaosMesh,
		APIVersion:   "chaos-mesh.org/v1alpha1",
		ResourceKind: "NetworkChaos",
		Spec:         map[string]any{"action": "delay", "delay": map[string]any{"latency": "250ms"}},
		Targets:      []simian.TargetRef{{Namespace: "online-boutique", Name: "paymentservice"}},
		Duration:     2 * time.Minute,
	}
}

func TestApplyHappyPath(t *testing.T) {
	exec, driver, auditor := newTestExecutor(t, DefaultConfig(), map[string]bool{"online-boutique": true}, nil)
	uid, err := exec.Apply(context.Background(), goodManifest())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if uid == "" {
		t.Fatal("expected fault UID")
	}
	if got := len(driver.AppliedCopy()); got != 1 {
		t.Fatalf("driver.Applied=%d, want 1", got)
	}
	wantEvents := []string{
		audit.EventExecutorReceived,
		audit.EventExecutorValidated,
		audit.EventDriverApplied,
		audit.EventLeaseRegistered,
	}
	for _, ev := range wantEvents {
		if _, ok := auditor.FindEvent(ev); !ok {
			t.Errorf("missing audit event %q", ev)
		}
	}
}

func TestApplyRejectsIneligibleNamespace(t *testing.T) {
	exec, driver, auditor := newTestExecutor(t, DefaultConfig(), map[string]bool{"online-boutique": true}, nil)
	m := goodManifest()
	m.Targets[0].Namespace = "kube-system"

	_, err := exec.Apply(context.Background(), m)
	if err == nil {
		t.Fatal("expected rejection")
	}
	var ee *simian.ExecutorError
	if !errors.As(err, &ee) {
		t.Fatalf("expected *ExecutorError, got %T", err)
	}
	if ee.Reason != simian.ReasonNamespaceNotEligible {
		t.Errorf("reason=%s, want %s", ee.Reason, simian.ReasonNamespaceNotEligible)
	}
	if got := len(driver.AppliedCopy()); got != 0 {
		t.Errorf("driver.Applied=%d, want 0 (rejection should never reach driver)", got)
	}
	if _, ok := auditor.FindEvent(audit.EventExecutorRejected); !ok {
		t.Error("expected executor.rejected audit event")
	}
}

func TestApplyRejectsExcludedWorkload(t *testing.T) {
	exec, _, _ := newTestExecutor(t,
		DefaultConfig(),
		map[string]bool{"online-boutique": true},
		map[string][]string{"online-boutique": {"paymentservice"}},
	)
	_, err := exec.Apply(context.Background(), goodManifest())
	if err == nil {
		t.Fatal("expected rejection")
	}
	var ee *simian.ExecutorError
	if !errors.As(err, &ee) || ee.Reason != simian.ReasonWorkloadExcluded {
		t.Fatalf("expected workload-excluded, got %v", err)
	}
}

func TestApplyRejectsDurationOverCeiling(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DurationCeiling = 1 * time.Minute
	exec, _, _ := newTestExecutor(t, cfg, map[string]bool{"online-boutique": true}, nil)

	m := goodManifest()
	m.Duration = 5 * time.Minute
	_, err := exec.Apply(context.Background(), m)
	var ee *simian.ExecutorError
	if !errors.As(err, &ee) || ee.Reason != simian.ReasonDurationOverCeiling {
		t.Fatalf("expected duration-over-ceiling, got %v", err)
	}
}

func TestApplyRejectsTierNotPermitted(t *testing.T) {
	cfg := DefaultConfig()
	// Strip node + external from the policy; only namespace tier permitted.
	cfg.PermittedTiers = map[simian.BlastRadiusTier]bool{simian.TierNamespace: true}
	exec, _, _ := newTestExecutor(t, cfg, map[string]bool{"online-boutique": true}, nil)

	m := goodManifest()
	m.ResourceKind = "KernelChaos" // node tier
	_, err := exec.Apply(context.Background(), m)
	var ee *simian.ExecutorError
	if !errors.As(err, &ee) || ee.Reason != simian.ReasonTierNotPermitted {
		t.Fatalf("expected tier-not-permitted, got %v", err)
	}
}

func TestClearForgetsAndCallsDriver(t *testing.T) {
	exec, driver, auditor := newTestExecutor(t, DefaultConfig(), map[string]bool{"online-boutique": true}, nil)
	uid, err := exec.Apply(context.Background(), goodManifest())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := exec.Clear(context.Background(), uid); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if got := len(driver.Cleared); got != 1 {
		t.Errorf("driver.Cleared=%d, want 1", got)
	}
	if _, ok := auditor.FindEvent(audit.EventLeaseCleared); !ok {
		t.Error("expected lease.cleared audit event")
	}
	active, _ := exec.ListActive(context.Background(), "")
	if got := len(active); got != 0 {
		t.Errorf("ListActive=%d, want 0 after clear", got)
	}
}

func newTestExecutorWithHistory(t *testing.T, cfg Config, eligible map[string]bool, exclusions map[string][]string) (*Executor, *testutil.FakeDriver, *History) {
	t.Helper()
	driver := &testutil.FakeDriver{EngineName: simian.EngineChaosMesh}
	registry := lease.NewRegistry("test-holder")
	auditor := &testutil.FakeAuditor{}
	elig := &StaticEligibility{Eligible: eligible, Exclusions: exclusions}
	hist := NewHistory(0)
	exec := New(cfg, map[simian.Engine]simian.ChaosDriver{simian.EngineChaosMesh: driver}, registry, auditor, elig, WithHistory(hist))
	return exec, driver, hist
}

func TestApplyPushesHistoryEntry(t *testing.T) {
	exec, _, hist := newTestExecutorWithHistory(t, DefaultConfig(), map[string]bool{"online-boutique": true}, nil)
	uid, err := exec.Apply(context.Background(), goodManifest())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := hist.List("", 0)
	if len(got) != 1 {
		t.Fatalf("History len=%d, want 1", len(got))
	}
	if got[0].FaultUID != uid {
		t.Errorf("history UID=%q, want %q", got[0].FaultUID, uid)
	}
	if !got[0].ClearedAt.IsZero() {
		t.Errorf("expected ClearedAt zero on fresh entry, got %v", got[0].ClearedAt)
	}
	if got := exec.Recent("", 0); len(got) != 1 {
		t.Errorf("Executor.Recent len=%d, want 1", len(got))
	}
}

func TestClearUpdatesHistory(t *testing.T) {
	exec, _, hist := newTestExecutorWithHistory(t, DefaultConfig(), map[string]bool{"online-boutique": true}, nil)
	uid, err := exec.Apply(context.Background(), goodManifest())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := exec.Clear(context.Background(), uid); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	got := hist.List("", 0)
	if len(got) != 1 {
		t.Fatalf("History len=%d, want 1", len(got))
	}
	if got[0].ClearReason != "explicit-clear" {
		t.Errorf("ClearReason=%q, want explicit-clear", got[0].ClearReason)
	}
	if got[0].ClearedAt.IsZero() {
		t.Errorf("ClearedAt should be set after Clear")
	}
}

func TestRecentReturnsNilWithoutHistory(t *testing.T) {
	exec, _, _ := newTestExecutor(t, DefaultConfig(), map[string]bool{"online-boutique": true}, nil)
	if got := exec.Recent("", 0); got != nil {
		t.Errorf("Recent without history wired = %v, want nil", got)
	}
}
