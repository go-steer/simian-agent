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
	"fmt"
	"sort"
	"strings"

	"github.com/go-steer/simian-agent/pkg/scenario"
)

// noReport is the comment every graded measure uses when the subject produced
// nothing structured, so the four parity measures agree on what that is worth.
const noReport = "subject produced no structured report"

// scenarioNamespace returns the namespace a scenario is about, or "" if its
// faults do not agree on one.
//
// Findings about another namespace are discarded rather than graded: the
// prompt named one namespace, and a cluster-wide note is not what is being
// measured. When the scenario spans namespaces there is nothing to scope
// against, so nothing is discarded.
func scenarioNamespace(s scenario.Scenario) string {
	ns := ""
	for _, f := range s.Faults {
		for _, t := range f.Targets {
			if t.Namespace == "" {
				continue
			}
			if ns == "" {
				ns = t.Namespace
				continue
			}
			if ns != t.Namespace {
				return ""
			}
		}
	}
	return ns
}

// inScope reports whether a finding is about the scenario's namespace. An
// empty namespace on the finding is accepted: the subject was asked about one
// namespace, so omitting it is terse, not wrong.
func inScope(ns string, f scenario.Finding) bool {
	return ns == "" || f.Namespace == "" || f.Namespace == ns
}

// matchesAny reports whether any in-scope finding satisfies the expectation.
func matchesAny(ns string, e scenario.ExpectedFinding, findings []scenario.Finding) bool {
	for _, f := range findings {
		if !inScope(ns, f) {
			continue
		}
		if e.Matches(f) {
			return true
		}
	}
	return false
}

// scoreCoverage is the shared body of Recall and RootCause: what fraction of
// a set of expectations the report satisfied, with the misses named.
func scoreCoverage(name string, ns string, want []scenario.ExpectedFinding, findings []scenario.Finding, noun string) Score {
	var found, missed []string
	for _, e := range want {
		label := fmt.Sprintf("%s/%s", e.Kind, e.Name)
		if matchesAny(ns, e, findings) {
			found = append(found, label)
		} else {
			missed = append(missed, label)
		}
	}
	sort.Strings(missed)

	comment := fmt.Sprintf("%s %d/%d", noun, len(found), len(want))
	if len(missed) > 0 {
		comment += ", missed=" + strings.Join(missed, ",")
	}
	return Score{
		Name:    name,
		Value:   float64(len(found)) / float64(len(want)),
		Unit:    UnitFraction,
		Comment: comment,
	}
}

// Recall is the fraction of a scenario's expected findings the subject
// actually reported.
//
// This is the number the live tier exists to produce, and the one a
// prompt-based eval cannot pose: when the fault is stated in the prompt,
// "did the subject find it" is not a question the dataset can ask. Here the
// fault is only in the cluster, so a miss means the subject looked in the
// wrong place or read the wrong thing.
type Recall struct{}

// Name implements Measure.
func (Recall) Name() string { return MeasureRecall }

// Score implements Measure.
func (Recall) Score(s scenario.Scenario, run Run) Score {
	if len(s.Expect) == 0 {
		return Score{Name: MeasureRecall, Skipped: true, Unit: UnitFraction, Comment: "healthy control; nothing to recall"}
	}
	if run.Report == nil {
		return Score{Name: MeasureRecall, Value: 0, Unit: UnitFraction, Comment: noReport}
	}
	return scoreCoverage(MeasureRecall, scenarioNamespace(s), s.Expect, run.Report.Findings, "found")
}

// RootCause is the fraction of a scenario's root-cause expectations the
// subject named, on the scenarios where a cause and its downstream symptom
// are both expected.
//
// # Why recall cannot express this
//
// On a scenario with a cause and a symptom, recall gives 0.5 to a subject
// that reported only the symptom and 0.5 to one that reported only the cause.
// Those are not the same answer. The symptom is what the operator already saw
// — it is why they opened the incident — and the cause is the thing they have
// to change.
//
// Reporting the symptom too is not penalised. Reporting both is the best
// answer: the symptom is how the problem is recognised and the cause is how
// it is fixed. Only the cause's absence costs anything.
//
// Skipped on every scenario that marks no root. Marking the single expectation
// of a single-fault scenario as its own root would make this a second copy of
// recall on most of the pack and drown the scenarios where the distinction is
// real.
type RootCause struct{}

// Name implements Measure.
func (RootCause) Name() string { return MeasureRootCause }

// Score implements Measure.
func (RootCause) Score(s scenario.Scenario, run Run) Score {
	roots := s.Roots()
	if len(roots) == 0 {
		return Score{Name: MeasureRootCause, Skipped: true, Unit: UnitFraction, Comment: "scenario marks no root cause"}
	}
	if run.Report == nil {
		return Score{Name: MeasureRootCause, Value: 0, Unit: UnitFraction, Comment: noReport}
	}
	return scoreCoverage(MeasureRootCause, scenarioNamespace(s), roots, run.Report.Findings, "named")
}

