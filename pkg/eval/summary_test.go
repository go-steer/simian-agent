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
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/go-steer/simian-agent/pkg/scenario"
)

func packScenario(id string, expect ...scenario.ExpectedFinding) scenario.Scenario {
	s := testScenario(expect...)
	s.ID = id
	s.Name = id
	return s
}

func controlScenario(id string) scenario.Scenario {
	s := packScenario(id)
	s.Faults = nil
	s.Severity = scenario.SeverityOK
	return s
}

func TestSummarizeAveragesEachMeasureOverTheRunsWhereItApplied(t *testing.T) {
	expect := scenario.ExpectedFinding{Kind: "Pod", Name: "checkout-api", Reasons: []string{"ImagePullBackOff"}}
	pack := scenario.Pack{Name: "parity", Scenarios: []scenario.Scenario{
		packScenario("s-a", expect),
		packScenario("s-b", expect),
	}}

	hit := runWith(finding("Pod", "checkout-api-1", "ImagePullBackOff", scenario.SeverityCritical))
	hit.ScenarioID = "s-a"
	hit.Manifested = true

	miss := runWith()
	miss.ScenarioID = "s-b"
	miss.Manifested = true

	sum, err := Summarize("agent-under-test", pack, []Run{hit, miss})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}

	approx(t, sum.Means[MeasureRecall], 0.5)
	approx(t, sum.Means[MeasureSeverity], 1)
	approx(t, sum.EfficacyRate, 1)
	if sum.Subject != "agent-under-test" || sum.Pack != "parity" {
		t.Errorf("summary = %+v, want the subject and pack carried through", sum)
	}
	if sum.Scenarios != 2 || sum.Manifested != 2 || sum.InjectFailures != 0 {
		t.Errorf("counts = %d/%d/%d, want 2/2/0", sum.Scenarios, sum.Manifested, sum.InjectFailures)
	}
}

// A skipped measure is excluded from its mean, not counted as zero. Averaging
// "not applicable" as zero would mark a subject down for scenarios that never
// asked the question.
func TestSummarizeExcludesSkippedMeasuresFromTheMean(t *testing.T) {
	rooted := packScenario("s-root",
		scenario.ExpectedFinding{Kind: "Pod", Name: "session-store", Reasons: []string{"CrashLoopBackOff"}, Root: true},
	)
	flat := packScenario("s-flat",
		scenario.ExpectedFinding{Kind: "Pod", Name: "checkout-api", Reasons: []string{"ImagePullBackOff"}},
	)
	pack := scenario.Pack{Name: "parity", Scenarios: []scenario.Scenario{rooted, flat}}

	hit := runWith(finding("Pod", "session-store-1", "CrashLoopBackOff", scenario.SeverityCritical))
	hit.ScenarioID = "s-root"
	hit.Manifested = true

	// This run scores 0 on recall, and root_cause does not apply to it at all.
	blank := runWith()
	blank.ScenarioID = "s-flat"
	blank.Manifested = true

	sum, err := Summarize("subject", pack, []Run{hit, blank})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}

	approx(t, sum.Means[MeasureRecall], 0.5)
	// Only s-root marks a root, and it was named: 1.0, not the 0.5 a
	// zero-for-skipped would give.
	approx(t, sum.Means[MeasureRootCause], 1)
}

// A measure skipped on every run has no mean at all, rather than a zero that
// reads like a failing grade.
func TestAMeasureSkippedEverywhereIsAbsentFromTheMeans(t *testing.T) {
	pack := scenario.Pack{Name: "parity", Scenarios: []scenario.Scenario{
		packScenario("s-a", scenario.ExpectedFinding{Kind: "Pod", Name: "checkout-api"}),
	}}
	run := runWith()
	run.ScenarioID = "s-a"

	sum, err := Summarize("subject", pack, []Run{run})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if _, ok := sum.Means[MeasureTimeToRemediate]; ok {
		t.Errorf("time_to_remediate has a mean of %v; nothing was remediated", sum.Means[MeasureTimeToRemediate])
	}
	if slices.Contains(sum.MeasureNames(), MeasureTimeToRemediate) {
		t.Errorf("MeasureNames = %v, want time_to_remediate absent", sum.MeasureNames())
	}
}

// A run naming a scenario the pack does not contain is an error, not a
// silently dropped row: dropping it shrinks the denominator and quietly
// improves every mean.
func TestSummarizeRejectsARunForAnUnknownScenario(t *testing.T) {
	pack := scenario.Pack{Name: "parity", Scenarios: []scenario.Scenario{packScenario("s-a")}}
	run := runWith()
	run.ScenarioID = "s-typo"

	sum, err := Summarize("subject", pack, []Run{run})
	if err == nil {
		t.Fatal("Summarize accepted a run for a scenario not in the pack")
	}
	if !strings.Contains(err.Error(), "s-typo") {
		t.Errorf("error = %v, want it to name the unknown scenario", err)
	}
	// Nothing partial comes back with the error. A summary carrying the rows
	// that happened to be processed before the bad one is a report of a suite
	// that was never run.
	if !reflect.DeepEqual(sum, Summary{}) {
		t.Errorf("summary = %+v, want the zero value alongside the error", sum)
	}
}

