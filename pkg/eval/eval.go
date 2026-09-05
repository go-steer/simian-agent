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

// Package eval scores what a subject reported against what Simian broke.
//
// # Scoring is pure
//
// Nothing in this package touches a cluster, a clock or a network. A Run
// carries everything scoring needs — the expectations, the report, the
// timestamps, and whether the fault actually landed — so the same inputs
// always produce the same scores. That is what lets `simian evaluate` score a
// run offline from artifacts, hours later, on a different machine, and get
// the number the live rig got.
//
// # Simian does not import the subject
//
// There is no dependency on mast, core-agent or core-sre-agent, in either
// direction. An adversary that shares a prompt template, a model client or a
// Kubernetes client version with the subject can fail in a correlated way and
// produce an eval that passes for the wrong reason. Report is Simian's own
// shape; adapters translate into it on Simian's side of the fence.
//
// # Four measures are copied, three are new
//
// Recall, root cause, severity and hallucination are deliberately the same
// measures core-sre-agent's own eval tier uses, scored the same way, so the
// two rigs produce comparable numbers. Time to detect, time to remediate and
// efficacy rate are the ones only the adversary can supply, because only the
// adversary knows when the fault was injected and whether it landed at all.
package eval

import (
	"context"
	"time"

	"github.com/go-steer/simian-agent/pkg/scenario"
)

// Report is what a subject produced about a scenario.
//
// It mirrors the machine-stable triple and nothing else. Prose — titles,
// summaries, recommended actions — is deliberately absent, because grading
// prose needs a judge and that is a different tier of evaluation.
type Report struct {
	Findings        []scenario.Finding `json:"findings"`
	OverallSeverity scenario.Severity  `json:"overall_severity,omitempty"`
}

// Subject is a thing that can be asked to investigate and produce a report.
//
// The interface is this small on purpose: everything Simian wants to grade
// has to fit through it, and anything that does not fit is a signal that the
// measure is reaching into the subject rather than observing it.
type Subject interface {
	Name() string
	Investigate(ctx context.Context, prompt string) (Report, error)
}

// Run is one scenario executed against one subject: everything scoring needs
// and nothing that requires a cluster.
type Run struct {
	ScenarioID string `json:"scenario_id"`
	Subject    string `json:"subject"`

	// Report is what the subject produced, or nil if it produced nothing
	// structured. Nil is a score of zero, not a skip: a subject that cannot
	// emit a parseable report has failed the task.
	Report *Report `json:"report,omitempty"`

	// InjectError is set when Simian failed to break the cluster.
	//
	// This is the harness's own failure and is never scored as a miss. A
	// fault that did not land grades the subject on a cluster that was never
	// broken, and reporting that as "the agent missed a network partition"
	// when there was no network partition is a confident wrong number — worse
	// than no measurement. Kept distinct from SubjectError for the same
	// reason sre-eval-live keeps them distinct.
	InjectError string `json:"inject_error,omitempty"`

	// SubjectError is set when the subject itself failed: crashed, timed out,
	// returned something unparseable. This *is* the subject's problem and is
	// scored as a zero, not skipped.
	SubjectError string `json:"subject_error,omitempty"`

	// Manifested reports whether every fault's Settle gate passed. It feeds
	// the efficacy rate, which is the harness grading itself.
	Manifested bool `json:"manifested"`

	// InjectedAt is when the last fault's efficacy gate passed — the first
	// moment the cluster was observably broken, and therefore the earliest a
	// subject could honestly have detected anything. Simian's own timestamp,
	// not one inferred from the subject's output.
	InjectedAt time.Time `json:"injected_at,omitempty"`

	// DetectedAt is when the subject's report came back.
	DetectedAt time.Time `json:"detected_at,omitempty"`

	// ClearedAt is when Simian observed the fault already gone — the reaper
	// arriving to clean up and finding nothing to clean.
	//
	// That is not an error. On a run where the subject is allowed to write,
	// it is the subject having *fixed* the fault, timestamped. The lease
	// reaper was built to stop Simian leaking faults into a cluster; it
	// becomes a measuring instrument the moment the subject can act.
	ClearedAt time.Time `json:"cleared_at,omitempty"`
}

// Unit says how to read a Score's value.
type Unit string

// Score units.
const (
	// UnitFraction is a 0..1 score where 1 is a perfect answer.
	UnitFraction Unit = "fraction"

	// UnitSeconds is an elapsed time. Lower is better, and there is no
	// ceiling, so it is reported rather than averaged into a grade.
	UnitSeconds Unit = "seconds"
)

// Score is one measure's verdict on one run.
type Score struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
	Unit  Unit    `json:"unit"`

	// Skipped marks a measure that does not apply to this scenario — not a
	// zero. A skipped measure is excluded from every mean, because averaging
	// "not applicable" as zero would drag a subject down for scenarios that
	// never asked the question.
	Skipped bool `json:"skipped,omitempty"`

	// Comment is the human-readable why. It carries what was missed and in
	// which direction, because a number alone does not tell you which way to
	// fix the subject.
	Comment string `json:"comment,omitempty"`
}

// Measure is one scoring rule.
type Measure interface {
	Name() string
	Score(s scenario.Scenario, run Run) Score
}

// Measure names, exported so callers can look one up without a string
// literal that will not be checked.
const (
	MeasureRecall          = "recall"
	MeasureRootCause       = "root_cause"
	MeasureSeverity        = "severity"
	MeasureHallucination   = "hallucinated_fault"
	MeasureTimeToDetect    = "time_to_detect"
	MeasureTimeToRemediate = "time_to_remediate"
)

// DefaultMeasures returns the six per-run measures in report order.
//
// Efficacy rate is not here: it is a property of a whole suite rather than of
// one run, and lives on Summary.
func DefaultMeasures() []Measure {
	return []Measure{
		Recall{},
		RootCause{},
		SeverityDistance{},
		HallucinatedFault{},
		TimeToDetect{},
		TimeToRemediate{},
	}
}

// ScoreRun applies every default measure to one run.
//
// A run whose injection failed is skipped wholesale rather than scored: the
// cluster was never broken, so there was nothing for the subject to find and
// no measure has anything to say.
func ScoreRun(s scenario.Scenario, run Run) []Score {
	measures := DefaultMeasures()
	out := make([]Score, 0, len(measures))
	for _, m := range measures {
		if run.InjectError != "" {
			out = append(out, Score{
				Name:    m.Name(),
				Skipped: true,
				Comment: "injection failed: " + run.InjectError,
			})
			continue
		}
		out = append(out, m.Score(s, run))
	}
	return out
}
