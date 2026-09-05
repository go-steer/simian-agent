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

package eval

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/simian-agent/pkg/scenario"
	"github.com/go-steer/simian-agent/pkg/simian"
)

func testScenario(expect ...scenario.ExpectedFinding) scenario.Scenario {
	return scenario.Scenario{
		ID:     "s-1",
		Name:   "test",
		Prompt: "Check namespace shop and report what you find.",
		Source: scenario.SourcePackParity,
		Faults: []simian.FaultManifest{{
			Engine:       simian.EngineKubeState,
			APIVersion:   "apps/v1",
			ResourceKind: "ImageUnresolvable",
			Duration:     5 * time.Minute,
			Targets:      []simian.TargetRef{{Namespace: "shop"}},
		}},
		Expect:   expect,
		Severity: scenario.SeverityCritical,
	}
}

func finding(kind, name, reason string, sev scenario.Severity) scenario.Finding {
	return scenario.Finding{Kind: kind, ResourceName: name, Reason: reason, Severity: sev, Namespace: "shop"}
}

func runWith(findings ...scenario.Finding) Run {
	return Run{
		ScenarioID: "s-1",
		Manifested: true,
		Report:     &Report{Findings: findings, OverallSeverity: scenario.SeverityCritical},
	}
}

func approx(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("value = %v, want %v", got, want)
	}
}

// --- recall ---

func TestRecall(t *testing.T) {
	s := testScenario(
		scenario.ExpectedFinding{Kind: "Pod", Name: "checkout-api", Reasons: []string{"ImagePullBackOff"}},
		scenario.ExpectedFinding{Kind: "Pod", Name: "payments-worker", Reasons: []string{"CrashLoopBackOff"}},
	)

	full := Recall{}.Score(s, runWith(
		finding("Pod", "checkout-api-abc", "ImagePullBackOff", scenario.SeverityCritical),
		finding("Pod", "payments-worker-def", "CrashLoopBackOff", scenario.SeverityCritical),
	))
	approx(t, full.Value, 1)

	half := Recall{}.Score(s, runWith(
		finding("Pod", "checkout-api-abc", "ImagePullBackOff", scenario.SeverityCritical),
	))
	approx(t, half.Value, 0.5)
	if !strings.Contains(half.Comment, "payments-worker") {
		t.Errorf("comment should name the miss, got %q", half.Comment)
	}
}

// The misses are sorted so the comment reads the same for two runs that missed
// the same things, whatever order the scenario happened to list them in.
func TestRecallNamesMissesInASortedOrder(t *testing.T) {
	s := testScenario(
		scenario.ExpectedFinding{Kind: "Pod", Name: "zulu"},
		scenario.ExpectedFinding{Kind: "Pod", Name: "alpha"},
		scenario.ExpectedFinding{Kind: "Pod", Name: "mike"},
	)
	got := Recall{}.Score(s, runWith())
	if !strings.HasSuffix(got.Comment, "missed=Pod/alpha,Pod/mike,Pod/zulu") {
		t.Errorf("comment = %q, want the misses sorted", got.Comment)
	}
}

func TestRecallIsSkippedOnAHealthyControl(t *testing.T) {
	got := Recall{}.Score(testScenario(), runWith())
	if !got.Skipped {
		t.Errorf("recall on a control = %+v, want skipped", got)
	}
}

// A subject that cannot produce a parseable report has failed the task. That
// is a zero, not a skip — skipping it would remove the run from the mean and
// let a subject improve its score by crashing.
func TestRecallScoresZeroWhenThereIsNoReport(t *testing.T) {
	s := testScenario(scenario.ExpectedFinding{Kind: "Pod", Name: "checkout-api"})
	got := Recall{}.Score(s, Run{ScenarioID: "s-1"})
	if got.Skipped || got.Value != 0 {
		t.Errorf("recall with no report = %+v, want a hard zero", got)
	}
}

