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

package eval_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/eval"
	"github.com/go-steer/simian-agent/pkg/scenario"
	"github.com/go-steer/simian-agent/pkg/simian"
)

// emitter writes audit events exactly the way `simian serve` does — through
// the real SLogAuditor onto a JSON handler.
//
// The offline scorer reads a file written by a process that is long gone, so
// the one thing that must never drift is the shape of that file. Hand-rolling
// the fixture would let the reader and the writer disagree without a test
// noticing.
type emitter struct {
	buf     bytes.Buffer
	auditor *audit.SLogAuditor
}

func newEmitter() *emitter {
	e := &emitter{}
	e.auditor = audit.New(slog.New(slog.NewJSONHandler(&e.buf, nil)))
	return e
}

func (e *emitter) emit(scenarioID string, ev simian.AuditEvent) {
	ctx := audit.WithScenarioID(context.Background(), scenarioID)
	e.auditor.Emit(ctx, ev)
}

// applied writes the events a fault that landed cleanly produces.
func (e *emitter) applied(scenarioID, faultUID string) {
	e.emit(scenarioID, simian.AuditEvent{Event: audit.EventDriverApplied, FaultUID: faultUID})
	e.emit(scenarioID, simian.AuditEvent{Event: audit.EventLeaseRegistered, FaultUID: faultUID})
	e.emit(scenarioID, simian.AuditEvent{
		Event:    audit.EventFaultEfficacy,
		FaultUID: faultUID,
		Payload: map[string]any{
			"probe":    "image-pull-failed",
			"mode":     simian.ProbeModeSettle,
			"passed":   true,
			"observed": "ImagePullBackOff",
			"expected": "ImagePullBackOff",
		},
	})
}

func (e *emitter) log() *strings.Reader { return strings.NewReader(e.buf.String()) }

func factsFor(t *testing.T, facts []eval.ScenarioFacts, id string) eval.ScenarioFacts {
	t.Helper()
	for _, f := range facts {
		if f.ScenarioID == id {
			return f
		}
	}
	t.Fatalf("no facts for scenario %q in %+v", id, facts)
	return eval.ScenarioFacts{}
}

func TestReadAuditReconstructsAFaultThatLanded(t *testing.T) {
	e := newEmitter()
	e.applied("s-1", "fault-a")

	facts, err := eval.ReadAudit(e.log())
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("got %d scenarios, want 1: %+v", len(facts), facts)
	}
	got := facts[0]
	if got.ScenarioID != "s-1" || !got.Manifested || got.InjectError != "" {
		t.Errorf("facts = %+v, want s-1 manifested with no inject error", got)
	}
	if got.InjectedAt.IsZero() {
		t.Error("no injection timestamp; the efficacy gate passing is what dates the run")
	}
	if len(got.Faults) != 1 || got.Faults[0] != "fault-a" {
		t.Errorf("faults = %v, want [fault-a]", got.Faults)
	}
}

// The acceptance criterion: a scenario with no passing efficacy record fails
// loudly. A zero here would read as "the agent missed it" on a cluster that
// was never broken.
func TestAFaultWithNoEfficacyRecordIsAnInjectFailure(t *testing.T) {
	e := newEmitter()
	e.emit("s-1", simian.AuditEvent{Event: audit.EventDriverApplied, FaultUID: "fault-a"})
	e.emit("s-1", simian.AuditEvent{Event: audit.EventLeaseRegistered, FaultUID: "fault-a"})

	facts, err := eval.ReadAudit(e.log())
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	got := factsFor(t, facts, "s-1")
	if got.Manifested {
		t.Error("an unverified fault was reported as manifested")
	}
	if !strings.Contains(got.InjectError, "no passing efficacy record") {
		t.Errorf("inject error = %q, want it to say the gate never passed", got.InjectError)
	}
	if !got.InjectedAt.IsZero() {
		t.Error("an unverified fault must not date the run")
	}
}