// severityLevels is the number of steps between the extreme severities, which
// is what a distance is divided by to land in 0..1. Four severities, three
// gaps.
const severityLevels = 3

// severityRank mirrors the ordering in pkg/scenario. It is duplicated rather
// than exported from there because scoring needs the numeric distance between
// two severities, not just their order, and a distance is a scoring concept.
var severityRank = map[scenario.Severity]int{
	scenario.SeverityOK:       0,
	scenario.SeverityInfo:     1,
	scenario.SeverityWarning:  2,
	scenario.SeverityCritical: 3,
}

// SeverityDistance grades the report-level severity by how far off it was,
// rather than by exact match.
//
// Distance because the observed misses are one-directional: exact match
// scores a systematically hot subject the same as a confused one, and those
// call for opposite fixes. The direction is in the comment for that reason.
// Over-calling is the lesser sin, but it is still a miss — a subject that
// grades everything critical has stopped discriminating.
type SeverityDistance struct{}

// Name implements Measure.
func (SeverityDistance) Name() string { return MeasureSeverity }

// Score implements Measure.
func (SeverityDistance) Score(s scenario.Scenario, run Run) Score {
	if s.Severity == "" {
		return Score{Name: MeasureSeverity, Skipped: true, Unit: UnitFraction, Comment: "scenario declares no expected severity"}
	}
	if run.Report == nil {
		return Score{Name: MeasureSeverity, Value: 0, Unit: UnitFraction, Comment: noReport}
	}

	got := run.Report.OverallSeverity
	if !got.Valid() {
		return Score{
			Name:    MeasureSeverity,
			Value:   0,
			Unit:    UnitFraction,
			Comment: fmt.Sprintf("invalid severity %q; expected %s", got, s.Severity),
		}
	}

	delta := severityRank[got] - severityRank[s.Severity]
	dist := delta
	if dist < 0 {
		dist = -dist
	}
	dir := "exact"
	switch {
	case delta > 0:
		dir = fmt.Sprintf("%d too high", delta)
	case delta < 0:
		dir = fmt.Sprintf("%d too low", -delta)
	}
	return Score{
		Name:    MeasureSeverity,
		Value:   1 - float64(dist)/severityLevels,
		Unit:    UnitFraction,
		Comment: fmt.Sprintf("actual=%s expected=%s (%s)", got, s.Severity, dir),
	}
}

// HallucinatedFault checks that the subject did not claim a failure mode the
// cluster does not have.
//
// # Why this is not a general precision metric
//
// The obvious counterpart to recall is "what fraction of findings were
// expected", and it is the wrong metric here. A scenario's manifests are
// minimal — no liveness probes, no PodDisruptionBudgets, no resource limits
// on the deliberately-broken workloads. A subject that notes those is
// *correct*, and a precision score would mark it down for thoroughness, which
// would push us to prompt subjects to say less. That is the opposite of what
// an operator wants.
//
// So exactly one class of finding is penalised: claiming one of the concrete
// failure modes the fault kinds know how to inject, in a scenario where that
// mode was not injected. Calling a Pending pod a CrashLoopBackOff is a
// misdiagnosis. Noting that it also has no resource limits is not.
//
// This is what makes a healthy control cost something, and it doubles as a
// misdiagnosis check on the broken ones.
//
// # What counts as injected
//
// The set of injected failure modes is read from the scenario's ground truth
// rather than from a table here, so a new scenario cannot forget to register
// itself. It reads both Expect and AlsoTrue: a fault often produces true
// consequences beyond the one the subject is asked for, and those are still
// true. See scenario.Scenario.AlsoTrue for why they are not simply extra
// expectations.
type HallucinatedFault struct{}

// Name implements Measure.
func (HallucinatedFault) Name() string { return MeasureHallucination }