// The prompt named one namespace. A finding about somewhere else is not an
// answer to the question that was asked.
func TestRecallIgnoresFindingsInAnotherNamespace(t *testing.T) {
	s := testScenario(scenario.ExpectedFinding{Kind: "Pod", Name: "checkout-api", Reasons: []string{"ImagePullBackOff"}})
	f := finding("Pod", "checkout-api", "ImagePullBackOff", scenario.SeverityCritical)
	f.Namespace = "somewhere-else"
	approx(t, Recall{}.Score(s, runWith(f)).Value, 0)
}

// Omitting the namespace is terse, not wrong: the subject was asked about one
// namespace and answering about it without repeating the name is normal.
func TestRecallAcceptsAFindingWithNoNamespace(t *testing.T) {
	s := testScenario(scenario.ExpectedFinding{Kind: "Pod", Name: "checkout-api", Reasons: []string{"ImagePullBackOff"}})
	f := finding("Pod", "checkout-api", "ImagePullBackOff", scenario.SeverityCritical)
	f.Namespace = ""
	approx(t, Recall{}.Score(s, runWith(f)).Value, 1)
}

// A scenario that breaks two namespaces has no single namespace to scope
// against, so nothing may be discarded. Scoping to whichever one came first
// would throw away half the answer to a question that spanned both.
func TestAScenarioSpanningNamespacesDiscardsNothing(t *testing.T) {
	s := testScenario(
		scenario.ExpectedFinding{Kind: "Pod", Name: "checkout-api", Reasons: []string{"ImagePullBackOff"}},
		scenario.ExpectedFinding{Kind: "Pod", Name: "payments-worker", Reasons: []string{"CrashLoopBackOff"}},
	)
	s.Faults = append(s.Faults, s.Faults[0])
	s.Faults[0].Targets = []simian.TargetRef{{Namespace: "shop"}}
	s.Faults[1].Targets = []simian.TargetRef{{Namespace: "payments"}}

	if ns := scenarioNamespace(s); ns != "" {
		t.Errorf("scenarioNamespace = %q, want empty for a scenario spanning namespaces", ns)
	}

	shop := finding("Pod", "checkout-api-1", "ImagePullBackOff", scenario.SeverityCritical)
	pay := finding("Pod", "payments-worker-1", "CrashLoopBackOff", scenario.SeverityCritical)
	pay.Namespace = "payments"
	approx(t, Recall{}.Score(s, runWith(shop, pay)).Value, 1)
}

// One namespace repeated across faults is still one namespace, and a
// cluster-scoped target alongside it is not a second one. A node has no
// namespace to disagree about, so treating its blank as a conflict would
// descope the whole scenario.
func TestAScenarioWithOneNamespaceScopesToIt(t *testing.T) {
	s := testScenario(scenario.ExpectedFinding{Kind: "Pod", Name: "checkout-api"})
	s.Faults = append(s.Faults, s.Faults[0])
	if ns := scenarioNamespace(s); ns != "shop" {
		t.Errorf("scenarioNamespace = %q, want shop", ns)
	}

	s.Faults[0].Targets = append(s.Faults[0].Targets, simian.TargetRef{Kind: "Node", Name: "gke-pool-1"})
	if ns := scenarioNamespace(s); ns != "shop" {
		t.Errorf("scenarioNamespace with a cluster-scoped target = %q, want shop", ns)
	}
}

// A fault that targets by label rather than by namespace leaves nothing to
// scope against.
func TestAScenarioWithNoNamespaceScopesToNothing(t *testing.T) {
	s := testScenario(scenario.ExpectedFinding{Kind: "Pod", Name: "checkout-api"})
	s.Faults[0].Targets = []simian.TargetRef{{Labels: map[string]string{"app": "checkout-api"}}}
	if ns := scenarioNamespace(s); ns != "" {
		t.Errorf("scenarioNamespace = %q, want empty", ns)
	}
}

// --- root cause ---