func TestAFailingEfficacyProbeIsAnInjectFailure(t *testing.T) {
	e := newEmitter()
	e.emit("s-1", simian.AuditEvent{Event: audit.EventDriverApplied, FaultUID: "fault-a"})
	e.emit("s-1", simian.AuditEvent{
		Event:    audit.EventFaultEfficacy,
		FaultUID: "fault-a",
		Reason:   string(simian.ReasonProbeFailed),
		Payload: map[string]any{
			"probe":    "oom-killed",
			"passed":   false,
			"expected": "OOMKilled",
			"observed": "Running",
		},
	})

	facts, err := eval.ReadAudit(e.log())
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	got := factsFor(t, facts, "s-1")
	if got.Manifested {
		t.Error("a failed gate was reported as manifested")
	}
	for _, want := range []string{"oom-killed", "OOMKilled", "Running"} {
		if !strings.Contains(got.InjectError, want) {
			t.Errorf("inject error = %q, want it to mention %q", got.InjectError, want)
		}
	}
}

// A fault whose gate fails usually goes on failing, and the later failures are
// consequences of the first. The first one is the one that explains the run.
func TestTheFirstFailingGateIsTheOneReported(t *testing.T) {
	e := newEmitter()
	e.emit("s-1", simian.AuditEvent{Event: audit.EventDriverApplied, FaultUID: "fault-a"})
	for _, probe := range []string{"settle-first", "steady-after"} {
		e.emit("s-1", simian.AuditEvent{
			Event:    audit.EventFaultEfficacy,
			FaultUID: "fault-a",
			Reason:   string(simian.ReasonProbeFailed),
			Payload:  map[string]any{"probe": probe, "passed": false},
		})
	}

	facts, err := eval.ReadAudit(e.log())
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	got := factsFor(t, facts, "s-1")
	if !strings.Contains(got.InjectError, "settle-first") {
		t.Errorf("inject error = %q, want the first failing gate", got.InjectError)
	}
	if strings.Contains(got.InjectError, "steady-after") {
		t.Errorf("inject error = %q, want only the first failing gate", got.InjectError)
	}
}

func TestADriverFailureIsAnInjectFailure(t *testing.T) {
	e := newEmitter()
	e.emit("s-1", simian.AuditEvent{
		Event:    audit.EventDriverFailed,
		FaultUID: "fault-a",
		Reason:   string(simian.ReasonDriverFailed),
		Payload:  map[string]any{"error": "namespace quota exceeded"},
	})

	facts, err := eval.ReadAudit(e.log())
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	got := factsFor(t, facts, "s-1")
	if got.Manifested || !strings.Contains(got.InjectError, "namespace quota exceeded") {
		t.Errorf("facts = %+v, want an inject failure naming the driver error", got)
	}
}

func TestARejectedFaultIsAnInjectFailure(t *testing.T) {
	e := newEmitter()
	e.emit("s-1", simian.AuditEvent{
		Event:    audit.EventExecutorRejected,
		FaultUID: "fault-a",
		Reason:   string(simian.ReasonPrecheckFailed),
	})

	facts, err := eval.ReadAudit(e.log())
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	got := factsFor(t, facts, "s-1")
	if got.Manifested || !strings.Contains(got.InjectError, string(simian.ReasonPrecheckFailed)) {
		t.Errorf("facts = %+v, want an inject failure naming the rejection", got)
	}
}

