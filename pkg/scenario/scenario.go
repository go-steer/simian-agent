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

// Package scenario defines the unit ground truth attaches to.
//
// # Why a scenario and not a manifest
//
// Simian's execution unit is the fault manifest: one perturbation, applied,
// gated, leased, reaped. That is the wrong granularity to grade against. A
// scenario about a cascading failure is one fault with two expected findings —
// a root cause and its downstream symptom — and a scenario about a noisy
// incident is three faults that must all be reported to score full recall.
// Neither collapses to a manifest, so expectations hang here instead.
//
// # The join key
//
// Scenario.ID is stamped into every audit event Simian emits while the
// scenario runs. That single field is what lets three independently produced
// records — Simian's audit log, the observer's findings, and the agent's
// transcript — be correlated after the fact into one answer to "what did we
// break, what did it detect, what did it fix". Without it the three are three
// piles of timestamps.
//
// # Expectations are deliberately borrowed, not invented
//
// ExpectedFinding is field-for-field the `Want` type from the agent-side live
// eval tier, with the same matching tolerances. The point is that a number
// produced here and a number produced there are comparable without anyone
// having to argue about whether the two rigs graded the same way. Divergence
// in the matcher is a divergence in the measurement, so the semantics are
// pinned by test.
package scenario

import (
	"slices"
	"sort"
	"strings"

	"github.com/go-steer/simian-agent/pkg/simian"
)

// Severity is the level a single finding can carry.
type Severity string

