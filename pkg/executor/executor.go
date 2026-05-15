package executor

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/catalog"
	"github.com/go-steer/simian-agent/pkg/lease"
	"github.com/go-steer/simian-agent/pkg/simian"
)

// Executor implements simian.FaultExecutor. It is the only code path that
// invokes a chaos driver; all safety policy, audit, and lifecycle live here.
type Executor struct {
	cfg      Config
	drivers  map[simian.Engine]simian.ChaosDriver
	registry *lease.Registry
	auditor  simian.Auditor
	elig     EligibilityChecker
	history  *History // optional; nil disables get_recent_faults backing

	mu            sync.Mutex
	lastApplyByNS map[string]time.Time
}

// Option configures an Executor at construction time.
type Option func(*Executor)

// WithHistory wires a recent-faults history buffer into the Executor. Apply
// pushes a RecentFault on success; Clear updates ClearedAt with reason
// "explicit-clear". Reaper-driven clears must call History.UpdateCleared
// directly (wired in cmd/simian/serve.go via lease.Reaper.OnExpire).
func WithHistory(h *History) Option {
	return func(e *Executor) { e.history = h }
}

// New constructs an Executor.
func New(cfg Config, drivers map[simian.Engine]simian.ChaosDriver, registry *lease.Registry, auditor simian.Auditor, elig EligibilityChecker, opts ...Option) *Executor {
	e := &Executor{
		cfg:           cfg,
		drivers:       drivers,
		registry:      registry,
		auditor:       auditor,
		elig:          elig,
		lastApplyByNS: map[string]time.Time{},
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Recent returns a slice of recently-handled faults, optionally filtered by
// namespace. Returns nil if no history buffer is wired.
func (e *Executor) Recent(namespace string, limit int) []RecentFault {
	if e.history == nil {
		return nil
	}
	return e.history.List(namespace, limit)
}

// History returns the underlying buffer (may be nil). Exposed so the reaper
// callback in serve.go can update entries on deadline-driven clears without
// importing the executor's internals.
func (e *Executor) History() *History { return e.history }

// Apply runs the full executor pipeline.
func (e *Executor) Apply(ctx context.Context, m simian.FaultManifest) (string, error) {
	if m.UID == "" {
		m.UID = newFaultUID()
	}
	e.auditor.Emit(ctx, simian.AuditEvent{
		Event:    audit.EventExecutorReceived,
		FaultUID: m.UID,
		PlanID:   m.PlanID,
		Mode:     m.Source,
		Payload: map[string]any{
			"engine": m.Engine,
			"kind":   m.ResourceKind,
		},
	})

	if err := e.validateSchema(m); err != nil {
		e.rejected(ctx, m, err)
		return "", err
	}
	if err := e.validateSafety(ctx, &m); err != nil {
		e.rejected(ctx, m, err)
		return "", err
	}

	e.auditor.Emit(ctx, simian.AuditEvent{
		Event:    audit.EventExecutorValidated,
		FaultUID: m.UID,
		PlanID:   m.PlanID,
		Mode:     m.Source,
	})

	driver, ok := e.drivers[m.Engine]
	if !ok {
		err := simian.NewExecutorError(simian.StageDriver, simian.ReasonDriverFailed,
			fmt.Sprintf("no driver registered for engine %q", m.Engine), nil)
		e.rejected(ctx, m, err)
		return "", err
	}

	engineUID, applyErr := driver.Apply(ctx, m)
	if applyErr != nil {
		err := simian.NewExecutorError(simian.StageDriver, simian.ReasonDriverFailed,
			"driver apply failed", applyErr)
		e.auditor.Emit(ctx, simian.AuditEvent{
			Event:    audit.EventDriverFailed,
			FaultUID: m.UID,
			PlanID:   m.PlanID,
			Mode:     m.Source,
			Reason:   string(simian.ReasonDriverFailed),
			Payload:  map[string]any{"error": applyErr.Error()},
		})
		return "", err
	}

	now := time.Now()
	deadline := now.Add(m.Duration)
	e.registry.Register(m.UID, engineUID, m, deadline)
	e.recordApply(m)
	if e.history != nil {
		e.history.Push(RecentFault{
			FaultUID:  m.UID,
			Manifest:  m,
			AppliedAt: now.UTC(),
		})
	}

	e.auditor.Emit(ctx, simian.AuditEvent{
		Event:    audit.EventDriverApplied,
		FaultUID: m.UID,
		PlanID:   m.PlanID,
		Mode:     m.Source,
		Payload: map[string]any{
			"engine_uid": engineUID,
			"deadline":   deadline.UTC().Format(time.RFC3339),
		},
	})
	e.auditor.Emit(ctx, simian.AuditEvent{
		Event:    audit.EventLeaseRegistered,
		FaultUID: m.UID,
		PlanID:   m.PlanID,
		Mode:     m.Source,
	})

	return m.UID, nil
}

// Clear removes an active fault before its lease expires.
func (e *Executor) Clear(ctx context.Context, faultUID string) error {
	af, ok := e.registry.Get(faultUID)
	if !ok {
		return fmt.Errorf("fault %q not found", faultUID)
	}
	driver, ok := e.drivers[af.Manifest.Engine]
	if !ok {
		return fmt.Errorf("no driver for engine %q", af.Manifest.Engine)
	}
	if err := driver.Clear(ctx, af.EngineUID); err != nil {
		return err
	}
	_ = e.registry.Forget(faultUID)
	if e.history != nil {
		e.history.UpdateCleared(faultUID, time.Now().UTC(), "explicit-clear")
	}
	e.auditor.Emit(ctx, simian.AuditEvent{
		Event:    audit.EventLeaseCleared,
		FaultUID: faultUID,
		Reason:   "explicit-clear",
	})
	return nil
}

// ListActive returns currently leased faults, optionally filtered by namespace.
func (e *Executor) ListActive(_ context.Context, namespace string) ([]simian.ActiveFault, error) {
	return e.registry.List(namespace), nil
}

// validateSchema is intentionally light in M1 — full CRD OpenAPI validation
// against discovered schemas lands when catalog discovery surfaces them.
func (e *Executor) validateSchema(m simian.FaultManifest) error {
	if m.Engine == "" {
		return simian.NewExecutorError(simian.StageSchema, simian.ReasonSchemaInvalid,
			"engine is required", nil)
	}
	if m.ResourceKind == "" {
		return simian.NewExecutorError(simian.StageSchema, simian.ReasonSchemaInvalid,
			"resource_kind is required", nil)
	}
	if m.APIVersion == "" {
		return simian.NewExecutorError(simian.StageSchema, simian.ReasonSchemaInvalid,
			"api_version is required", nil)
	}
	if len(m.Targets) == 0 {
		return simian.NewExecutorError(simian.StageSchema, simian.ReasonSchemaInvalid,
			"manifest must declare at least one target", nil)
	}
	if m.Spec == nil {
		return simian.NewExecutorError(simian.StageSchema, simian.ReasonSchemaInvalid,
			"spec is required", nil)
	}
	return nil
}

func (e *Executor) validateSafety(ctx context.Context, m *simian.FaultManifest) error {
	// Duration ceiling.
	if m.Duration <= 0 {
		return simian.NewExecutorError(simian.StageSafety, simian.ReasonDurationOverCeiling,
			"duration must be positive", nil)
	}
	if e.cfg.DurationCeiling > 0 && m.Duration > e.cfg.DurationCeiling {
		return simian.NewExecutorError(simian.StageSafety, simian.ReasonDurationOverCeiling,
			fmt.Sprintf("duration %s exceeds ceiling %s", m.Duration, e.cfg.DurationCeiling), nil)
	}

	// Namespace eligibility.
	for _, t := range m.Targets {
		if t.Namespace == "" {
			return simian.NewExecutorError(simian.StageSafety, simian.ReasonNamespaceNotEligible,
				"target namespace is empty", nil)
		}
		ok, err := e.elig.IsEligible(ctx, t.Namespace)
		if err != nil {
			return simian.NewExecutorError(simian.StageSafety, simian.ReasonNamespaceNotEligible,
				"eligibility lookup failed", err)
		}
		if !ok {
			return simian.NewExecutorError(simian.StageSafety, simian.ReasonNamespaceNotEligible,
				fmt.Sprintf("namespace %q is not eligible for chaos", t.Namespace), nil)
		}
		excluded, err := e.elig.ExcludedWorkloads(ctx, t.Namespace)
		if err != nil {
			return simian.NewExecutorError(simian.StageSafety, simian.ReasonWorkloadExcluded,
				"exclusion lookup failed", err)
		}
		if t.Name != "" && slices.Contains(excluded, t.Name) {
			return simian.NewExecutorError(simian.StageSafety, simian.ReasonWorkloadExcluded,
				fmt.Sprintf("workload %q in namespace %q is excluded", t.Name, t.Namespace), nil)
		}
	}

	// Blast-radius classification + tier policy.
	tier := catalog.ReclassifyForSpec(*m, e.cfg.AllCIDRs(), e.cfg.ClusterDomains)
	m.BlastRadiusTier = tier
	if !e.cfg.PermittedTiers[tier] {
		return simian.NewExecutorError(simian.StageSafety, simian.ReasonTierNotPermitted,
			fmt.Sprintf("blast tier %q is not permitted by current policy", tier), nil)
	}

	// Concurrency budget.
	if e.cfg.MaxConcurrentFaults > 0 {
		if active := e.registry.List(""); len(active) >= e.cfg.MaxConcurrentFaults {
			return simian.NewExecutorError(simian.StageSafety, simian.ReasonBudgetExceeded,
				fmt.Sprintf("max concurrent faults reached (%d)", e.cfg.MaxConcurrentFaults), nil)
		}
	}

	// Per-namespace cooldown.
	if e.cfg.MinCooldown > 0 {
		ns := m.Targets[0].Namespace
		e.mu.Lock()
		last, ok := e.lastApplyByNS[ns]
		e.mu.Unlock()
		if ok && time.Since(last) < e.cfg.MinCooldown {
			return simian.NewExecutorError(simian.StageSafety, simian.ReasonBudgetExceeded,
				fmt.Sprintf("namespace %q is in cooldown", ns), nil)
		}
	}

	return nil
}

func (e *Executor) recordApply(m simian.FaultManifest) {
	if len(m.Targets) == 0 {
		return
	}
	e.mu.Lock()
	e.lastApplyByNS[m.Targets[0].Namespace] = time.Now()
	e.mu.Unlock()
}

func (e *Executor) rejected(ctx context.Context, m simian.FaultManifest, err error) {
	reason := ""
	if ee, ok := err.(*simian.ExecutorError); ok {
		reason = string(ee.Reason)
	}
	e.auditor.Emit(ctx, simian.AuditEvent{
		Event:    audit.EventExecutorRejected,
		FaultUID: m.UID,
		PlanID:   m.PlanID,
		Mode:     m.Source,
		Reason:   reason,
		Payload:  map[string]any{"error": err.Error()},
	})
}

func newFaultUID() string {
	return "f-" + ulid.Make().String()
}