// A scenario is one incident. Half a cascade is not the incident the
// expectations describe, so grading a subject against it would score it on
// ground truth that was never true.
func TestOneFaultOfTwoLandingIsNotAManifestedScenario(t *testing.T) {
	e := newEmitter()
	e.applied("s-1", "fault-a")
	e.emit("s-1", simian.AuditEvent{Event: audit.EventDriverApplied, FaultUID: "fault-b"})

	facts, err := eval.ReadAudit(e.log())
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	got := factsFor(t, facts, "s-1")
	if got.Manifested {
		t.Error("a half-injected cascade was reported as manifested")
	}
	if !strings.Contains(got.InjectError, "fault-b") {
		t.Errorf("inject error = %q, want it to name the fault that did not land", got.InjectError)
	}
	if !slices.Equal(got.Faults, []string{"fault-a", "fault-b"}) {
		t.Errorf("faults = %v, want both, sorted", got.Faults)
	}
	// The half that landed has a real timestamp, and keeping it would make the
	// timing measures look valid for an incident that never fully happened.
	if !got.InjectedAt.IsZero() {
		t.Errorf("InjectedAt = %s, want zero on a half-injected scenario", got.InjectedAt)
	}
}

// Fault order is sorted, not map order, so a rerun over the same log does not
// shuffle the list an operator is diffing.
func TestFaultsAreListedInSortedOrder(t *testing.T) {
	e := newEmitter()
	for _, uid := range []string{"fault-m", "fault-a", "fault-z", "fault-b"} {
		e.applied("s-1", uid)
	}
	facts, err := eval.ReadAudit(e.log())
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	got := factsFor(t, facts, "s-1")
	want := []string{"fault-a", "fault-b", "fault-m", "fault-z"}
	if !slices.Equal(got.Faults, want) {
		t.Errorf("faults = %v, want %v", got.Faults, want)
	}
}

// The injection timestamp is when the *last* fault became live, because that
// is the first moment the whole scenario was there to be found.
func TestInjectedAtIsTheLastGateToPass(t *testing.T) {
	e := newEmitter()
	e.applied("s-1", "fault-a")
	time.Sleep(2 * time.Millisecond)
	e.applied("s-1", "fault-b")

	facts, err := eval.ReadAudit(e.log())
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	got := factsFor(t, facts, "s-1")
	if !got.Manifested {
		t.Fatalf("facts = %+v, want manifested", got)
	}

	last := lastEfficacyTime(t, e.buf.String())
	if !got.InjectedAt.Equal(last) {
		t.Errorf("InjectedAt = %s, want the last passing gate at %s", got.InjectedAt, last)
	}
}

func lastEfficacyTime(t *testing.T, log string) time.Time {
	t.Helper()
	var last time.Time
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		var l struct {
			Event string    `json:"event"`
			TS    time.Time `json:"ts"`
		}
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatalf("fixture line is not JSON: %v", err)
		}
		if l.Event == audit.EventFaultEfficacy && l.TS.After(last) {
			last = l.TS
		}
	}
	return last
}

// The reaper runs on its own context, long after the applying request
// returned, so its events carry a fault UID and no scenario. They also say
// nothing about whether the fault landed — a lease expiring is Simian tidying
// up on a timer, not the subject remediating — so they change no fact here.
func TestReaperEventsChangeNothing(t *testing.T) {
	e := newEmitter()
	e.applied("s-1", "fault-a")
	quiet, err := eval.ReadAudit(strings.NewReader(e.buf.String()))
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}

	// No scenario in context — this is the reaper's own goroutine.
	e.emit("", simian.AuditEvent{Event: audit.EventLeaseExpired, FaultUID: "fault-a", Reason: "deadline-reached"})
	reaped, err := eval.ReadAudit(e.log())
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}

	if !reflect.DeepEqual(quiet, reaped) {
		t.Errorf("the reaper changed the facts:\n without=%+v\n    with=%+v", quiet, reaped)
	}
}