// Score implements Measure.
func (HallucinatedFault) Score(s scenario.Scenario, run Run) Score {
	if run.Report == nil {
		return Score{Name: MeasureHallucination, Value: 0, Unit: UnitFraction, Comment: noReport}
	}

	// The families this scenario actually injected, taken from its own ground
	// truth so that adding a scenario cannot forget to update a second table.
	//
	// Both halves of the ground truth, because required and true are
	// different sets. Expect names the findings a correct report must contain;
	// AlsoTrue names the ones it may contain without being wrong — a stuck
	// rollout really does leave a pod crash-looping, and reading only Expect
	// charged that observation as an invention.
	injected := map[string]bool{}
	for _, e := range s.Expect {
		for _, r := range e.Reasons {
			if fam := familyOf(r); fam != "" {
				injected[fam] = true
			}
		}
	}
	for _, r := range s.AlsoTrue {
		if fam := familyOf(r); fam != "" {
			injected[fam] = true
		}
	}

	ns := scenarioNamespace(s)
	var claimed, bogus []string
	for _, f := range run.Report.Findings {
		// Only failures are graded. Info-level notes are observations, and a
		// subject is asked to report observations.
		if !f.Severity.AtLeast(scenario.SeverityWarning) {
			continue
		}
		if !inScope(ns, f) {
			continue
		}
		fam := familyOf(f.Reason)
		if fam == "" {
			continue
		}
		claimed = append(claimed, fam)
		if !injected[fam] {
			bogus = append(bogus, fmt.Sprintf("%s(%s/%s)", f.Reason, f.Kind, f.ResourceName))
		}
	}

	if len(claimed) == 0 {
		return Score{
			Name:    MeasureHallucination,
			Value:   1,
			Unit:    UnitFraction,
			Comment: "claimed no concrete failure mode",
		}
	}
	comment := fmt.Sprintf("%d/%d failure claims are real", len(claimed)-len(bogus), len(claimed))
	if len(bogus) > 0 {
		comment += ", invented=" + strings.Join(bogus, ",")
	}
	return Score{
		Name:    MeasureHallucination,
		Value:   1 - float64(len(bogus))/float64(len(claimed)),
		Unit:    UnitFraction,
		Comment: comment,
	}
}

// TimeToDetect is how long the subject took to report, measured from the
// moment the fault was observably live.
//
// The clock starts when Simian's own efficacy gate passed, not when Apply was
// called and not at anything inferred from the subject's output. That
// distinction is the whole reason this measure belongs to the adversary: only
// Simian knows when the cluster actually became broken, so only Simian can
// say whether a subject was fast or merely lucky about when it looked.
//
// Reported in seconds and never averaged into a grade. There is no ceiling on
// it and no defensible target, so folding it into a 0..1 score would be
// inventing a threshold.
type TimeToDetect struct{}

// Name implements Measure.
func (TimeToDetect) Name() string { return MeasureTimeToDetect }

// Score implements Measure.
func (TimeToDetect) Score(_ scenario.Scenario, run Run) Score {
	if run.InjectedAt.IsZero() || run.DetectedAt.IsZero() {
		return Score{Name: MeasureTimeToDetect, Skipped: true, Unit: UnitSeconds, Comment: "no injection or detection timestamp"}
	}
	d := run.DetectedAt.Sub(run.InjectedAt)
	if d < 0 {
		// A report that predates the fault landing is not a fast subject, it
		// is a clock or a harness problem, and averaging it in would make a
		// broken run look like the best one in the suite.
		return Score{
			Name:    MeasureTimeToDetect,
			Skipped: true,
			Unit:    UnitSeconds,
			Comment: fmt.Sprintf("detected %s before injection; timestamps are inconsistent", -d),
		}
	}
	return Score{
		Name:    MeasureTimeToDetect,
		Value:   d.Seconds(),
		Unit:    UnitSeconds,
		Comment: fmt.Sprintf("detected after %s", d),
	}
}

// TimeToRemediate is how long the subject took to actually fix the fault,
// measured from the same moment.
//
// This one falls out for free, and it is worth stating plainly: the lease
// reaper exists to stop Simian leaking faults into a cluster it does not own.
// The moment the subject is allowed to write, a reaper that arrives and finds
// the fault already gone is not reporting an error — it is reporting that the
// subject fixed it, with a timestamp. A safety mechanism becomes a measuring
// instrument without changing a line of it.
//
// Skipped, never zero, when the fault was still there at the end. A subject
// that did not fix anything has no remediation time, and scoring that as
// instant would invert the measure.
type TimeToRemediate struct{}

// Name implements Measure.
func (TimeToRemediate) Name() string { return MeasureTimeToRemediate }

// Score implements Measure.
func (TimeToRemediate) Score(_ scenario.Scenario, run Run) Score {
	if run.ClearedAt.IsZero() {
		return Score{Name: MeasureTimeToRemediate, Skipped: true, Unit: UnitSeconds, Comment: "fault was not remediated by the subject"}
	}
	if run.InjectedAt.IsZero() {
		return Score{Name: MeasureTimeToRemediate, Skipped: true, Unit: UnitSeconds, Comment: "no injection timestamp"}
	}
	d := run.ClearedAt.Sub(run.InjectedAt)
	if d < 0 {
		return Score{
			Name:    MeasureTimeToRemediate,
			Skipped: true,
			Unit:    UnitSeconds,
			Comment: fmt.Sprintf("cleared %s before injection; timestamps are inconsistent", -d),
		}
	}
	return Score{
		Name:    MeasureTimeToRemediate,
		Value:   d.Seconds(),
		Unit:    UnitSeconds,
		Comment: fmt.Sprintf("remediated after %s", d),
	}
}
