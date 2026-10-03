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
	"testing"
	"time"

	"github.com/go-steer/simian-agent/pkg/simian"
)

var t0 = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

func at(min int) time.Time { return t0.Add(time.Duration(min) * time.Minute) }

func applied(uid string, min int, deadline time.Time) []Record {
	return []Record{
		{TS: at(min), Event: EventExecutorReceived, FaultUID: uid, Mode: "autonomous", PlanID: "p-1",
			Payload: map[string]any{"kind": "DNSChaos", "spec": map[string]any{"action": "error"}}},
		{TS: at(min), Event: EventDriverApplied, FaultUID: uid,
			Payload: map[string]any{"engine": "chaos-mesh", "kind": "DNSChaos", "duration": "10m0s",
				"targets":    []any{map[string]any{"namespace": "bank", "name": "userservice"}},
				"spec":       map[string]any{"action": "error", "selector": map[string]any{"namespaces": []any{"bank"}}},
				"engine_uid": "chaos-mesh|bank|simian-x", "deadline": deadline.Format(time.RFC3339)}},
	}
}

func TestFaultsFoldsALifecycleIntoOneRow(t *testing.T) {
	recs := applied("f-1", 0, at(10))
	recs = append(recs,
		Record{TS: at(1), Event: EventFaultInjected, FaultUID: "f-1", Payload: map[string]any{"passed": true}},
		Record{TS: at(2), Event: EventFaultEfficacy, FaultUID: "f-1", Payload: map[string]any{"passed": true}},
		Record{TS: at(2), Event: EventFaultEfficacy, FaultUID: "f-1", Payload: map[string]any{"passed": false}},
		Record{TS: at(10), Event: EventLeaseExpired, FaultUID: "f-1", Reason: "deadline-reached"},
		Record{TS: at(3), Event: EventCycleStarted}, // no fault: ignored
	)
	rows := Faults(recs)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.Outcome != OutcomeExpired || !r.EndedAt.Equal(at(10)) || r.Source != "autonomous" || r.PlanID != "p-1" {
		t.Errorf("row = %+v", r)
	}
	if spec, _ := r.Spec.(map[string]any); spec["selector"] == nil {
		t.Errorf("spec = %v, want what reached the cluster, not what was asked for", r.Spec)
	}
	if r.Injected == nil || !*r.Injected {
		t.Error("injected not recorded")
	}
	if r.Efficacy == nil || *r.Efficacy {
		t.Error("one failed efficacy probe should make the verdict no")
	}
}

func TestFaultsKnowsEveryWayAFaultEnds(t *testing.T) {
	recs := []Record{
		{TS: at(0), Event: EventExecutorReceived, FaultUID: "refused"},
		{TS: at(0), Event: EventExecutorRejected, FaultUID: "refused", Reason: "schema-invalid", Payload: map[string]any{"error": "spec.port: required"}},
		{TS: at(0), Event: EventDriverFailed, FaultUID: "driver", Reason: "driver-failed"},
	}
	recs = append(recs, applied("cleared", 0, at(10))...)
	recs = append(recs, Record{TS: at(4), Event: EventLeaseCleared, FaultUID: "cleared", Reason: "explicit-clear"})
	recs = append(recs, applied("leaked", 0, at(10))...)
	recs = append(recs, Record{TS: at(4), Event: EventLeaseCleared, FaultUID: "leaked", Reason: "injection-failed",
		Payload: map[string]any{"left_to_reaper": true}})

	want := map[string]string{"refused": OutcomeRefused, "driver": OutcomeDriverFailed, "cleared": OutcomeCleared, "leaked": OutcomeOpen}
	for _, r := range Faults(recs) {
		if r.Outcome != want[r.FaultUID] {
			t.Errorf("%s: outcome = %q, want %q", r.FaultUID, r.Outcome, want[r.FaultUID])
		}
		if r.FaultUID == "refused" && r.Error != "spec.port: required" {
			t.Errorf("refused row lost its error: %+v", r)
		}
	}
}

