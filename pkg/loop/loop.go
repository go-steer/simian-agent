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
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/executor"
	"github.com/go-steer/simian-agent/pkg/planner"
	"github.com/go-steer/simian-agent/pkg/simian"
)

// RecentLookup mirrors mcp.RecentLookup so the loop can read its own
// recent-faults context without importing pkg/mcp (which would create a
// loop → mcp → loop cycle through serve.go's wiring).
type RecentLookup interface {
	Recent(namespace string, limit int) []executor.RecentFault
}

// RefusalLookup is the optional half of the executor's history: what it would
// not run. Checked for on Recents, so wiring the executor there brings both.
type RefusalLookup interface {
	Refused(namespace string, limit int) []executor.RefusedFault
}

// A step refused this many times inside the window is not submitted again
// until the window has moved past the oldest refusal. The planner is told
// about refusals and usually stops; this is for when it does not, so the
// executor is not asked the same question hundreds of times.
const (
	repeatedRefusalLimit  = 3
	repeatedRefusalWindow = time.Hour
)

// CatalogFunc returns the currently-permitted fault catalog. Typically a
// thin wrapper over the executor's gatherCatalog helper.
type CatalogFunc func(ctx context.Context) ([]simian.CatalogEntry, error)

// Loop runs the autonomous-mode planning cycle on a tick.
type Loop struct {
	Namespaces []string
	Interval   time.Duration

	Generator *planner.Generator
	Executor  simian.FaultExecutor
	Topology  TopologySnapshotter
	Baselines BaselineLookup
	Recents   RecentLookup
	Catalog   CatalogFunc
	Health    HealthGate
	Budget    planner.Budget

	Auditor    simian.Auditor
	Logger     *slog.Logger
	Hypothesis string

	refusalMu sync.Mutex
	refusals  map[string][]time.Time // step key → recent refusal times

	// slotPoll is how often a namespace waiting for a fault slot looks
	// again. Zero means defaultSlotPoll; tests shorten it.
	slotPoll time.Duration
}

const defaultSlotPoll = 5 * time.Second