// The efficacy rate is the harness grading itself, so a fault that did not
// land has to show up in it even though the run is otherwise unscored.
func TestEfficacyRateCountsFaultsThatDidNotLand(t *testing.T) {
	expect := scenario.ExpectedFinding{Kind: "Pod", Name: "checkout-api", Reasons: []string{"ImagePullBackOff"}}
	pack := scenario.Pack{Name: "parity", Scenarios: []scenario.Scenario{
		packScenario("s-a", expect),
		packScenario("s-b", expect),
		packScenario("s-c", expect),
		packScenario("s-d", expect),
	}}

	var runs []Run
	for _, id := range []string{"s-a", "s-b", "s-c"} {
		r := runWith(finding("Pod", "checkout-api-1", "ImagePullBackOff", scenario.SeverityCritical))
		r.ScenarioID = id
		r.Manifested = true
		runs = append(runs, r)
	}
	inert := runWith()
	inert.ScenarioID = "s-d"
	inert.Manifested = false
	runs = append(runs, inert)

	sum, err := Summarize("subject", pack, runs)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	approx(t, sum.EfficacyRate, 0.75)
	if sum.Manifested != 3 {
		t.Errorf("manifested = %d, want 3", sum.Manifested)
	}
}

// A control has no fault to manifest. Counting it in the denominator would cap
// the efficacy rate below 1 on every pack that has controls — and a pack
// without controls cannot measure invention at all.
func TestControlsAreOutsideTheEfficacyDenominator(t *testing.T) {
	expect := scenario.ExpectedFinding{Kind: "Pod", Name: "checkout-api", Reasons: []string{"ImagePullBackOff"}}
	pack := scenario.Pack{Name: "parity", Scenarios: []scenario.Scenario{
		packScenario("s-a", expect),
		controlScenario("s-control"),
	}}

	hit := runWith(finding("Pod", "checkout-api-1", "ImagePullBackOff", scenario.SeverityCritical))
	hit.ScenarioID = "s-a"
	hit.Manifested = true

	control := runWith()
	control.ScenarioID = "s-control"
	control.Report.OverallSeverity = scenario.SeverityOK
	control.Manifested = false

	sum, err := Summarize("subject", pack, []Run{hit, control})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	approx(t, sum.EfficacyRate, 1)
}

// A suite of nothing but controls has no efficacy rate to report. Zero would
// read as a totally broken harness.
func TestAllControlsLeaveTheEfficacyRateAtZeroWithNoInjectables(t *testing.T) {
	pack := scenario.Pack{Name: "controls", Scenarios: []scenario.Scenario{controlScenario("s-control")}}
	run := runWith()
	run.ScenarioID = "s-control"
	run.Report.OverallSeverity = scenario.SeverityOK

	sum, err := Summarize("subject", pack, []Run{run})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sum.EfficacyRate != 0 {
		t.Errorf("efficacy rate = %v, want 0 with nothing injectable", sum.EfficacyRate)
	}
	if sum.Scenarios != 1 {
		t.Errorf("scenarios = %d, want 1", sum.Scenarios)
	}
}

// An inject failure is visible on the row as well as in the count, so a reader
// can tell a row of skips that means "not applicable" from one that means "the
// harness broke".
func TestScenarioResultCarriesTheInjectFailure(t *testing.T) {
	pack := scenario.Pack{Name: "parity", Scenarios: []scenario.Scenario{
		packScenario("s-a", scenario.ExpectedFinding{Kind: "Pod", Name: "checkout-api"}),
	}}
	run := runWith()
	run.ScenarioID = "s-a"
	run.InjectError = "quota exceeded"

	sum, err := Summarize("subject", pack, []Run{run})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sum.InjectFailures != 1 {
		t.Errorf("inject failures = %d, want 1", sum.InjectFailures)
	}
	if len(sum.Results) != 1 || sum.Results[0].InjectError != "quota exceeded" {
		t.Fatalf("results = %+v, want the inject error on the row", sum.Results)
	}
	if len(sum.Means) != 0 {
		t.Errorf("means = %v, want none; the only run was unscored", sum.Means)
	}
	if _, ok := sum.Results[0].Score(MeasureRecall); !ok {
		t.Error("the row should still carry a recall entry, skipped")
	}
}

func TestScenarioResultScoreLookup(t *testing.T) {
	res := ScenarioResult{Scores: []Score{{Name: MeasureRecall, Value: 0.5}}}
	got, ok := res.Score(MeasureRecall)
	if !ok || got.Value != 0.5 {
		t.Errorf("Score(recall) = %+v, %v", got, ok)
	}
	if _, ok := res.Score(MeasureSeverity); ok {
		t.Error("Score returned a measure that is not on the row")
	}
}

func TestMeasureNamesAreInReportOrder(t *testing.T) {
	sum := Summary{Means: map[string]float64{
		MeasureSeverity:      1,
		MeasureRecall:        1,
		MeasureHallucination: 1,
	}}
	got := sum.MeasureNames()
	want := []string{MeasureRecall, MeasureSeverity, MeasureHallucination}
	if !slices.Equal(got, want) {
		t.Errorf("MeasureNames() = %v, want %v", got, want)
	}
}

// Summarizing nothing is not an error; it is an empty suite.
func TestSummarizeWithNoRuns(t *testing.T) {
	sum, err := Summarize("subject", scenario.Pack{Name: "parity"}, nil)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sum.Scenarios != 0 || len(sum.Results) != 0 || len(sum.Means) != 0 || sum.EfficacyRate != 0 {
		t.Errorf("summary = %+v, want empty", sum)
	}
}
