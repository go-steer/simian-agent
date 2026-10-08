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

package executor_test

import (
	"context"
	"testing"
	"time"

	internaltest "github.com/go-steer/simian-agent/internal/testutil"
	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/executor"
	"github.com/go-steer/simian-agent/pkg/lease"
	"github.com/go-steer/simian-agent/pkg/simian"
)

// A fault submitted and cleared from the web UI is recorded with the person
// who did each, as the audit export's fault row shows them.
func TestTheAuditTrailSaysWhoAppliedAndWhoCleared(t *testing.T) {
	log := audit.NewFaultLog(0)
	exec := executor.New(executor.DefaultConfig(),
		map[simian.Engine]simian.ChaosDriver{simian.EngineChaosMesh: &internaltest.FakeDriver{EngineName: simian.EngineChaosMesh}},
		lease.NewRegistry("h"), log, &executor.StaticEligibility{Eligible: map[string]bool{"boutique": true}})
	m := simian.FaultManifest{
		Source: simian.SourceDirected, Engine: simian.EngineChaosMesh, APIVersion: "chaos-mesh.org/v1alpha1",
		ResourceKind: "PodChaos", Duration: time.Minute,
		Spec:    map[string]any{"action": "pod-kill", "mode": "one", "selector": map[string]any{"labelSelectors": map[string]any{"app": "cartservice"}}},
		Targets: []simian.TargetRef{{Namespace: "boutique", Name: "cartservice", Labels: map[string]string{"app": "cartservice"}}},
	}
	uid, err := exec.Apply(simian.WithActor(context.Background(), "alice@example.com"), m)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := exec.Clear(simian.WithActor(context.Background(), "bob@example.com"), uid); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	rows := log.Recent("boutique", 10)
	if len(rows) != 1 || rows[0].RequestedBy != "alice@example.com" || rows[0].ClearedBy != "bob@example.com" || rows[0].Outcome != audit.OutcomeCleared {
		t.Fatalf("rows = %+v", rows)
	}

	// Without an actor — the CLI, the autonomous loop — nothing is claimed.
	uid2, err := exec.Apply(context.Background(), m)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, r := range log.Recent("boutique", 10) {
		if r.FaultUID == uid2 && r.RequestedBy != "" {
			t.Errorf("a fault with no actor was recorded as requested by %q", r.RequestedBy)
		}
	}
}
