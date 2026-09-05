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
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/simian-agent/pkg/scenario"
)

// scoredRun is a fully populated run used by the purity and inject-error
// tests, where the point is to exercise every measure at once rather than any
// one of them.
func scoredRun() (scenario.Scenario, Run) {
	s := testScenario(
		scenario.ExpectedFinding{Kind: "Pod", Name: "session-store", Reasons: []string{"CrashLoopBackOff"}, Root: true},
		scenario.ExpectedFinding{Kind: "Service", Name: "session-store", Reasons: []string{"NoEndpoints"}},
	)
	base := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	run := runWith(
		finding("Pod", "session-store-abc", "CrashLoopBackOff", scenario.SeverityCritical),
		finding("Pod", "unrelated", "OOMKilled", scenario.SeverityCritical),
	)
	run.Subject = "test-subject"
	run.InjectedAt = base
	run.DetectedAt = base.Add(30 * time.Second)
	run.ClearedAt = base.Add(2 * time.Minute)
	return s, run
}

// The first acceptance criterion: scoring is pure. Same inputs, same scores,
// no cluster access, no clock. This is what lets `simian evaluate` reproduce a
// live run's numbers offline from artifacts.
func TestScoringIsPure(t *testing.T) {
	s, run := scoredRun()

	first := ScoreRun(s, run)
	for i := range 20 {
		got := ScoreRun(s, run)
		if !equalScores(first, got) {
			t.Fatalf("iteration %d differed:\n first=%+v\n   got=%+v", i, first, got)
		}
	}

	// Nothing in a Run is read from the ambient environment, so a Run that
	// round-trips through JSON — which is how `simian evaluate` will receive
	// it — scores identically.
	blob, err := json.Marshal(run)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded Run
	if err := json.Unmarshal(blob, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !equalScores(first, ScoreRun(s, decoded)) {
		t.Errorf("scores changed across a JSON round trip:\n before=%+v\n  after=%+v", first, ScoreRun(s, decoded))
	}
}

// Scoring must not mutate what it is given, or a second pass over the same
// artifacts produces different numbers.
func TestScoringDoesNotMutateItsInputs(t *testing.T) {
	s, run := scoredRun()
	before, err := json.Marshal(struct {
		S scenario.Scenario
		R Run
	}{s, run})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	ScoreRun(s, run)

	after, err := json.Marshal(struct {
		S scenario.Scenario
		R Run
	}{s, run})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("ScoreRun mutated its inputs:\n before=%s\n  after=%s", before, after)
	}
}

func equalScores(a, b []Score) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The second acceptance criterion: an injection failure is the harness's
// failure, recorded separately from the subject's, and never scored as a miss.
//
// The alternative is the worst number this package could produce: "the agent
// missed a crash loop" on a cluster where nothing ever crashed.
func TestAnInjectionFailureIsNeverScoredAsAMiss(t *testing.T) {
	s, run := scoredRun()
	run.Report = nil
	run.Manifested = false
	run.InjectError = "apply deployment: nodes are full"

	scores := ScoreRun(s, run)
	if len(scores) != len(DefaultMeasures()) {
		t.Fatalf("got %d scores, want %d", len(scores), len(DefaultMeasures()))
	}
	for _, sc := range scores {
		if !sc.Skipped {
			t.Errorf("%s = %v, want skipped", sc.Name, sc.Value)
		}
		if sc.Value != 0 {
			t.Errorf("%s carries value %v; a skipped measure should carry none", sc.Name, sc.Value)
		}
		if !strings.Contains(sc.Comment, "nodes are full") {
			t.Errorf("%s comment = %q, want the injection error", sc.Name, sc.Comment)
		}
	}
}

// A run whose injection succeeded is scored normally even if it produced
// nothing else, so the skip above is attributable to InjectError alone.
func TestAnInjectionFailureIsTheOnlyThingThatSkipsEverything(t *testing.T) {
	s, run := scoredRun()
	run.InjectError = ""

	for _, sc := range ScoreRun(s, run) {
		if sc.Skipped {
			t.Errorf("%s was skipped on a run that injected cleanly: %s", sc.Name, sc.Comment)
		}
	}
}