// Log order is not time order: audit lines from concurrent injections are
// interleaved by whoever reaches the writer first, and two runs' logs are
// routinely concatenated. The gate that passed latest is still the gate the
// clock starts at.
func TestAnOutOfOrderLogStillFindsTheLastGate(t *testing.T) {
	const log = `{"event":"driver.applied","ts":"2026-09-05T12:00:00Z","fault_uid":"fault-a","scenario_id":"s-1"}
{"event":"fault.efficacy","ts":"2026-09-05T12:00:30Z","fault_uid":"fault-a","scenario_id":"s-1","payload":{"probe":"settle","passed":true}}
{"event":"fault.efficacy","ts":"2026-09-05T12:00:10Z","fault_uid":"fault-a","scenario_id":"s-1","payload":{"probe":"steady","passed":true}}
`
	facts, err := eval.ReadAudit(strings.NewReader(log))
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	got := factsFor(t, facts, "s-1")
	want := time.Date(2026, 9, 5, 12, 0, 30, 0, time.UTC)
	if !got.InjectedAt.Equal(want) {
		t.Errorf("InjectedAt = %s, want the latest gate at %s", got.InjectedAt, want)
	}
}

// An audit log is routinely a controller's whole stdout. A startup banner is
// not a reason to refuse to score the run underneath it.
func TestReadAuditSkipsLinesItCannotUse(t *testing.T) {
	e := newEmitter()
	e.applied("s-1", "fault-a")

	log := "starting simian serve v1.2.3\n" +
		`{"level":"INFO","msg":"listening","addr":":8080"}` + "\n" +
		"{ not json at all\n" +
		"\n" +
		e.buf.String()

	facts, err := eval.ReadAudit(strings.NewReader(log))
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	if len(facts) != 1 || !facts[0].Manifested {
		t.Errorf("facts = %+v, want the one scenario, manifested", facts)
	}
}

// Events with no scenario and no known fault belong to something else sharing
// the log — a manual chaos apply, the controller's own lifecycle.
func TestReadAuditIgnoresEventsOutsideAnyScenario(t *testing.T) {
	e := newEmitter()
	e.emit("", simian.AuditEvent{Event: audit.EventCycleStarted})
	e.emit("", simian.AuditEvent{Event: audit.EventDriverApplied, FaultUID: "someone-elses-fault"})
	e.applied("s-1", "fault-a")

	facts, err := eval.ReadAudit(e.log())
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	if len(facts) != 1 || facts[0].ScenarioID != "s-1" {
		t.Errorf("facts = %+v, want only s-1", facts)
	}
}

// Plenty of events are stamped with a scenario and name no fault — the cycle
// starting, the scenario being loaded. Counting one as a fault would invent a
// fault nobody could have verified, and fail the scenario for it.
func TestScenarioEventsThatNameNoFaultAreNotFaults(t *testing.T) {
	e := newEmitter()
	e.emit("s-1", simian.AuditEvent{Event: audit.EventCycleStarted})
	e.applied("s-1", "fault-a")

	facts, err := eval.ReadAudit(e.log())
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	got := factsFor(t, facts, "s-1")
	if !got.Manifested {
		t.Errorf("facts = %+v, want manifested; a fault-less event became a fault", got)
	}
	if !slices.Equal(got.Faults, []string{"fault-a"}) {
		t.Errorf("faults = %v, want only the real one", got.Faults)
	}
}

