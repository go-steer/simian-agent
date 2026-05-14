package loop

import (
	"context"
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
}

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
	for _, ns := range l.Namespaces {
		l.runOneSafely(ctx, ns)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			for _, ns := range l.Namespaces {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				l.runOneSafely(ctx, ns)
			}
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
// generator setup error). Health-gate failures and LLM unavailability are
// treated as benign skips: the plan is empty, applied is nil, error is nil.
func (l *Loop) RunOnce(ctx context.Context, ns string) (simian.AttackPlan, []string, error) {
	if l.Auditor != nil {
		l.Auditor.Emit(ctx, simian.AuditEvent{Event: audit.EventCycleStarted, Mode: simian.SourceAutonomous, Payload: map[string]any{"namespace": ns}})
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
				uid, err := l.Executor.Apply(ctx, m)
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
func tierOrdinal(t simian.BlastRadiusTier) int {
	switch t {
	case simian.TierNamespace:
		return 1
	case simian.TierNode:
		return 2
	case simian.TierExternal:
		return 3
	default:
		return 1
	}
}

func tierExceeds(have, max simian.BlastRadiusTier) bool {
	return tierOrdinal(have) > tierOrdinal(max)
}