// The subject's own failure is the subject's problem: a zero, not a skip.
// Skipping it would let a subject improve its mean by crashing.
func TestASubjectFailureIsScoredAsAZero(t *testing.T) {
	s, run := scoredRun()
	run.Report = nil
	run.SubjectError = "context deadline exceeded"

	scores := ScoreRun(s, run)
	for _, name := range []string{MeasureRecall, MeasureRootCause, MeasureSeverity, MeasureHallucination} {
		sc, ok := scoreNamed(scores, name)
		if !ok {
			t.Fatalf("no %s score", name)
		}
		if sc.Skipped || sc.Value != 0 {
			t.Errorf("%s = %+v, want a hard zero", name, sc)
		}
	}
}

func scoreNamed(scores []Score, name string) (Score, bool) {
	for _, s := range scores {
		if s.Name == name {
			return s, true
		}
	}
	return Score{}, false
}

func TestScoreRunReturnsEveryMeasureOnceInOrder(t *testing.T) {
	s, run := scoredRun()
	scores := ScoreRun(s, run)

	measures := DefaultMeasures()
	if len(scores) != len(measures) {
		t.Fatalf("got %d scores for %d measures", len(scores), len(measures))
	}
	for i, m := range measures {
		if scores[i].Name != m.Name() {
			t.Errorf("score %d = %q, want %q", i, scores[i].Name, m.Name())
		}
	}
}

// Efficacy rate is the seventh measure and is deliberately absent from the
// per-run set, because it is a property of a suite.
func TestDefaultMeasuresAreTheSixPerRunOnes(t *testing.T) {
	want := []string{
		MeasureRecall,
		MeasureRootCause,
		MeasureSeverity,
		MeasureHallucination,
		MeasureTimeToDetect,
		MeasureTimeToRemediate,
	}
	measures := DefaultMeasures()
	if len(measures) != len(want) {
		t.Fatalf("got %d measures, want %d", len(measures), len(want))
	}
	for i, m := range measures {
		if m.Name() != want[i] {
			t.Errorf("measure %d = %q, want %q", i, m.Name(), want[i])
		}
		if m.Name() == MeasureEfficacyRate {
			t.Errorf("efficacy rate is a suite measure and must not be scored per run")
		}
	}
}

// fakeSubject is the smallest thing that satisfies Subject, and its size is
// the point: everything Simian grades has to fit through two methods.
type fakeSubject struct {
	name    string
	report  Report
	gotPrmt string
}

func (f *fakeSubject) Name() string { return f.name }

func (f *fakeSubject) Investigate(_ context.Context, prompt string) (Report, error) {
	f.gotPrmt = prompt
	return f.report, nil
}

// The wiring a live runner will do: ask the subject, put its report on a Run,
// score the Run. Nothing in between needs the subject again, which is what
// keeps scoring offline-reproducible.
func TestASubjectsReportScoresThroughARun(t *testing.T) {
	s := testScenario(scenario.ExpectedFinding{Kind: "Pod", Name: "checkout-api", Reasons: []string{"ImagePullBackOff"}})
	subject := &fakeSubject{
		name: "core-sre-agent",
		report: Report{
			Findings:        []scenario.Finding{finding("Pod", "checkout-api-1", "ImagePullBackOff", scenario.SeverityCritical)},
			OverallSeverity: scenario.SeverityCritical,
		},
	}

	report, err := subject.Investigate(context.Background(), s.Prompt)
	if err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if subject.gotPrmt != s.Prompt {
		t.Errorf("subject saw prompt %q, want the scenario's", subject.gotPrmt)
	}

	run := Run{ScenarioID: s.ID, Subject: subject.Name(), Report: &report, Manifested: true}
	got, ok := scoreNamed(ScoreRun(s, run), MeasureRecall)
	if !ok || got.Value != 1 {
		t.Errorf("recall = %+v, want 1", got)
	}
}

// Every fraction measure has to say so, because a reader averaging seconds
// into a grade would be reporting nonsense.
func TestEveryScoreDeclaresItsUnit(t *testing.T) {
	s, run := scoredRun()
	for _, sc := range ScoreRun(s, run) {
		switch sc.Unit {
		case UnitFraction:
			if sc.Value < 0 || sc.Value > 1 {
				t.Errorf("%s = %v, outside 0..1", sc.Name, sc.Value)
			}
		case UnitSeconds:
			if sc.Value < 0 {
				t.Errorf("%s = %v seconds", sc.Name, sc.Value)
			}
		default:
			t.Errorf("%s declares no unit", sc.Name)
		}
	}
}