// Run drives the loop on a ticker until ctx is done. Returns the context
// error on shutdown.
func (l *Loop) Run(ctx context.Context) error {
	if l.Interval <= 0 {
		return fmt.Errorf("loop: interval must be positive")
	}
	if len(l.Namespaces) == 0 {
		return fmt.Errorf("loop: at least one namespace is required")
	}
	t := time.NewTicker(l.Interval)
	defer t.Stop()
	// Run an immediate first cycle on startup so operators don't wait a
	// full interval to see anything.
	for cycle := 0; ; cycle++ {
		for _, ns := range l.cycleOrder(cycle) {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			l.runOneSafely(ctx, ns)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// cycleOrder rotates which namespace goes first. Under a concurrency cap the
// first namespace of a cycle takes the slot; always starting with the same one
// left the others refused cycle after cycle (#161).
func (l *Loop) cycleOrder(cycle int) []string {
	n := len(l.Namespaces)
	order := make([]string, 0, n)
	for i := range n {
		order = append(order, l.Namespaces[(cycle+i)%n])
	}
	return order
}

// waitForSlot holds a namespace's turn until the concurrency cap has room,
// for at most the namespace's share of the cycle interval. It returns false,
// with the active count, when no slot opened in time; the caller skips
// without asking the planner for a plan the executor would refuse. A wait
// that cannot succeed — every active fault runs past the window — ends at
// once.
func (l *Loop) waitForSlot(ctx context.Context) (ok bool, active int, waited time.Duration) {
	limit := l.Budget.MaxConcurrentFaults
	if limit <= 0 || l.Executor == nil {
		return true, 0, 0
	}
	var window time.Duration
	if l.Interval > 0 && len(l.Namespaces) > 0 {
		window = l.Interval / time.Duration(len(l.Namespaces))
	}
	poll := l.slotPoll
	if poll <= 0 {
		poll = defaultSlotPoll
	}
	start := time.Now()
	end := start.Add(window)
	for {
		faults, err := l.Executor.ListActive(ctx, "")
		if err != nil || len(faults) < limit {
			// An unreadable registry is the executor's call to make at apply.
			return true, len(faults), time.Since(start)
		}
		freesInTime := false
		for _, f := range faults {
			if f.Deadline.IsZero() || f.Deadline.Before(end) {
				freesInTime = true
				break
			}
		}
		if !freesInTime || !time.Now().Add(poll).Before(end) {
			return false, len(faults), time.Since(start)
		}
		select {
		case <-ctx.Done():
			return false, len(faults), time.Since(start)
		case <-time.After(poll):
		}
	}
}

func (l *Loop) runOneSafely(ctx context.Context, ns string) {
	defer func() {
		if r := recover(); r != nil {
			if l.Logger != nil {
				l.Logger.Error("loop: cycle panicked", slog.String("namespace", ns), slog.Any("recover", r))
			}
		}
	}()
	if _, _, err := l.RunOnce(ctx, ns); err != nil && l.Logger != nil {
		l.Logger.Warn("loop: cycle ended with error", slog.String("namespace", ns), slog.String("err", err.Error()))
	}
}

// RunOnce drives a single planning cycle for the given namespace. Returns
// the generated plan, the slice of fault UIDs successfully applied, and an
// error only when the cycle could not even start (catalog gather failure,
// generator setup error). Health-gate failures, a full concurrency budget and
// LLM unavailability are treated as benign skips: the plan is empty, applied
// is nil, error is nil.
func (l *Loop) RunOnce(ctx context.Context, ns string) (simian.AttackPlan, []string, error) {
	if l.Auditor != nil {
		l.Auditor.Emit(ctx, simian.AuditEvent{Event: audit.EventCycleStarted, Mode: simian.SourceAutonomous, Payload: map[string]any{"namespace": ns}})
	}

	if ok, active, waited := l.waitForSlot(ctx); !ok {
		if l.Auditor != nil {
			l.Auditor.Emit(ctx, simian.AuditEvent{
				Event:  audit.EventCycleSkipped,
				Mode:   simian.SourceAutonomous,
				Reason: "budget-full",
				Payload: map[string]any{
					"namespace":             ns,
					"active_faults":         active,
					"max_concurrent_faults": l.Budget.MaxConcurrentFaults,
					"waited":                waited.Truncate(time.Second).String(),
				},
			})
		}
		return simian.AttackPlan{}, nil, nil
	}

	if l.Health != nil {
		if err := l.Health.Check(ctx, ns); err != nil {
			if l.Auditor != nil {
				l.Auditor.Emit(ctx, simian.AuditEvent{
					Event:   audit.EventHealthGateFailed,
					Mode:    simian.SourceAutonomous,
					Reason:  err.Error(),
					Payload: map[string]any{"namespace": ns},
				})
				l.Auditor.Emit(ctx, simian.AuditEvent{Event: audit.EventCycleSkipped, Mode: simian.SourceAutonomous, Reason: "health-gate", Payload: map[string]any{"namespace": ns}})
			}
			return simian.AttackPlan{}, nil, nil
		}
	}

	cat, err := l.Catalog(ctx)
	if err != nil {
		return simian.AttackPlan{}, nil, fmt.Errorf("catalog: %w", err)
	}

	in := planner.GenerateInput{
		Namespace:  ns,
		Catalog:    cat,
		Budget:     l.Budget,
		Hypothesis: l.Hypothesis,
	}
	if l.Topology != nil {
		if snap, terr := l.Topology.Snapshot(ctx, ns); terr == nil {
			in.Topology = snap
		}
	}
	if l.Baselines != nil {
		if bl, ok := l.Baselines.Baseline(ns); ok {
			in.Baseline = &bl
		}
	}
	if l.Recents != nil {
		if rs := l.Recents.Recent(ns, 10); len(rs) > 0 {
			in.RecentFaults = rs
		}
		if rl, ok := l.Recents.(RefusalLookup); ok {
			if rs := rl.Refused(ns, 10); len(rs) > 0 {
				in.RecentRefusals = rs
			}
		}
	}

	plan, err := l.Generator.Generate(ctx, in)
	if err != nil {
		if l.Auditor != nil {
			l.Auditor.Emit(ctx, simian.AuditEvent{
				Event:   audit.EventLLMUnavailable,
				Mode:    simian.SourceAutonomous,
				Reason:  err.Error(),
				Payload: map[string]any{"namespace": ns},
			})
			l.Auditor.Emit(ctx, simian.AuditEvent{Event: audit.EventCycleSkipped, Mode: simian.SourceAutonomous, Reason: "llm-unavailable", Payload: map[string]any{"namespace": ns}})
		}
		return simian.AttackPlan{}, nil, nil
	}

	if l.Auditor != nil {
		l.Auditor.Emit(ctx, simian.AuditEvent{
			Event:  audit.EventPlanGenerated,
			PlanID: plan.PlanID,
			Mode:   simian.SourceAutonomous,
			Payload: map[string]any{
				"namespace":  ns,
				"step_count": len(plan.Steps),
				"hypothesis": plan.Hypothesis,
				"steps":      planStepRecords(plan.Steps),
			},
		})
	}

	applied := l.executePlan(ctx, ns, plan)

	if l.Auditor != nil {
		l.Auditor.Emit(ctx, simian.AuditEvent{
			Event:  audit.EventCycleCompleted,
			PlanID: plan.PlanID,
			Mode:   simian.SourceAutonomous,
			Payload: map[string]any{
				"namespace":     ns,
				"applied_count": len(applied),
				"applied_uids":  applied,
			},
		})
	}
	return plan, applied, nil
}

// executePlan walks the plan's topological layers and applies steps under
// the loop's budget caps. Within a layer, steps run in parallel up to
// MaxConcurrentFaults; when MaxConcurrentFaults=1 the fan-out collapses
// to serial. Steps whose blast tier exceeds MaxSeverityPerCycle are
// skipped. Per-step Apply errors are audited but do not abort siblings.
func (l *Loop) executePlan(ctx context.Context, ns string, plan simian.AttackPlan) []string {
	layers, err := planner.PlanLayers(plan.Steps)
	if err != nil {
		if l.Auditor != nil {
			l.Auditor.Emit(ctx, simian.AuditEvent{
				Event:   audit.EventCycleSkipped,
				PlanID:  plan.PlanID,
				Mode:    simian.SourceAutonomous,
				Reason:  "plan-layering-failed",
				Payload: map[string]any{"namespace": ns, "error": err.Error()},
			})
		}
		return nil
	}

	maxCycle := l.Budget.MaxFaultsPerCycle
	if maxCycle <= 0 {
		maxCycle = len(plan.Steps)
	}
	maxConc := l.Budget.MaxConcurrentFaults
	if maxConc <= 0 {
		maxConc = 1
	}
	maxTier := l.Budget.MaxSeverityPerCycle

	stepsByOrder := make(map[int]simian.PlanStep, len(plan.Steps))
	for _, s := range plan.Steps {
		stepsByOrder[s.Order] = s
	}

	var (
		applied   []string
		scheduled int
		mu        sync.Mutex
	)

	for _, layer := range layers {
		mu.Lock()
		if scheduled >= maxCycle {
			mu.Unlock()
			break
		}
		mu.Unlock()
		// Concurrency-limited fan-out within the layer.
		sem := make(chan struct{}, maxConc)
		var wg sync.WaitGroup
		for _, order := range layer {
			step := stepsByOrder[order]
			if maxTier != "" && tierExceeds(step.Manifest.BlastRadiusTier, maxTier) {
				if l.Auditor != nil {
					l.Auditor.Emit(ctx, simian.AuditEvent{
						Event:   audit.EventStepSkipped,
						PlanID:  plan.PlanID,
						Mode:    simian.SourceAutonomous,
						Reason:  "severity-cap",
						Payload: map[string]any{"namespace": ns, "order": step.Order, "tier": string(step.Manifest.BlastRadiusTier)},
					})
				}
				continue
			}
			mu.Lock()
			if scheduled >= maxCycle {
				mu.Unlock()
				if l.Auditor != nil {
					l.Auditor.Emit(ctx, simian.AuditEvent{
						Event:   audit.EventStepSkipped,
						PlanID:  plan.PlanID,
						Mode:    simian.SourceAutonomous,
						Reason:  "cycle-budget-exhausted",
						Payload: map[string]any{"namespace": ns, "order": step.Order},
					})
				}
				continue
			}
			scheduled++
			mu.Unlock()
			wg.Add(1)
			sem <- struct{}{}
			go func(s simian.PlanStep) {
				defer wg.Done()
				defer func() { <-sem }()
				m := s.Manifest
				m.PlanID = plan.PlanID
				m.Source = simian.SourceAutonomous
				// Keyed before Apply: safety narrowing rewrites the spec in place.
				key := stepKey(m)
				if n := l.recentRefusals(key); n >= repeatedRefusalLimit {
					if l.Auditor != nil {
						l.Auditor.Emit(ctx, simian.AuditEvent{
							Event:   audit.EventStepSkipped,
							PlanID:  plan.PlanID,
							Mode:    simian.SourceAutonomous,
							Reason:  "repeated-refusal",
							Payload: map[string]any{"namespace": ns, "order": s.Order, "refusals": n, "window": repeatedRefusalWindow.String()},
						})
					}
					return
				}
				uid, err := l.Executor.Apply(ctx, m)
				l.noteOutcome(key, err)
				if err != nil {
					if l.Auditor != nil {
						l.Auditor.Emit(ctx, simian.AuditEvent{
							Event:   audit.EventStepSkipped,
							PlanID:  plan.PlanID,
							Mode:    simian.SourceAutonomous,
							Reason:  "executor-rejected",
							Payload: map[string]any{"namespace": ns, "order": s.Order, "error": err.Error()},
						})
					}
					return
				}
				mu.Lock()
				applied = append(applied, uid)
				mu.Unlock()
			}(step)
		}
		wg.Wait()
	}
	return applied
}

// tierOrdinal maps a tier string to an ordering: namespace < node < external.
// It reports ok=false for anything it does not recognise; callers decide what
// that means, because the safe answer is not the same on both sides.
func tierOrdinal(t simian.BlastRadiusTier) (int, bool) {
	switch t {
	case simian.TierNamespace:
		return 1, true
	case simian.TierNode:
		return 2, true
	case simian.TierExternal:
		return 3, true
	default:
		return 0, false
	}
}

// tierExceeds reports whether a step's tier is above the cycle's severity cap.
//
// Both unknowns fail closed, and they fail closed for different reasons. A
// manifest carrying a tier we do not recognise is not a namespace-scoped fault
// we can wave through — it is a fault whose blast radius we cannot bound, which
// is strictly worse than a known external one. A cap we cannot parse permits
// nothing, because the operator who set it was narrowing something.
func tierExceeds(have, max simian.BlastRadiusTier) bool {
	h, okHave := tierOrdinal(have)
	m, okMax := tierOrdinal(max)
	if !okHave || !okMax {
		return true
	}
	return h > m
}

// planStepRecords is the plan's steps as they go on plan.generated. A step
// that is never submitted — its dependency failed, the cycle was cut short —
// leaves no executor record, so this is the only place it is written down.
func planStepRecords(steps []simian.PlanStep) []any {
	out := make([]any, 0, len(steps))
	for _, s := range steps {
		rec := map[string]any{
			"order":     s.Order,
			"rationale": s.Rationale,
			"fault":     s.Manifest.AuditRecord(),
		}
		if len(s.DependsOn) > 0 {
			rec["depends_on"] = s.DependsOn
		}
		out = append(out, rec)
	}
	return out
}

// stepKey identifies a step by what it would do: kind, targets and spec. Two
// steps with the same key get the same answer from the executor, give or take
// the cluster's state.
func stepKey(m simian.FaultManifest) string {
	b, err := json.Marshal(struct {
		Kind    string             `json:"k"`
		Targets []simian.TargetRef `json:"t"`
		Spec    map[string]any     `json:"s"`
	}{m.ResourceKind, m.Targets, m.Spec})
	if err != nil {
		return ""
	}
	return string(b)
}

// recentRefusals returns how many times the step was refused inside the
// window, dropping older refusals as it goes.
func (l *Loop) recentRefusals(key string) int {
	if key == "" {
		return 0
	}
	l.refusalMu.Lock()
	defer l.refusalMu.Unlock()
	cutoff := time.Now().Add(-repeatedRefusalWindow)
	times := l.refusals[key]
	i := 0
	for i < len(times) && times[i].Before(cutoff) {
		i++
	}
	if i == len(times) {
		delete(l.refusals, key)
		return 0
	}
	l.refusals[key] = times[i:]
	return len(times) - i
}

// noteOutcome records a refusal against the step, or forgets its refusals if
// it ran. Budget refusals are not counted: they are about the moment, not the
// step.
func (l *Loop) noteOutcome(key string, err error) {
	if key == "" {
		return
	}
	l.refusalMu.Lock()
	defer l.refusalMu.Unlock()
	if err == nil {
		delete(l.refusals, key)
		return
	}
	if !executor.RefusalWorthRemembering(err) {
		return
	}
	if l.refusals == nil {
		l.refusals = map[string][]time.Time{}
	}
	l.refusals[key] = append(l.refusals[key], time.Now())
}
