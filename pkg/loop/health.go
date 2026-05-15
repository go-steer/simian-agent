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

// Package loop runs the M3 autonomous-mode planning cycle: tick →
// health gate → topology snapshot → plan generation → bounded execution.
// The package owns no fault-application logic of its own; that lives in
// pkg/executor. The loop is the orchestrator only.
package loop

import (
	"context"
	"fmt"

	"github.com/go-steer/simian-agent/pkg/simian"
	"github.com/go-steer/simian-agent/pkg/sut"
	"github.com/go-steer/simian-agent/pkg/topology"
)

// HealthGate decides whether a cycle may proceed. A nil error means the
// arena is healthy enough to inject chaos; a non-nil error means skip the
// cycle (with the error string surfaced as the audit reason).
type HealthGate interface {
	Check(ctx context.Context, namespace string) error
}

// BaselineHealthGate is the v1 health-gate impl. It requires:
//   - A baseline cached for the namespace.
//   - Every baseline workload at desired replicas, ready (verified via the
//     topology snapshot — no extra round-trips).
//   - Zero Simian-managed faults currently active in the namespace.
//
// Metric drift from the baseline is intentionally NOT checked in v1 because
// get_metrics ships as a stub; once a metrics provider is wired in a later
// milestone the gate can grow a drift check without breaking callers.
type BaselineHealthGate struct {
	Baselines    BaselineLookup
	Topology     TopologySnapshotter
	ActiveFaults ActiveFaultsLookup
}

// BaselineLookup is satisfied by *sut.Manager.
type BaselineLookup interface {
	Baseline(namespace string) (sut.Baseline, bool)
}

// TopologySnapshotter is satisfied by *topology.Discoverer.
type TopologySnapshotter interface {
	Snapshot(ctx context.Context, namespace string) (*topology.TargetTopology, error)
}

// ActiveFaultsLookup is satisfied by simian.FaultExecutor — its ListActive
// returns the namespace-filtered active set.
type ActiveFaultsLookup interface {
	ListActive(ctx context.Context, namespace string) ([]simian.ActiveFault, error)
}

// Check implements HealthGate.
func (g *BaselineHealthGate) Check(ctx context.Context, ns string) error {
	if g.Baselines == nil || g.Topology == nil || g.ActiveFaults == nil {
		return fmt.Errorf("health gate: dependencies not configured")
	}
	bl, ok := g.Baselines.Baseline(ns)
	if !ok {
		return fmt.Errorf("no baseline cached for namespace %q (deploy a SUT first)", ns)
	}
	active, err := g.ActiveFaults.ListActive(ctx, ns)
	if err != nil {
		return fmt.Errorf("listing active faults: %w", err)
	}
	if len(active) > 0 {
		return fmt.Errorf("%d simian-managed fault(s) still active in %q", len(active), ns)
	}
	snap, err := g.Topology.Snapshot(ctx, ns)
	if err != nil {
		return fmt.Errorf("topology snapshot: %w", err)
	}
	for _, w := range bl.Workloads {
		desired := w.DesiredReplicas
		ready := readyPodsFor(snap, w.Name)
		if ready < desired {
			return fmt.Errorf("workload %s/%s: %d/%d pods ready (baseline expected %d)",
				w.Kind, w.Name, ready, desired, desired)
		}
	}
	return nil
}

func readyPodsFor(snap *topology.TargetTopology, workload string) int32 {
	if snap == nil {
		return 0
	}
	pods, ok := snap.PodStatus[workload]
	if !ok {
		return 0
	}
	var n int32
	for _, p := range pods {
		if p.Ready {
			n++
		}
	}
	return n
}