// Scenario order follows the log, so two scorecards for the same run are
// diffable and a rerun does not shuffle the rows.
func TestScenarioOrderFollowsTheLog(t *testing.T) {
	e := newEmitter()
	for _, id := range []string{"s-c", "s-a", "s-b"} {
		e.applied(id, "fault-"+id)
	}
	facts, err := eval.ReadAudit(e.log())
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	var got []string
	for _, f := range facts {
		got = append(got, f.ScenarioID)
	}
	want := []string{"s-c", "s-a", "s-b"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// Every fault that went wrong is named, not just the first. An operator
// chasing a broken rig needs the whole list in one pass.
func TestEveryFailedFaultIsNamed(t *testing.T) {
	e := newEmitter()
	e.emit("s-1", simian.AuditEvent{
		Event: audit.EventDriverFailed, FaultUID: "fault-a",
		Payload: map[string]any{"error": "quota exceeded"},
	})
	e.emit("s-1", simian.AuditEvent{Event: audit.EventDriverApplied, FaultUID: "fault-b"})

	facts, err := eval.ReadAudit(e.log())
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	got := factsFor(t, facts, "s-1").InjectError
	for _, want := range []string{"fault-a", "quota exceeded", "fault-b", "; "} {
		if !strings.Contains(got, want) {
			t.Errorf("inject error = %q, want it to mention %q", got, want)
		}
	}
}

// A payload that carries no detail still has to produce a sentence. An empty
// InjectError would read as "this scenario is fine".
func TestAFailureWithNoDetailStillExplainsItself(t *testing.T) {
	for _, tc := range []struct {
		name string
		ev   simian.AuditEvent
		want string
	}{
		{"driver failed", simian.AuditEvent{Event: audit.EventDriverFailed, FaultUID: "f"}, "driver apply failed"},
		{"rejected", simian.AuditEvent{Event: audit.EventExecutorRejected, FaultUID: "f"}, "rejected by the executor"},
		{
			"bare probe failure",
			simian.AuditEvent{Event: audit.EventFaultEfficacy, FaultUID: "f", Payload: map[string]any{"passed": false}},
			"efficacy gate did not pass",
		},
	} {
		e := newEmitter()
		e.emit("s-1", tc.ev)
		facts, err := eval.ReadAudit(e.log())
		if err != nil {
			t.Fatalf("%s: ReadAudit: %v", tc.name, err)
		}
		got := factsFor(t, facts, "s-1").InjectError
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: inject error = %q, want it to mention %q", tc.name, got, tc.want)
		}
	}
}

// A probe that errored rather than merely disagreed says so, and a payload
// field that is not a string is still readable rather than dropped.
func TestProbeFailureDetailSurvivesTheLog(t *testing.T) {
	e := newEmitter()
	e.emit("s-1", simian.AuditEvent{
		Event:    audit.EventFaultEfficacy,
		FaultUID: "f",
		Payload: map[string]any{
			"probe":    "http-probe",
			"passed":   false,
			"expected": 200,
			"observed": 503,
			"error":    "connection refused",
		},
	})
	facts, err := eval.ReadAudit(e.log())
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	got := factsFor(t, facts, "s-1").InjectError
	for _, want := range []string{"http-probe", "200", "503", "connection refused"} {
		if !strings.Contains(got, want) {
			t.Errorf("inject error = %q, want it to mention %q", got, want)
		}
	}
}

// Not every writer of an audit line is SLogAuditor. A log that carries only
// slog's own `time` still dates the run rather than dating it to the zero
// value, which would make time_to_detect a fifty-six-year interval.
func TestALineWithNoAuditStampFallsBackToTheRecordTime(t *testing.T) {
	line := `{"time":"2026-09-05T12:00:04Z","level":"INFO","msg":"audit","event":"fault.efficacy",` +
		`"fault_uid":"f-1","scenario_id":"s-1","payload":{"passed":true}}`

	facts, err := eval.ReadAudit(strings.NewReader(line))
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	got := factsFor(t, facts, "s-1")
	want := time.Date(2026, 9, 5, 12, 0, 4, 0, time.UTC)
	if !got.InjectedAt.Equal(want) {
		t.Errorf("InjectedAt = %s, want %s", got.InjectedAt, want)
	}
}

// --- run file ---

const goodRunFile = `{
  "subject": "core-sre-agent",
  "runs": [
    {
      "scenario_id": "s-1",
      "detected_at": "2026-09-05T12:00:30Z",
      "report": {
        "overall_severity": "critical",
        "findings": [
          {"kind": "Pod", "resource_name": "checkout-api-abc", "reason": "ImagePullBackOff", "severity": "critical", "namespace": "shop"}
        ]
      }
    }
  ]
}`

func TestReadRunFile(t *testing.T) {
	rf, err := eval.ReadRunFile(strings.NewReader(goodRunFile))
	if err != nil {
		t.Fatalf("ReadRunFile: %v", err)
	}
	if rf.Subject != "core-sre-agent" || len(rf.Runs) != 1 {
		t.Fatalf("run file = %+v", rf)
	}
	rec := rf.Runs[0]
	if rec.Report == nil || len(rec.Report.Findings) != 1 {
		t.Fatalf("record = %+v, want one finding", rec)
	}
	if rec.Report.Findings[0].Namespace != "shop" {
		t.Errorf("namespace = %q, want shop", rec.Report.Findings[0].Namespace)
	}
	if rec.DetectedAt.IsZero() {
		t.Error("detected_at did not decode")
	}
}

func TestReadRunFileRejectsBadArtifacts(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"not json", `{`, "reading run file"},
		{"no subject", `{"runs":[]}`, "names no subject"},
		{"no scenario id", `{"subject":"x","runs":[{"report":{}}]}`, "no scenario_id"},
		{
			"duplicate scenario",
			`{"subject":"x","runs":[{"scenario_id":"s-1"},{"scenario_id":"s-1"}]}`,
			"two runs for scenario",
		},
		// A typo in a field name would otherwise become a silent zero — an
		// unread detected_at is a skipped measure nobody asked for.
		{"unknown field", `{"subject":"x","runs":[{"scenario_id":"s-1","detectedAt":"2026-09-05T12:00:00Z"}]}`, "reading run file"},
	} {
		_, err := eval.ReadRunFile(strings.NewReader(tc.body))
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
}

