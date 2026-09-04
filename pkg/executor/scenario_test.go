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
	"testing"

	"github.com/go-steer/simian-agent/internal/testutil"
	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/lease"
	"github.com/go-steer/simian-agent/pkg/simian"
)

// newScenarioExecutor builds an executor whose auditor stamps the scenario ID
// the way the production sink does, and hands back the underlying recorder.
func newScenarioExecutor(t *testing.T) (*Executor, *testutil.FakeAuditor) {
	t.Helper()
	rec := &testutil.FakeAuditor{}
	driver := &testutil.FakeDriver{EngineName: simian.EngineChaosMesh}
	exec := New(
		DefaultConfig(),
		map[simian.Engine]simian.ChaosDriver{simian.EngineChaosMesh: driver},
		lease.NewRegistry("test-holder"),
		audit.NewScenarioStamper(rec),
		&StaticEligibility{Eligible: map[string]bool{"online-boutique": true}},
	)
	return exec, rec
}

// This is the acceptance criterion for the scenario join key: every audit
// event emitted while a scenario is running carries that scenario's ID.
//
// It is asserted over *all* recorded events rather than a named list, because
// the failure this guards against is a new emission point added later that
// nobody remembers to stamp. A per-event allowlist would keep passing while
// the hole opened.
func TestEveryAuditEventDuringAScenarioCarriesTheScenarioID(t *testing.T) {
	exec, rec := newScenarioExecutor(t)
	ctx := audit.WithScenarioID(context.Background(), "s-cascade-01")

	uid, err := exec.Apply(ctx, goodManifest())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := exec.Clear(ctx, uid); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	if len(rec.Events) == 0 {
		t.Fatal("no audit events recorded")
	}
	for _, ev := range rec.Events {
		if ev.ScenarioID != "s-cascade-01" {
			t.Errorf("event %q has ScenarioID %q, want %q", ev.Event, ev.ScenarioID, "s-cascade-01")
		}
	}
	t.Logf("checked %d events", len(rec.Events))
}

// A rejected fault is still part of the scenario's record. If the rejection
// were unstamped, a scenario whose fault was refused would look like a
// scenario that was never attempted.
func TestRejectionEventsAlsoCarryTheScenarioID(t *testing.T) {
	exec, rec := newScenarioExecutor(t)
	ctx := audit.WithScenarioID(context.Background(), "s-rejected")

	m := goodManifest()
	m.Targets[0].Namespace = "kube-system"
	if _, err := exec.Apply(ctx, m); err == nil {
		t.Fatal("expected the ineligible namespace to be rejected")
	}

	if len(rec.Events) == 0 {
		t.Fatal("no audit events recorded")
	}
	for _, ev := range rec.Events {
		if ev.ScenarioID != "s-rejected" {
			t.Errorf("event %q has ScenarioID %q, want %q", ev.Event, ev.ScenarioID, "s-rejected")
		}
	}
}

// Outside a scenario the field stays empty rather than picking up a stale or
// invented value, so ad-hoc chaos is distinguishable from a graded run.
func TestEventsOutsideAScenarioCarryNoScenarioID(t *testing.T) {
	exec, rec := newScenarioExecutor(t)
	if _, err := exec.Apply(context.Background(), goodManifest()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, ev := range rec.Events {
		if ev.ScenarioID != "" {
			t.Errorf("event %q has ScenarioID %q outside a scenario", ev.Event, ev.ScenarioID)
		}
	}
}
