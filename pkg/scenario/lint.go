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

package scenario

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/go-steer/simian-agent/pkg/catalog"
)

// failureModePhrases name a failure mode distinctively enough to be searched
// for anywhere in the prompt, including across word boundaries. All spelling
// is normalized away by normalizeReason, so "crash loop back off",
// "crash-looping" and "CrashLoopBackOff" all reduce onto the "crashloop"
// entry.
//
// Entries here must be long and specific. A short entry matched as a
// substring is a false-positive machine: "oom" appears in "room", which would
// reject a perfectly good prompt about a room. Short and common words belong
// in failureModeWords instead.
var failureModePhrases = []string{
	"crashloop",
	"imagepull",
	"errimage",
	"oomkill",
	"outofmemory",
	"outofcpu",
	"unschedulable",
	"failedscheduling",
	"nodenotready",
	"notready",
	"starterror",
	"createcontainer",
	"invalidimage",
	"networkpolicy",
	"backoff",
	"misconfigur",
	"insufficientmemory",
	"insufficientcpu",
}

// failureModeWords name a failure mode but are ordinary enough English that
// they must match a whole token to count. "pending" is a diagnosis leak;
// "depending" is not.
var failureModeWords = []string{
	"pending",
	"evicted",
	"terminating",
	"unavailable",
	"unreachable",
	"partitioned",
	"partition",
	"throttled",
	"throttling",
	"latency",
	"timeout",
	"timeouts",
	"restarting",
	"restarts",
	"crashing",
	"crashed",
	"oom",
	"unhealthy",
	"degraded",
}

// wordish splits a prompt into candidate identifier tokens. Hyphenated and
// dotted names survive as one token, which is what lets a prompt naming
// "checkout-api" or "redis.cache" be caught.
//
// Trailing punctuation is trimmed separately rather than excluded here: a
// name at the end of a sentence ("...wrong with checkout-api.") would
// otherwise carry the full stop into the token and match nothing.
var wordish = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9._-]*`)

func tokenize(prompt string) map[string]bool {
	tokens := map[string]bool{}
	for _, t := range wordish.FindAllString(prompt, -1) {
		tokens[normalizeReason(strings.Trim(t, "._-"))] = true
	}
	delete(tokens, "")
	return tokens
}

// LintPrompt reports whether a scenario's prompt leaks its own answer.
//
// Two things disqualify a prompt: naming a failure mode, and naming a
// workload the scenario targets or expects a finding about. Both turn a
// diagnosis task into a paraphrase task, and a subject that paraphrases well
// is indistinguishable from one that diagnoses well once the answer is in the
// question.
//
// The workload half derives its vocabulary from the scenario rather than from
// a list kept somewhere else, so a pack that grows a new fixture cannot forget
// to register its names. Namespaces are exempt on purpose: naming the
// namespace to look at is the task.
func LintPrompt(s Scenario) error {
	prompt := strings.TrimSpace(s.Prompt)
	if prompt == "" {
		return errors.New("prompt is empty: a scenario must state a task")
	}

	tokens := tokenize(prompt)

	var leaks []string

	// Phrases match anywhere in the normalized prompt, so "the pod is
	// crash-looping" is caught even though no single token equals
	// "crashloop".
	flat := normalizeReason(prompt)
	for _, mode := range failureModePhrases {
		if strings.Contains(flat, mode) {
			leaks = append(leaks, fmt.Sprintf("names the failure mode %q", mode))
		}
	}

	// Common words must match a whole token, or every prompt containing
	// "depending" would be rejected for saying "pending".
	for _, mode := range failureModeWords {
		if tokens[mode] {
			leaks = append(leaks, fmt.Sprintf("names the failure mode %q", mode))
		}
	}

	for name := range s.workloadNames() {
		if tokens[normalizeReason(name)] {
			leaks = append(leaks, fmt.Sprintf("names the target workload %q", name))
		}
	}

	if len(leaks) > 0 {
		return fmt.Errorf("scenario %q: prompt leaks the diagnosis: %s", s.Name, strings.Join(leaks, "; "))
	}
	return nil
}

// workloadNames is every object name the scenario is about: the fault
// targets, and the objects the expectations name. Namespaces are excluded —
// telling the subject where to look is the task, not the answer.
func (s Scenario) workloadNames() map[string]bool {
	names := map[string]bool{}
	for _, f := range s.Faults {
		for _, t := range f.Targets {
			if t.Name != "" {
				names[t.Name] = true
			}
			for _, v := range t.Labels {
				if v != "" {
					names[v] = true
				}
			}
		}
	}
	for _, e := range s.Expect {
		if e.Name != "" {
			names[e.Name] = true
		}
	}
	return names
}

// Validate checks a scenario is well-formed enough to run and to grade.
func (s Scenario) Validate() error {
	var errs []error

	if strings.TrimSpace(s.ID) == "" {
		return errors.New("scenario: ID is required; it is the audit join key")
	}
	if strings.TrimSpace(s.Name) == "" {
		errs = append(errs, fmt.Errorf("scenario %q: Name is required", s.ID))
	}
	if !s.Source.Valid() {
		errs = append(errs, fmt.Errorf("scenario %q: Source %q is not a known source", s.ID, s.Source))
	}
	if len(s.Faults) == 0 && !s.IsControl() {
		errs = append(errs, fmt.Errorf("scenario %q: has expectations but no faults; nothing would break", s.ID))
	}
	if s.Severity != "" && !s.Severity.Valid() {
		errs = append(errs, fmt.Errorf("scenario %q: Severity %q is not a known severity", s.ID, s.Severity))
	}
	if err := LintPrompt(s); err != nil {
		errs = append(errs, err)
	}

	for i, f := range s.Faults {
		if len(f.Targets) == 0 {
			errs = append(errs, fmt.Errorf("scenario %q: fault %d has no targets", s.ID, i))
		}
		// A fault with no Settle probe cannot be shown to have landed, and a
		// fault that did not land grades the subject on a cluster that was
		// never broken. Efficacy gating is what makes a zero score mean
		// "missed it" rather than "there was nothing to miss".
		if len(catalog.DefaultProbes(f)) == 0 && len(f.Probes) == 0 {
			errs = append(errs, fmt.Errorf(
				"scenario %q: fault %d (%s/%s) has no Settle probe and no default gate; it could be graded without ever landing",
				s.ID, i, f.Engine, f.ResourceKind))
		}
	}

	// A control's whole job is to be scored on precision, so an exemption on
	// one is the single worst place to put it: it disarms the measure on the
	// scenario that exists to take it.
	if s.IsControl() && len(s.AlsoTrue) > 0 {
		errs = append(errs, fmt.Errorf(
			"scenario %q: a control has AlsoTrue entries; a scenario that injects nothing has no true consequences to exempt", s.ID))
	}
	for i, r := range s.AlsoTrue {
		if strings.TrimSpace(r) == "" {
			errs = append(errs, fmt.Errorf("scenario %q: AlsoTrue %d is empty", s.ID, i))
		}
	}

	for i, e := range s.Expect {
		if strings.TrimSpace(e.Kind) == "" {
			errs = append(errs, fmt.Errorf("scenario %q: expectation %d has no Kind", s.ID, i))
		}
		if strings.TrimSpace(e.Name) == "" {
			errs = append(errs, fmt.Errorf("scenario %q: expectation %d has no Name", s.ID, i))
		}
		if e.MinSeverity != "" && !e.MinSeverity.Valid() {
			errs = append(errs, fmt.Errorf("scenario %q: expectation %d: MinSeverity %q is not a known severity", s.ID, i, e.MinSeverity))
		}
	}

	return errors.Join(errs...)
}
