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

package audit

import (
	"context"
	"testing"

	"github.com/go-steer/simian-agent/pkg/simian"
)

func cyc(min int, event, reason string, payload map[string]any) Record {
	return Record{TS: at(min), Event: event, Reason: reason, Payload: payload}
}

func TestCyclesFoldWhatEachCycleDecided(t *testing.T) {
	ns := func(extra map[string]any) map[string]any {
		m := map[string]any{"namespace": "boutique"}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	recs := []Record{
		cyc(0, EventCycleStarted, "", ns(nil)),
		{TS: at(0), Event: EventPlanGenerated, PlanID: "p-1", Payload: ns(map[string]any{
			"hypothesis": "checkout survives a cartservice restart",
			"steps": []any{map[string]any{"order": float64(1), "duration_rationale": "recovery takes a minute",
				"fault": map[string]any{"kind": "PodChaos", "duration": "1m0s", "targets": []any{map[string]any{"name": "cartservice"}}}}},
		})},
		{TS: at(0), Event: EventStepSkipped, Reason: "executor-rejected", Payload: ns(map[string]any{"order": float64(2), "error": "target-incompatible"})},
		{TS: at(0), Event: EventCycleCompleted, PlanID: "p-1", Payload: ns(map[string]any{"applied_uids": []any{"f-1"}})},
		cyc(5, EventCycleStarted, "", ns(nil)),
		cyc(5, EventHealthGateFailed, "workload Deployment/adservice: 0/1 pods ready", ns(nil)),
		cyc(5, EventCycleSkipped, "health-gate", ns(nil)),
		cyc(10, EventCycleStarted, "", ns(nil)),
		cyc(10, EventCycleSkipped, "no-valid-plan", ns(map[string]any{"error": "step 1: HTTPChaos on port 50051 …"})),
		cyc(15, EventCycleStarted, "", ns(nil)),
		cyc(20, EventCycleStarted, "", ns(nil)),                  // the controller restarted mid-cycle
		{TS: at(21), Event: EventDriverApplied, FaultUID: "f-2"}, // not a cycle event
	}
	rows := Cycles(recs)
	if len(rows) != 5 {
		t.Fatalf("rows = %d, want 5: %+v", len(rows), rows)
	}
	c := rows[0]
	if c.Outcome != CycleCompleted || c.PlanID != "p-1" || c.Hypothesis == "" || len(c.Applied) != 1 ||
		len(c.Steps) != 1 || c.Steps[0].Kind != "PodChaos" || c.Steps[0].Target != "cartservice" ||
		c.Steps[0].DurationRationale == "" || len(c.Refused) != 1 || c.Refused[0].Order != 2 {
		t.Errorf("completed cycle = %+v", c)
	}
	if r := rows[1]; r.Outcome != CycleSkipped || r.Reason != "health-gate" || r.Detail != "workload Deployment/adservice: 0/1 pods ready" {
		t.Errorf("health-gate cycle = %+v", r)
	}
	if r := rows[2]; r.Reason != "no-valid-plan" || r.Detail == "" {
		t.Errorf("no-valid-plan cycle = %+v", r)
	}
	if rows[3].Outcome != CycleUnfinished || rows[4].Outcome != CycleOpen {
		t.Errorf("restart: %s then %s, want unfinished then open", rows[3].Outcome, rows[4].Outcome)
	}
}

// A live event carries Go types — an int order, a []string of UIDs — where
// one read back from the file carries JSON's. The log folds both alike.
func TestCycleLogReadsLiveEventsAsTheFileWould(t *testing.T) {
	l := NewCycleLog(0)
	emit := func(e simian.AuditEvent) { l.Emit(context.Background(), e) }
	emit(simian.AuditEvent{Event: EventCycleStarted, Payload: map[string]any{"namespace": "bank"}})
	emit(simian.AuditEvent{Event: EventStepSkipped, Reason: "severity-cap", Payload: map[string]any{"namespace": "bank", "order": 1}})
	emit(simian.AuditEvent{Event: EventCycleCompleted, Payload: map[string]any{"namespace": "bank", "applied_uids": []string{"f-9"}}})
	emit(simian.AuditEvent{Event: EventCycleStarted, Payload: map[string]any{"namespace": "boutique"}})
	emit(simian.AuditEvent{Event: EventDriverApplied, FaultUID: "f-9"}) // ignored

	if all := l.Recent("", 0); len(all) != 2 || all[0].Namespace != "boutique" {
		t.Fatalf("recent = %+v, want boutique (newest) then bank", all)
	}
	bank := l.Recent("bank", 1)
	if len(bank) != 1 || len(bank[0].Applied) != 1 || bank[0].Applied[0] != "f-9" || bank[0].Refused[0].Order != 1 {
		t.Errorf("bank = %+v", bank)
	}
}

func TestCycleLogKeepsOnlyTheNewest(t *testing.T) {
	l := NewCycleLog(4)
	for i := range 6 {
		l.Add(cyc(i, EventCycleStarted, "", map[string]any{"namespace": "ns"}))
	}
	if got := len(l.Recent("", 0)); got != 4 {
		t.Errorf("cycles kept = %d, want 4", got)
	}
}

func TestFaultLogFoldsFaultsAsTheExportDoes(t *testing.T) {
	l := NewFaultLog(0)
	emit := func(e simian.AuditEvent) { l.Emit(context.Background(), e) }
	target := []map[string]any{{"namespace": "boutique", "name": "cartservice"}}
	emit(simian.AuditEvent{Event: EventExecutorReceived, FaultUID: "f-1", Mode: simian.SourceAutonomous, Payload: map[string]any{"kind": "PodChaos", "targets": target}})
	emit(simian.AuditEvent{Event: EventDriverApplied, FaultUID: "f-1", Payload: map[string]any{"kind": "PodChaos", "targets": target, "deadline": "2026-10-07T12:00:00Z"}})
	emit(simian.AuditEvent{Event: EventLeaseExpired, FaultUID: "f-1", Reason: "deadline-reached"})
	emit(simian.AuditEvent{Event: EventFaultRecovered, FaultUID: "f-1", Payload: map[string]any{"passed": true}})
	emit(simian.AuditEvent{Event: EventExecutorReceived, FaultUID: "f-2", Payload: map[string]any{"kind": "PodChaos", "targets": []map[string]any{{"namespace": "bank"}}}})
	emit(simian.AuditEvent{Event: EventCycleStarted, Payload: map[string]any{"namespace": "boutique"}}) // not about a fault

	if all := l.Recent("", 0); len(all) != 2 || all[0].FaultUID != "f-2" {
		t.Fatalf("recent = %+v, want f-2 (newest) then f-1", all)
	}
	got := l.Recent("boutique", 5)
	if len(got) != 1 || got[0].Outcome != OutcomeExpired || got[0].Recovered == nil || !*got[0].Recovered || got[0].Kind != "PodChaos" {
		t.Errorf("boutique = %+v", got)
	}
}