// The acceptance criterion: root cause is scored independently of recall, and
// a scenario naming no root is skipped rather than counted.
func TestRootCauseIsSkippedWhenNoRootIsMarked(t *testing.T) {
	s := testScenario(scenario.ExpectedFinding{Kind: "Pod", Name: "checkout-api"})
	got := RootCause{}.Score(s, runWith())
	if !got.Skipped {
		t.Errorf("root_cause with no root marked = %+v, want skipped", got)
	}
}

// The distinction recall cannot make: reporting only the symptom and
// reporting only the cause both score 0.5 on recall, and they are not the
// same answer.
func TestRootCauseSeparatesTheCauseFromTheSymptom(t *testing.T) {
	s := testScenario(
		scenario.ExpectedFinding{Kind: "Pod", Name: "session-store", Reasons: []string{"CrashLoopBackOff"}, Root: true},
		scenario.ExpectedFinding{Kind: "Service", Name: "session-store", Reasons: []string{"NoEndpoints"}},
	)

	symptomOnly := runWith(finding("Service", "session-store", "NoEndpoints", scenario.SeverityCritical))
	causeOnly := runWith(finding("Pod", "session-store-abc", "CrashLoopBackOff", scenario.SeverityCritical))

	approx(t, Recall{}.Score(s, symptomOnly).Value, 0.5)
	approx(t, Recall{}.Score(s, causeOnly).Value, 0.5)

	approx(t, RootCause{}.Score(s, symptomOnly).Value, 0)
	approx(t, RootCause{}.Score(s, causeOnly).Value, 1)
}

// Reporting the symptom as well as the cause is the best answer, not a
// penalty. Only the cause's absence costs anything.
func TestRootCauseDoesNotPenaliseAlsoReportingTheSymptom(t *testing.T) {
	s := testScenario(
		scenario.ExpectedFinding{Kind: "Pod", Name: "session-store", Reasons: []string{"CrashLoopBackOff"}, Root: true},
		scenario.ExpectedFinding{Kind: "Service", Name: "session-store", Reasons: []string{"NoEndpoints"}},
	)
	both := runWith(
		finding("Pod", "session-store-abc", "CrashLoopBackOff", scenario.SeverityCritical),
		finding("Service", "session-store", "NoEndpoints", scenario.SeverityCritical),
	)
	approx(t, RootCause{}.Score(s, both).Value, 1)
	approx(t, Recall{}.Score(s, both).Value, 1)
}

// --- severity ---

func TestSeverityDistance(t *testing.T) {
	s := testScenario(scenario.ExpectedFinding{Kind: "Pod", Name: "x"})
	s.Severity = scenario.SeverityWarning

	for _, tc := range []struct {
		got  scenario.Severity
		want float64
		dir  string
	}{
		{scenario.SeverityWarning, 1, "exact"},
		{scenario.SeverityCritical, 1 - 1.0/3, "1 too high"},
		{scenario.SeverityInfo, 1 - 1.0/3, "1 too low"},
		{scenario.SeverityOK, 1 - 2.0/3, "2 too low"},
	} {
		run := runWith()
		run.Report.OverallSeverity = tc.got
		score := SeverityDistance{}.Score(s, run)
		approx(t, score.Value, tc.want)
		if !strings.Contains(score.Comment, tc.dir) {
			t.Errorf("severity %q: comment %q should say %q", tc.got, score.Comment, tc.dir)
		}
	}
}

// The direction matters because a systematically hot subject and a confused
// one need opposite fixes, and an exact-match score cannot tell them apart.
func TestSeverityDistanceRecordsTheDirection(t *testing.T) {
	s := testScenario()
	s.Severity = scenario.SeverityInfo
	run := runWith()
	run.Report.OverallSeverity = scenario.SeverityCritical
	got := SeverityDistance{}.Score(s, run)
	if !strings.Contains(got.Comment, "too high") {
		t.Errorf("comment = %q, want it to say the subject over-called", got.Comment)
	}
}