// Severity values, ordered by severityRank below.
const (
	SeverityOK       Severity = "ok"
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// severityRank orders severities for "at least this bad" comparisons. Higher
// is worse. Unknown severities rank 0, which makes an unparseable severity
// exactly as good as "ok" — that is, never sufficient for a MinSeverity gate.
var severityRank = map[Severity]int{
	SeverityOK:       0,
	SeverityInfo:     1,
	SeverityWarning:  2,
	SeverityCritical: 3,
}

// Valid reports whether s is a severity this package understands.
func (s Severity) Valid() bool {
	_, ok := severityRank[s]
	return ok
}

// AtLeast reports whether s is as severe as other, or worse.
func (s Severity) AtLeast(other Severity) bool {
	return severityRank[s] >= severityRank[other]
}

// ParseSeverity accepts a severity in any case, with or without the bracketed
// or trailing-colon forms a free-text report uses ("[CRITICAL]", "critical",
// "CRITICAL:"). Reports reach us from more than one producer and they do not
// agree on spelling.
func ParseSeverity(s string) (Severity, bool) {
	t := Severity(strings.ToLower(strings.Trim(strings.TrimSpace(s), "[]:")))
	if !t.Valid() {
		return "", false
	}
	return t, true
}

// Finding is one issue a subject reported, reduced to the machine-stable
// triple. Title and detail are deliberately absent: they are prose, and
// grading prose needs a judge, which is a different tier of evaluation.
type Finding struct {
	Kind         string   `json:"kind"`
	ResourceName string   `json:"resource_name"`
	Reason       string   `json:"reason"`
	Severity     Severity `json:"severity"`

	// Namespace scopes the finding. It is not part of the graded triple and
	// expectations do not carry one — an expectation describes an object
	// inside its own scenario and would otherwise repeat the namespace on
	// every entry. Scoring uses it only to discard findings about somewhere
	// else. Empty is accepted rather than rejected: the subject was asked
	// about one namespace, so omitting it is terse, not wrong.
	Namespace string `json:"namespace,omitempty"`
}

// ExpectedFinding is one finding a correct report must contain.
//
// Every field here mirrors the agent-side `faults.Want`, including the
// tolerances. See the package comment for why that matching is copied rather
// than reinterpreted.
type ExpectedFinding struct {
	// Kind is the Kubernetes kind, as the report spells it.
	Kind string `json:"kind"`

	// Name is the affected object. Deployment-owned pods carry a generated
	// suffix, so this is matched as a prefix — see MatchesName.
	Name string `json:"name"`

	// Reasons are the acceptable reason tokens; any one of them counts. Empty
	// means the reason is not graded and Kind+Name alone must match.
	//
	// A set rather than a string because "ImagePullBackOff" and "ErrImagePull"
	// are the same fault observed a few seconds apart, and a report is not
	// wrong for naming the one the API server showed it.
	Reasons []string `json:"reasons,omitempty"`

	// AlsoAcceptKinds lets a finding about the controller stand in for one
	// about the pod, or vice versa. "Deployment web is unavailable" and "Pod
	// web-abc123 is Pending" are the same discovery, and the former names the
	// object an operator would act on.
	AlsoAcceptKinds []string `json:"also_accept_kinds,omitempty"`

	// MinSeverity is the least severe this finding may be graded correct at.
	// Empty means severity is not graded for this finding.
	MinSeverity Severity `json:"min_severity,omitempty"`

	// Root marks this as the scenario's root cause, where some other expected
	// finding is its downstream symptom.
	//
	// Recall alone cannot express the difference. A scenario with a cause and
	// a symptom has two expectations, and a subject that reports only the
	// symptom scores the same 0.5 as one that reports only the cause — which
	// is a far better outcome and should not grade identically.
	Root bool `json:"root,omitempty"`
}

// MatchesName reports whether a reported resource name is the expected one.
//
// Exact, or the expected name plus a generated suffix. The prefix must not
// run the other way: "checkout" is a shorter, different name, and crediting
// it would let a subject score by naming a namespace's common prefix instead
// of the workload that broke.
func (e ExpectedFinding) MatchesName(got string) bool {
	got = strings.TrimSpace(got)
	if got == "" {
		return false
	}
	if got == e.Name {
		return true
	}
	return strings.HasPrefix(got, e.Name+"-")
}

// MatchesKind reports whether a reported kind is one this expectation
// accepts. Compared case-insensitively: the schema asks for "Pod", and a
// model that writes "pod" has not made a diagnostic error.
func (e ExpectedFinding) MatchesKind(got string) bool {
	if strings.EqualFold(got, e.Kind) {
		return true
	}
	for _, k := range e.AlsoAcceptKinds {
		if strings.EqualFold(got, k) {
			return true
		}
	}
	return false
}

// MatchesReason reports whether a reported reason is acceptable.
//
// Substring, case-insensitively, in both directions, ignoring separators:
// subjects write "CrashLoopBackOff", "CrashLoop" and "crash_loop_backoff" for
// the same state. The reason is graded to check that the failure mode was
// identified, not that our spelling of it was matched.
func (e ExpectedFinding) MatchesReason(got string) bool {
	if len(e.Reasons) == 0 {
		return true
	}
	g := normalizeReason(got)
	if g == "" {
		return false
	}
	for _, r := range e.Reasons {
		if n := normalizeReason(r); strings.Contains(g, n) || strings.Contains(n, g) {
			return true
		}
	}
	return false
}

func normalizeReason(s string) string {
	return strings.ToLower(strings.NewReplacer("_", "", "-", "", " ", "").Replace(s))
}

// Matches reports whether a finding satisfies this expectation.
func (e ExpectedFinding) Matches(f Finding) bool {
	if !e.MatchesKind(f.Kind) || !e.MatchesName(f.ResourceName) {
		return false
	}
	if !e.MatchesReason(f.Reason) {
		return false
	}
	if e.MinSeverity != "" && !f.Severity.AtLeast(e.MinSeverity) {
		return false
	}
	return true
}

// Source records where a scenario came from. Scores are only comparable
// within a source: a hand-written pack is a fixed yardstick, while a
// topology-generated scenario differs per cluster.
type Source string

// Scenario sources.
const (
	// SourcePackParity is the hand-written pack that mirrors the agent-side
	// live eval fixtures one-for-one.
	SourcePackParity Source = "pack:parity"

	// SourcePackLookout is the hand-written pack aimed at the observer.
	SourcePackLookout Source = "pack:lookout"

	// SourceGeneratedTopology is synthesized from a discovered cluster
	// topology rather than written down.
	SourceGeneratedTopology Source = "generated:topology"
)

// Valid reports whether s is a source this package understands.
func (s Source) Valid() bool {
	switch s {
	case SourcePackParity, SourcePackLookout, SourceGeneratedTopology:
		return true
	}
	return false
}

// Scenario is one graded experiment: what to break, and what a correct report
// about it says.
type Scenario struct {
	// ID is stamped into every audit event emitted while this scenario runs.
	// It is the key that joins Simian's audit log to the subject's output.
	ID string `json:"id"`

	// Name is the human label, unique within a pack.
	Name string `json:"name"`

	// Prompt is what the subject is asked. It must name the *task*, never the
	// fault: "check namespace X" is the entire point. A prompt that leaks the
	// diagnosis measures paraphrasing instead of diagnosis. Enforced by
	// LintPrompt, not by convention.
	Prompt string `json:"prompt"`

	// Substrate names a registered SUT to stand up in the scenario's
	// namespace before any fault is applied, and take down after. Empty means
	// the faults synthesize their own subject matter, which is what every
	// pack did before this field existed.
	//
	// # Why a scenario ever needs one
	//
	// A kube-state fault brings its own workload: the fault *is* the
	// Deployment it creates. That works for everything whose symptom is
	// visible on a Kubernetes object — a pod that will not pull, a rollout
	// that will not finish. It stops working the moment the symptom is a
	// property of traffic. Nothing about "the p99 doubled" is stored in the
	// API server, and a fault cannot inject latency into a caller that does
	// not exist.
	//
	// The obvious workaround — one fault to synthesize a caller and a callee,
	// a second to degrade them — does not work, and the reason is worth
	// recording so nobody spends an afternoon rediscovering it: kube-state
	// appends a suffix derived from the fault UID to every workload it
	// creates, so the second fault cannot name, label-select or otherwise
	// predict what the first one made. Substrate is the deterministic-name
	// half that the faults then attack.
	Substrate string `json:"substrate,omitempty"`

	// Faults are the manifests to apply, each carrying its own Settle probes
	// so that a fault which did not land is never graded as though it had.
	Faults []simian.FaultManifest `json:"faults"`

	// Expect are the findings a correct report must contain. Empty is
	// meaningful: a scenario with no expectations is a healthy control, and a
	// suite without controls makes recall gameable by reporting every failure
	// mode everywhere.
	Expect []ExpectedFinding `json:"expect,omitempty"`

	// AlsoTrue names failure modes the fault genuinely produces that the
	// subject is *not* required to report.
	//
	// It exists because "correct" and "required" are different sets, and only
	// Expect was expressing both. A stuck rollout is diagnosed at Deployment
	// altitude — new_ready=0/N against old_ready=N/N — and that is the only
	// finding worth demanding. But the reason the new revision is not ready is
	// that its container exits non-zero, so there is also a pod in
	// CrashLoopBackOff, and a subject that mentions it has said something true
	// about the cluster.
	//
	// With only Expect to read from, the precision measure inferred the
	// injected failure modes from the required ones and charged that true
	// statement as an invention: bad-rollout scored hallucinated_fault 0.50
	// against a detector that was right twice. Promoting the pod to an
	// expectation would have fixed precision by breaking recall — the subject
	// that reports only the Deployment gives the better answer and would have
	// scored 0.50 for it.
	//
	// So these tokens suppress the hallucination charge and contribute nothing
	// to recall. They are reason tokens, not objects: the claim being licensed
	// is "this namespace contains a crash loop", and which pod is the
	// subject's business rather than ground truth's.
	//
	// Use it only for a consequence the fault *mechanically* produces, and
	// keep the list short. Every entry is a claim the subject can no longer be
	// charged for, so a generous one turns precision off. A control may not
	// have any, which is enforced: a scenario that injects nothing has no
	// consequences, and a control with an exemption measures nothing at all.
	AlsoTrue []string `json:"also_true,omitempty"`

	// Severity is the report-level severity a correct report carries.
	Severity Severity `json:"severity,omitempty"`

	// Source records where this scenario came from.
	Source Source `json:"source"`
}

// Namespaces returns the distinct namespaces this scenario's faults target,
// sorted.
//
// This is the same derivation the scorer uses to decide which findings are in
// scope, so the namespace the harness provisions is the namespace the ground
// truth is about — and, for a scenario with a Substrate, the namespace the
// substrate is deployed into.
func (s Scenario) Namespaces() []string {
	var out []string
	for _, f := range s.Faults {
		for _, t := range f.Targets {
			if t.Namespace != "" && !slices.Contains(out, t.Namespace) {
				out = append(out, t.Namespace)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Roots returns the expectations marked as root causes.
func (s Scenario) Roots() []ExpectedFinding {
	var roots []ExpectedFinding
	for _, e := range s.Expect {
		if e.Root {
			roots = append(roots, e)
		}
	}
	return roots
}

// IsControl reports whether this scenario expects no findings at all — a
// healthy cluster the subject should not invent problems about.
func (s Scenario) IsControl() bool { return len(s.Expect) == 0 }