// --- join ---

func joinPack(scenarios ...scenario.Scenario) scenario.Pack {
	return scenario.Pack{Name: "parity", Scenarios: scenarios}
}

func joinScenario(id string) scenario.Scenario {
	return scenario.Scenario{
		ID:     id,
		Name:   id,
		Prompt: "Check namespace shop and report what you find.",
		Source: scenario.SourcePackParity,
		Faults: []simian.FaultManifest{{
			Engine:       simian.EngineKubeState,
			ResourceKind: "ImageUnresolvable",
			Duration:     5 * time.Minute,
			Targets:      []simian.TargetRef{{Namespace: "shop"}},
		}},
		Expect: []scenario.ExpectedFinding{
			{Kind: "Pod", Name: "checkout-api", Reasons: []string{"ImagePullBackOff"}},
		},
		Severity: scenario.SeverityCritical,
	}
}

func joinControl(id string) scenario.Scenario {
	s := joinScenario(id)
	s.Faults = nil
	s.Expect = nil
	s.Severity = scenario.SeverityOK
	return s
}

func TestJoinPairsTheTwoArtifactsOnScenarioID(t *testing.T) {
	e := newEmitter()
	e.applied("s-1", "fault-a")
	facts, err := eval.ReadAudit(e.log())
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	rf, err := eval.ReadRunFile(strings.NewReader(goodRunFile))
	if err != nil {
		t.Fatalf("ReadRunFile: %v", err)
	}

	runs, err := eval.Join(joinPack(joinScenario("s-1")), facts, rf)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("got %d runs, want 1", len(runs))
	}
	run := runs[0]
	if run.Subject != "core-sre-agent" || !run.Manifested || run.InjectError != "" {
		t.Errorf("run = %+v", run)
	}
	if run.InjectedAt.IsZero() || run.DetectedAt.IsZero() {
		t.Error("both timestamps should have survived the join")
	}
	// The two timestamps come from opposite sides of the join: InjectedAt is
	// the audit log's, DetectedAt is the report's. Neither may be taken from
	// the other, which is why they disagree in a fixture where the audit log
	// is written now and the report is a fixed string.
	if run.InjectedAt.Equal(run.DetectedAt) {
		t.Error("the two timestamps came from the same place")
	}
}