func TestSeverityIsSkippedWhenTheScenarioDeclaresNone(t *testing.T) {
	s := testScenario()
	s.Severity = ""
	if got := (SeverityDistance{}).Score(s, runWith()); !got.Skipped {
		t.Errorf("severity = %+v, want skipped", got)
	}
}

// An unparseable severity is a zero however far the scenario's own severity
// is from OK — an unknown value has no rank, so it must not be scored as one.
func TestSeverityScoresZeroForAnUnparseableValue(t *testing.T) {
	for _, expected := range []scenario.Severity{
		scenario.SeverityOK,
		scenario.SeverityInfo,
		scenario.SeverityWarning,
		scenario.SeverityCritical,
	} {
		s := testScenario()
		s.Severity = expected
		run := runWith()
		run.Report.OverallSeverity = "extremely bad"

		got := SeverityDistance{}.Score(s, run)
		if got.Skipped || got.Value != 0 {
			t.Errorf("severity against %s = %+v, want a hard zero", expected, got)
		}
		if !strings.Contains(got.Comment, "invalid severity") {
			t.Errorf("comment = %q, want it to say the value was unparseable", got.Comment)
		}
	}
}

// --- hallucination ---

func TestHallucinatedFaultChargesAnInventedFailureMode(t *testing.T) {
	s := testScenario(scenario.ExpectedFinding{Kind: "Pod", Name: "checkout-api", Reasons: []string{"ImagePullBackOff"}})
	got := HallucinatedFault{}.Score(s, runWith(
		finding("Pod", "checkout-api", "ImagePullBackOff", scenario.SeverityCritical),
		finding("Pod", "other", "OOMKilled", scenario.SeverityCritical),
	))
	approx(t, got.Value, 0.5)
	if !strings.Contains(got.Comment, "OOMKilled") {
		t.Errorf("comment should name the invention, got %q", got.Comment)
	}
}

// The reason a general precision metric is wrong: the scenario's manifests
// are minimal, so a subject noting a missing liveness probe or absent
// resource limits is *correct*. Marking it down for thoroughness would push
// us to prompt subjects to say less.
func TestHallucinatedFaultDoesNotPenaliseAdvisoryFindings(t *testing.T) {
	s := testScenario(scenario.ExpectedFinding{Kind: "Pod", Name: "checkout-api", Reasons: []string{"ImagePullBackOff"}})
	got := HallucinatedFault{}.Score(s, runWith(
		finding("Pod", "checkout-api", "ImagePullBackOff", scenario.SeverityCritical),
		finding("Deployment", "checkout-api", "NoResourceLimits", scenario.SeverityWarning),
		finding("Deployment", "checkout-api", "NoLivenessProbe", scenario.SeverityWarning),
	))
	approx(t, got.Value, 1)
}

// Info-level notes are observations, and subjects are asked to report
// observations.
func TestHallucinatedFaultIgnoresInfoLevelFindings(t *testing.T) {
	s := testScenario(scenario.ExpectedFinding{Kind: "Pod", Name: "checkout-api", Reasons: []string{"ImagePullBackOff"}})
	got := HallucinatedFault{}.Score(s, runWith(
		finding("Pod", "checkout-api", "ImagePullBackOff", scenario.SeverityCritical),
		finding("Pod", "other", "OOMKilled", scenario.SeverityInfo),
	))
	approx(t, got.Value, 1)
}

// This is what makes a healthy control cost something.
func TestHallucinatedFaultIsWhatMakesAControlCostSomething(t *testing.T) {
	control := testScenario()
	control.Faults = nil

	clean := HallucinatedFault{}.Score(control, runWith())
	approx(t, clean.Value, 1)

	invented := HallucinatedFault{}.Score(control, runWith(
		finding("Pod", "anything", "CrashLoopBackOff", scenario.SeverityCritical),
	))
	approx(t, invented.Value, 0)
}

func TestHallucinatedFaultScoresZeroWhenThereIsNoReport(t *testing.T) {
	got := HallucinatedFault{}.Score(testScenario(), Run{ScenarioID: "s-1"})
	if got.Skipped || got.Value != 0 {
		t.Errorf("hallucination with no report = %+v, want a hard zero", got)
	}
}

