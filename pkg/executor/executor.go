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
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/catalog"
	"github.com/go-steer/simian-agent/pkg/lease"
	"github.com/go-steer/simian-agent/pkg/probe"
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
	prober   probe.Prober
	// defaultProbes supplies the gate for manifests that declare none. nil
	// leaves every fault gated only by what its author wrote.
	defaultProbes func(simian.FaultManifest) []simian.ProbeSpec

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

// WithProber wires the efficacy gate. Without it, a manifest carrying Settle
// probes is rejected rather than applied unverified — see Executor.settle.
func WithProber(p probe.Prober) Option {
	return func(e *Executor) { e.prober = p }
}

// WithDefaultProbes makes Simian attach its own efficacy gate to manifests
// that do not bring one — catalog.DefaultProbes in production.
//
// This is an operator decision, not a manifest one. A fault's author is the
// planner being evaluated against it; letting the manifest opt out of its own
// verification would put the harness's honesty in the hands of the thing the
// harness is measuring.
func WithDefaultProbes(fn func(simian.FaultManifest) []simian.ProbeSpec) Option {
	return func(e *Executor) { e.defaultProbes = fn }
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
	narrowed, err := e.validateSafety(ctx, &m)
	if err != nil {
		e.rejected(ctx, m, err)
		return "", err
	}

	// Attach Simian's own gate before anything reads m.Probes. Done after
	// narrowing so a default probe inherits the selector the driver will
	// actually be given, not the one the manifest arrived with.
	attached := e.attachDefaultProbes(&m)

	validated := simian.AuditEvent{
		Event:    audit.EventExecutorValidated,
		FaultUID: m.UID,
		PlanID:   m.PlanID,
		Mode:     m.Source,
	}
	if len(attached) > 0 || len(narrowed) > 0 {
		validated.Payload = map[string]any{}
	}
	if len(attached) > 0 {
		validated.Payload["default_probes"] = attached
	}
	if len(narrowed) > 0 {
		// The spec left a selector unscoped and we pinned it to the target
		// namespaces. Record it: the manifest that reaches the driver is no
		// longer byte-identical to the one that was submitted.
		validated.Payload["selectors_narrowed_to"] = targetNamespaces(m)
		validated.Payload["selector_paths"] = narrowed
	}
	e.auditor.Emit(ctx, validated)

	driver, ok := e.drivers[m.Engine]
	if !ok {
		err := simian.NewExecutorError(simian.StageDriver, simian.ReasonDriverFailed,
			fmt.Sprintf("no driver registered for engine %q", m.Engine), nil)
		e.rejected(ctx, m, err)
		return "", err
	}

	// Nothing has touched the cluster yet, which is the point: a fault whose
	// starting conditions do not hold is rejected outright rather than applied
	// and rolled back.
	if err := e.precheck(ctx, m); err != nil {
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

	// The lease is registered before the gate runs, deliberately. Everything
	// below can fail, and a fault the cluster has already accepted must be
	// reapable no matter which way it does.
	if err := e.settle(ctx, m); err != nil {
		e.abandon(ctx, m, engineUID, err)
		return "", err
	}

	if e.history != nil {
		e.history.Push(RecentFault{
			FaultUID:  m.UID,
			Manifest:  m,
			AppliedAt: now.UTC(),
		})
	}

	return m.UID, nil
}

// settle runs the manifest's Settle probes in order and returns once they have
// all passed, or at the first one that has not.
//
// The deadline is not extended to cover this wait. A fault that takes 30s to
// manifest spends 30s of its own duration doing so, which keeps Simian's lease
// and the engine's own server-side spec.duration in agreement — they would
// otherwise drift apart and the fault would outlive its lease. The cost shows
// up as elapsed_ms on the efficacy event rather than as a silent discrepancy.
func (e *Executor) settle(ctx context.Context, m simian.FaultManifest) error {
	return e.runProbes(ctx, m, m.SettleProbes(), probeStage{
		mode:   simian.ProbeModeSettle,
		event:  audit.EventFaultEfficacy,
		stage:  simian.StageProbe,
		reason: simian.ReasonProbeFailed,
	})
}

// precheck runs the manifest's SOT probes before the driver does anything.
//
// A Settle probe only means something relative to a starting state. "The
// workload does not answer" proves a partition landed exactly when the
// workload answered a moment ago and nothing else changed in between; against
// a workload that was already down it is a gate that passes for the wrong
// reason, which is indistinguishable from no gate at all.
func (e *Executor) precheck(ctx context.Context, m simian.FaultManifest) error {
	return e.runProbes(ctx, m, m.SOTProbes(), probeStage{
		mode:   simian.ProbeModeSOT,
		event:  audit.EventFaultPrecheck,
		stage:  simian.StagePrecheck,
		reason: simian.ReasonPrecheckFailed,
	})
}

// probeStage is the per-mode wiring shared by precheck and settle.
type probeStage struct {
	mode   string
	event  string
	stage  simian.ExecutorStage
	reason simian.RejectionReason
}

// runProbes runs one mode's probes in order, emitting an audit event per
// probe, and stops at the first that does not pass.
func (e *Executor) runProbes(ctx context.Context, m simian.FaultManifest, probes []simian.ProbeSpec, st probeStage) error {
	if len(probes) == 0 {
		return nil
	}
	if e.prober == nil {
		// Loud, not skipped. Treating an unrunnable gate as a passing one
		// would mark unverified faults verified, which is the failure this
		// whole mechanism exists to prevent.
		return simian.NewExecutorError(st.stage, simian.ReasonProbeNotConfigured,
			fmt.Sprintf("manifest declares %d %s probe(s) but the executor has no prober wired in", len(probes), st.mode), nil)
	}

	target := probe.Target{}
	if len(m.Targets) > 0 {
		target.Namespace = m.Targets[0].Namespace
		target.Labels = m.Targets[0].Labels
	}
	for _, p := range probes {
		res := e.prober.Run(ctx, p, target)
		payload := map[string]any{
			"probe":      res.Name,
			"type":       res.Type,
			"mode":       st.mode,
			"passed":     res.Passed,
			"observed":   res.Observed,
			"expected":   res.Expected,
			"attempts":   res.Attempts,
			"elapsed_ms": res.Elapsed.Milliseconds(),
		}
		reason := ""
		if !res.Passed {
			reason = string(st.reason)
		}
		if res.Err != nil {
			payload["error"] = res.Err.Error()
		}
		e.auditor.Emit(ctx, simian.AuditEvent{
			Event:    st.event,
			FaultUID: m.UID,
			PlanID:   m.PlanID,
			Mode:     m.Source,
			Reason:   reason,
			Payload:  payload,
		})
		if !res.Passed {
			return simian.NewExecutorError(st.stage, st.reason, res.Describe(), res.Err)
		}
	}
	return nil
}

// attachDefaultProbes appends Simian's own gate to m and returns the names it
// added, for the audit record. No-op when no default source is wired.
func (e *Executor) attachDefaultProbes(m *simian.FaultManifest) []string {
	if e.defaultProbes == nil {
		return nil
	}
	defaults := e.defaultProbes(*m)
	if len(defaults) == 0 {
		return nil
	}
	names := make([]string, 0, len(defaults))
	for _, p := range defaults {
		names = append(names, p.Name)
	}
	m.Probes = append(m.Probes, defaults...)
	return names
}

// abandon backs out a fault that was applied but never manifested.
//
// An unverified fault is not a valid experiment, and leaving it running would
// contaminate the next one while the caller — holding an error and no UID —
// has no way to clear it. If the clear itself fails the lease is left in place
// on purpose, so the reaper collects it at the deadline instead.
func (e *Executor) abandon(ctx context.Context, m simian.FaultManifest, engineUID string, cause error) {
	reason := string(simian.ReasonProbeFailed)
	payload := map[string]any{
		"engine_uid": engineUID,
		"error":      cause.Error(),
	}
	if driver, ok := e.drivers[m.Engine]; ok {
		if err := driver.Clear(ctx, engineUID); err != nil {
			payload["clear_error"] = err.Error()
			payload["left_to_reaper"] = true
		} else {
			_ = e.registry.Forget(m.UID)
		}
	} else {
		payload["clear_error"] = fmt.Sprintf("no driver registered for engine %q", m.Engine)
		payload["left_to_reaper"] = true
	}
	e.auditor.Emit(ctx, simian.AuditEvent{
		Event:    audit.EventLeaseCleared,
		FaultUID: m.UID,
		PlanID:   m.PlanID,
		Mode:     m.Source,
		Reason:   reason,
		Payload:  payload,
	})
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

// validateSafety applies every safety gate to the manifest. It may mutate m:
// the blast-radius tier is classified here, and an unscoped spec selector is
// narrowed to the target namespaces. It returns the dotted spec paths that
// were narrowed, if any.
func (e *Executor) validateSafety(ctx context.Context, m *simian.FaultManifest) ([]string, error) {
	// Duration ceiling.
	if m.Duration <= 0 {
		return nil, simian.NewExecutorError(simian.StageSafety, simian.ReasonDurationOverCeiling,
			"duration must be positive", nil)
	}
	if e.cfg.DurationCeiling > 0 && m.Duration > e.cfg.DurationCeiling {
		return nil, simian.NewExecutorError(simian.StageSafety, simian.ReasonDurationOverCeiling,
			fmt.Sprintf("duration %s exceeds ceiling %s", m.Duration, e.cfg.DurationCeiling), nil)
	}

	// Namespace eligibility.
	for _, t := range m.Targets {
		if t.Namespace == "" {
			return nil, simian.NewExecutorError(simian.StageSafety, simian.ReasonNamespaceNotEligible,
				"target namespace is empty", nil)
		}
		ok, err := e.elig.IsEligible(ctx, t.Namespace)
		if err != nil {
			return nil, simian.NewExecutorError(simian.StageSafety, simian.ReasonNamespaceNotEligible,
				"eligibility lookup failed", err)
		}
		if !ok {
			return nil, simian.NewExecutorError(simian.StageSafety, simian.ReasonNamespaceNotEligible,
				fmt.Sprintf("namespace %q is not eligible for chaos", t.Namespace), nil)
		}
		excluded, err := e.elig.ExcludedWorkloads(ctx, t.Namespace)
		if err != nil {
			return nil, simian.NewExecutorError(simian.StageSafety, simian.ReasonWorkloadExcluded,
				"exclusion lookup failed", err)
		}
		if t.Name != "" && slices.Contains(excluded, t.Name) {
			return nil, simian.NewExecutorError(simian.StageSafety, simian.ReasonWorkloadExcluded,
				fmt.Sprintf("workload %q in namespace %q is excluded", t.Name, t.Namespace), nil)
		}
	}

	// Spec-level namespace scope. Targets alone do not bound a Chaos Mesh
	// fault — the PodSelector inside the spec does. Must run after the target
	// loop above, which is what makes the narrowing target trustworthy.
	narrowed, err := e.validateSpecNamespaceScope(ctx, m)
	if err != nil {
		return nil, err
	}

	// Blast-radius classification + tier policy.
	tier := catalog.ReclassifyForSpec(*m, e.cfg.AllCIDRs(), e.cfg.ClusterDomains)
	m.BlastRadiusTier = tier
	if !e.cfg.PermittedTiers[tier] {
		return nil, simian.NewExecutorError(simian.StageSafety, simian.ReasonTierNotPermitted,
			fmt.Sprintf("blast tier %q is not permitted by current policy", tier), nil)
	}

	// Concurrency budget.
	if e.cfg.MaxConcurrentFaults > 0 {
		if active := e.registry.List(""); len(active) >= e.cfg.MaxConcurrentFaults {
			return nil, simian.NewExecutorError(simian.StageSafety, simian.ReasonBudgetExceeded,
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
			return nil, simian.NewExecutorError(simian.StageSafety, simian.ReasonBudgetExceeded,
				fmt.Sprintf("namespace %q is in cooldown", ns), nil)
		}
	}

	return narrowed, nil
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
	var ee *simian.ExecutorError
	if errors.As(err, &ee) {
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