func TestJoinRejectsAReportForAScenarioTheAuditNeverSaw(t *testing.T) {
	rf, err := eval.ReadRunFile(strings.NewReader(goodRunFile))
	if err != nil {
		t.Fatalf("ReadRunFile: %v", err)
	}
	_, err = eval.Join(joinPack(joinScenario("s-1")), nil, rf)
	if err == nil || !strings.Contains(err.Error(), "no record of it being injected") {
		t.Errorf("error = %v, want a complaint about the missing audit trail", err)
	}
}

func TestJoinRejectsAnInjectedScenarioNobodyReportedOn(t *testing.T) {
	e := newEmitter()
	e.applied("s-1", "fault-a")
	e.applied("s-2", "fault-b")
	facts, err := eval.ReadAudit(e.log())
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	rf, err := eval.ReadRunFile(strings.NewReader(goodRunFile))
	if err != nil {
		t.Fatalf("ReadRunFile: %v", err)
	}

	_, err = eval.Join(joinPack(joinScenario("s-1"), joinScenario("s-2")), facts, rf)
	if err == nil || !strings.Contains(err.Error(), "s-2") {
		t.Errorf("error = %v, want it to name the unreported scenario", err)
	}
}

func TestJoinRejectsAScenarioOutsideThePack(t *testing.T) {
	e := newEmitter()
	e.applied("s-1", "fault-a")
	facts, err := eval.ReadAudit(e.log())
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	rf, err := eval.ReadRunFile(strings.NewReader(goodRunFile))
	if err != nil {
		t.Fatalf("ReadRunFile: %v", err)
	}

	_, err = eval.Join(joinPack(joinScenario("s-other")), facts, rf)
	if err == nil || !strings.Contains(err.Error(), "not in pack") {
		t.Errorf("error = %v, want a complaint about the pack", err)
	}
}

// A control injects nothing, so its audit trail looks exactly like a scenario
// nobody injected. Only the pack can tell them apart — and getting it wrong
// would skip every measure on the one scenario type whose whole purpose is to
// measure invention.
func TestAControlIsNotAnInjectFailure(t *testing.T) {
	e := newEmitter()
	e.emit("s-control", simian.AuditEvent{Event: audit.EventExecutorReceived})
	facts, err := eval.ReadAudit(e.log())
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	if factsFor(t, facts, "s-control").InjectError == "" {
		t.Error("read on its own, a scenario with no fault should look uninjected")
	}

	rf := eval.RunFile{Subject: "x", Runs: []eval.RunRecord{{
		ScenarioID: "s-control",
		Report:     &eval.Report{OverallSeverity: scenario.SeverityOK},
	}}}
	runs, err := eval.Join(joinPack(joinControl("s-control")), facts, rf)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if runs[0].InjectError != "" || !runs[0].Manifested {
		t.Errorf("run = %+v, want a control scored rather than skipped", runs[0])
	}

	for _, sc := range eval.ScoreRun(joinControl("s-control"), runs[0]) {
		if sc.Name == eval.MeasureHallucination && sc.Skipped {
			t.Error("hallucination was skipped on a control; that is the only measure a control exists for")
		}
	}
}

// The non-control version of the same trace really is an inject failure, so
// the fix-up above is not just switching the check off.
func TestAScenarioWithFaultsAndNoFaultEventsIsAnInjectFailure(t *testing.T) {
	e := newEmitter()
	e.emit("s-1", simian.AuditEvent{Event: audit.EventExecutorReceived})
	facts, err := eval.ReadAudit(e.log())
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}

	rf := eval.RunFile{Subject: "x", Runs: []eval.RunRecord{{ScenarioID: "s-1"}}}
	runs, err := eval.Join(joinPack(joinScenario("s-1")), facts, rf)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if runs[0].InjectError == "" || runs[0].Manifested {
		t.Errorf("run = %+v, want an inject failure", runs[0])
	}
}