// #142: the trial's restarts cut the trail at driver.applied. At start-up the
// controller closes what the previous process left open.
func TestClosingEventsCloseOnlyWhatReachedTheClusterAndWasLeftOpen(t *testing.T) {
	recs := applied("ended", 0, at(10))
	recs = append(recs, applied("running", 0, at(120))...)
	recs = append(recs, applied("done", 0, at(10))...)
	recs = append(recs,
		Record{TS: at(10), Event: EventLeaseExpired, FaultUID: "done"},
		Record{TS: at(0), Event: EventExecutorReceived, FaultUID: "never-applied"},
	)
	now := at(60)
	closing := ClosingEvents(Faults(recs), now)
	if len(closing) != 2 || closing[0].FaultUID != "ended" || closing[1].FaultUID != "running" {
		t.Fatalf("closing = %+v, want ended and running", closing)
	}
	if closing[0].Payload["still_running"] != nil || closing[1].Payload["still_running"] != true {
		t.Errorf("still_running flags wrong: %v / %v", closing[0].Payload, closing[1].Payload)
	}

	// Read back, the fault that ran out ends at its deadline, not at the
	// restart that noticed.
	rows := Faults(append(recs, closing...))
	for _, r := range rows {
		switch r.FaultUID {
		case "ended":
			if r.Outcome != OutcomeExpired || r.Reason != ReasonUntrackedAfterRestart || !r.EndedAt.Equal(at(10)) {
				t.Errorf("ended: %+v", r)
			}
		case "running":
			if !r.EndedAt.Equal(now) {
				t.Errorf("running: ended_at = %v, want the restart", r.EndedAt)
			}
		}
	}
	if again := ClosingEvents(rows, at(61)); len(again) != 0 {
		t.Errorf("a second start-up closed %d faults again", len(again))
	}
}

func TestSinceKeepsFaultsFirstSeenAfterTheCutoff(t *testing.T) {
	recs := append(applied("old", 0, at(10)), applied("new", 30, at(40))...)
	rows := Since(Faults(recs), at(15))
	if len(rows) != 1 || rows[0].FaultUID != "new" {
		t.Errorf("rows = %+v", rows)
	}
}

// #172: what a restarted controller adopts is rebuilt from the row, and only
// for a fault that is still in the cluster.
func TestActiveFaultRebuildsOnlyAStillRunningFault(t *testing.T) {
	recs := applied("running", 0, at(120))
	recs = append(recs, applied("ended", 0, at(10))...)
	recs = append(recs, applied("done", 0, at(120))...)
	recs = append(recs, Record{TS: at(20), Event: EventLeaseCleared, FaultUID: "done", Reason: "explicit-clear"})
	byUID := map[string]FaultRow{}
	for _, r := range Faults(recs) {
		byUID[r.FaultUID] = r
	}
	now := at(60)

	af, ok := byUID["running"].ActiveFault(now)
	if !ok {
		t.Fatal("a fault applied, never closed and before its deadline was not rebuilt")
	}
	m := af.Manifest
	if af.FaultUID != "running" || af.EngineUID != "chaos-mesh|bank|simian-x" || !af.Deadline.Equal(at(120)) || !af.AppliedAt.Equal(at(0)) ||
		m.Engine != simian.EngineChaosMesh || m.ResourceKind != "DNSChaos" || m.Duration != 10*time.Minute ||
		m.Source != simian.SourceAutonomous || m.PlanID != "p-1" || m.Spec["action"] != "error" ||
		len(m.Targets) != 1 || m.Targets[0].Namespace != "bank" || m.Targets[0].Name != "userservice" {
		t.Errorf("rebuilt = %+v", af)
	}
	if _, ok := byUID["ended"].ActiveFault(now); ok {
		t.Error("a fault past its deadline was rebuilt")
	}
	if _, ok := byUID["done"].ActiveFault(now); ok {
		t.Error("a cleared fault was rebuilt")
	}
	noTargets := byUID["running"]
	noTargets.Targets = nil
	if _, ok := noTargets.ActiveFault(now); ok {
		t.Error("a fault with no recorded target was rebuilt; nothing could count it per namespace")
	}
}

func TestFaultsRecordsWhetherTheWorkloadRecovered(t *testing.T) {
	recs := applied("f-1", 0, at(10))
	recs = append(recs,
		Record{TS: at(10), Event: EventLeaseExpired, FaultUID: "f-1", Reason: "deadline-reached"},
		Record{TS: at(15), Event: EventFaultRecovered, FaultUID: "f-1",
			Payload: map[string]any{"passed": false, "unready": []any{"bank/frontend-a: container front CrashLoopBackOff, 6 restarts"}}},
	)
	r := Faults(recs)[0]
	if r.Outcome != OutcomeExpired || r.Recovered == nil || *r.Recovered || len(r.Unready) != 1 {
		t.Errorf("row = %+v, want expired and not recovered", r)
	}
}