// --- timing ---

func TestTimeToDetect(t *testing.T) {
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	run := runWith()
	run.InjectedAt = base
	run.DetectedAt = base.Add(42 * time.Second)

	got := TimeToDetect{}.Score(testScenario(), run)
	approx(t, got.Value, 42)
	if got.Unit != UnitSeconds {
		t.Errorf("unit = %q, want %q", got.Unit, UnitSeconds)
	}
}

// Either timestamp missing makes the interval meaningless. Half of one is not
// a duration, and treating a zero time as an origin would produce fifty-six
// years of latency.
func TestTimeToDetectIsSkippedWithoutBothTimestamps(t *testing.T) {
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name               string
		injected, detected time.Time
	}{
		{"neither", time.Time{}, time.Time{}},
		{"no injection", time.Time{}, base},
		{"no detection", base, time.Time{}},
	} {
		run := runWith()
		run.InjectedAt = tc.injected
		run.DetectedAt = tc.detected
		got := TimeToDetect{}.Score(testScenario(), run)
		if !got.Skipped || got.Value != 0 {
			t.Errorf("%s: time_to_detect = %+v, want skipped with no value", tc.name, got)
		}
	}
}

// A report that predates the fault landing is a clock or harness problem, not
// a fast subject. Scored as a negative it would be the best run in the suite.
func TestTimeToDetectRefusesANegativeInterval(t *testing.T) {
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	run := runWith()
	run.InjectedAt = base
	run.DetectedAt = base.Add(-10 * time.Second)

	got := TimeToDetect{}.Score(testScenario(), run)
	if !got.Skipped {
		t.Fatalf("time_to_detect = %+v, want skipped", got)
	}
	if !strings.Contains(got.Comment, "inconsistent") {
		t.Errorf("comment = %q, want it to flag the inconsistency", got.Comment)
	}
}

// The reaper arriving to find nothing to clean up is the subject having fixed
// the fault, timestamped.
func TestTimeToRemediate(t *testing.T) {
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	run := runWith()
	run.InjectedAt = base
	run.ClearedAt = base.Add(3 * time.Minute)

	got := TimeToRemediate{}.Score(testScenario(), run)
	approx(t, got.Value, 180)
}

// A subject that fixed nothing has no remediation time. Scoring that as zero
// would make doing nothing look instantaneous — the best possible result.
func TestTimeToRemediateIsSkippedWhenNothingWasFixed(t *testing.T) {
	run := runWith()
	run.InjectedAt = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	got := TimeToRemediate{}.Score(testScenario(), run)
	if !got.Skipped || got.Value != 0 {
		t.Errorf("time_to_remediate = %+v, want skipped with no value", got)
	}
}

// A clear with nothing to measure it from is not a fifty-six-year
// remediation, which is what the zero time would make of it.
func TestTimeToRemediateIsSkippedWithoutAnInjectionTimestamp(t *testing.T) {
	run := runWith()
	run.ClearedAt = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	got := TimeToRemediate{}.Score(testScenario(), run)
	if !got.Skipped || got.Value != 0 {
		t.Errorf("time_to_remediate = %+v, want skipped with no value", got)
	}
}

// The same inconsistency check as detection: a fault cleared before it landed
// is a harness problem, and zero seconds would be the best score in the suite.
func TestTimeToRemediateRefusesANegativeInterval(t *testing.T) {
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	run := runWith()
	run.InjectedAt = base
	run.ClearedAt = base.Add(-time.Minute)

	got := TimeToRemediate{}.Score(testScenario(), run)
	if !got.Skipped {
		t.Fatalf("time_to_remediate = %+v, want skipped", got)
	}
	if !strings.Contains(got.Comment, "inconsistent") {
		t.Errorf("comment = %q, want it to flag the inconsistency", got.Comment)
	}
}
