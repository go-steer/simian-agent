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

package subject

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/go-steer/simian-agent/pkg/eval"
	"github.com/go-steer/simian-agent/pkg/scenario"
)

// lookoutMargin is taken off the harness's timeout when telling the detector
// how long it has. A detector that gives up on its own says why, in one line
// on stderr; one killed by the harness leaves a subject error that reads like
// a harness fault.
const lookoutMargin = 5 * time.Second

// lookoutFallbackTimeout is what the detector is given when the harness is
// running it unbounded. Its own default is ten seconds, which is short for a
// namespace that is deliberately full of broken pods.
const lookoutFallbackTimeout = time.Minute

// lookoutSummaryKind is the per-category scorecard line, which is a verdict
// about a category rather than a finding about an object.
const lookoutSummaryKind = "health.category"

// Lookout runs k8s-lookout's health scan as a subject.
//
// # Why this is not an exec: subject
//
// The detector already emits the finding stream Simian grades — object kind,
// name, canonical reason, severity — one JSON record per line. What it does
// not emit is a report envelope with a "findings" key, and it should not: a
// detector growing a Simian-shaped output mode is the coupling this package's
// doc comment refuses. So the shape translation lives here, on our side of the
// process boundary, where it can be read next to the measures that consume it.
//
// # Why a deterministic subject comes first
//
// An agent's score moves for three reasons: the fault, the agent, and sampling
// noise. Here there is no third term. Two runs of the same scenario that score
// differently are a bug in the harness — a fault that landed late, an efficacy
// gate that passed early, a namespace that leaked between scenarios — and that
// is a test which cannot be written with any agent subject.
type Lookout struct {
	// Argv is the detector's command line: the binary, plus any flags to pass
	// through to `health`. Empty argv is an error rather than a default,
	// because guessing at a binary on PATH produces a scorecard attributed to
	// a version nobody chose.
	Argv []string

	// Timeout bounds one scan. Zero means unbounded.
	Timeout time.Duration

	// Dir is the working directory. Empty inherits the harness's.
	Dir string

	// Env is appended to the parent environment. KUBECONFIG belongs here.
	Env []string
}

// Name implements eval.Subject.
func (l *Lookout) Name() string {
	if len(l.Argv) == 0 {
		return "lookout"
	}
	return filepath.Base(l.Argv[0])
}

// Investigate runs one namespace-scoped health scan and translates it.
//
// The namespace comes out of the prompt, the same place an agent reads it
// from. That is deliberate: handing the detector a namespace through a side
// channel would give it something no agent subject gets, and the comparison
// between the two rows of the scorecard is the entire point of running it.
func (l *Lookout) Investigate(ctx context.Context, prompt string) (eval.Report, error) {
	if len(l.Argv) == 0 {
		return eval.Report{}, errors.New("subject: lookout adapter has no command")
	}
	ns, err := namespaceFromPrompt(prompt)
	if err != nil {
		return eval.Report{}, fmt.Errorf("subject %s: %w", l.Name(), err)
	}

	argv := append([]string{l.Argv[0], "health",
		"--namespace", ns,
		"--format", "json",
		"--timeout", l.scanTimeout().String(),
	}, l.Argv[1:]...)

	stdout, err := child{
		name:    l.Name(),
		argv:    argv,
		dir:     l.Dir,
		env:     append(slices.Clone(l.Env), PromptEnv+"="+prompt),
		timeout: l.Timeout,
	}.run(ctx)
	if err != nil {
		return eval.Report{}, err
	}

	report, err := parseLookoutStream(stdout)
	if err != nil {
		return eval.Report{}, fmt.Errorf("subject %s: %w", l.Name(), err)
	}
	return report, nil
}

// scanTimeout is what the detector is told it has, which is a little less than
// what it actually has.
func (l *Lookout) scanTimeout() time.Duration {
	switch {
	case l.Timeout <= 0:
		return lookoutFallbackTimeout
	case l.Timeout > 2*lookoutMargin:
		return l.Timeout - lookoutMargin
	default:
		return l.Timeout
	}
}

// lookoutRecord is the subset of the detector's record that Simian grades.
// Everything else it emits — the fingerprint, the check kind, the container,
// the restart count — is richer than the graded triple and deliberately
// unread here.
type lookoutRecord struct {
	Kind         string `json:"kind"`
	Severity     string `json:"severity"`
	Namespace    string `json:"namespace"`
	KindOfObject string `json:"kind_of_object"`
	Name         string `json:"name"`
	Reason       string `json:"reason"`
}

// parseLookoutStream turns the detector's JSON-per-line output into a report.
//
// Three kinds of line are skipped: the per-category scorecard verdicts, the
// trailing summary object, and anything with no object attached. None of them
// name a resource, and a finding with no name matches no expectation and can
// only distort the hallucination count.
func parseLookoutStream(out []byte) (eval.Report, error) {
	report := eval.Report{Findings: []scenario.Finding{}}

	var (
		lines = strings.Split(string(out), "\n")
		saw   bool // at least one well-formed record, of any kind
	)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec lookoutRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			// Not fatal on its own: the detector writes its own diagnostics
			// to stderr, but a wrapper script might echo something.
			continue
		}
		saw = true
		if rec.Kind == "" || rec.Kind == lookoutSummaryKind || rec.Name == "" {
			continue
		}
		sev, ok := scenario.ParseSeverity(rec.Severity)
		if !ok {
			return eval.Report{}, fmt.Errorf("finding %s/%s has severity %q, which is not one of ok, info, warning, critical",
				rec.KindOfObject, rec.Name, rec.Severity)
		}
		report.Findings = append(report.Findings, scenario.Finding{
			Kind:         rec.KindOfObject,
			ResourceName: rec.Name,
			Reason:       rec.Reason,
			Severity:     sev,
			Namespace:    rec.Namespace,
		})
	}
	if !saw {
		// A scan that printed nothing parseable did not answer. Scored as an
		// empty report it would read as "the namespace is fine", which is a
		// wrong answer indistinguishable from a right one on a control.
		return eval.Report{}, fmt.Errorf("no JSON records on stdout; got %d byte(s) starting %q", len(out), head(out, 120))
	}

	report.OverallSeverity = overallSeverity(report.Findings)
	return report, nil
}

// overallSeverity is the worst finding, or ok when there are none.
//
// Derived rather than read, because a health scan answers per category and
// never states one verdict for the namespace. Taking the maximum is the
// reading that makes a control scenario cost something: a namespace the
// detector found nothing wrong with is the detector saying it is healthy.
func overallSeverity(findings []scenario.Finding) scenario.Severity {
	worst := scenario.SeverityOK
	for _, f := range findings {
		if f.Severity.AtLeast(worst) {
			worst = f.Severity
		}
	}
	return worst
}

// quotedToken matches the namespace as every shipped prompt spells it: in
// double quotes, ahead of the word "namespace".
var quotedToken = regexp.MustCompile(`"([a-z0-9]([-a-z0-9]*[a-z0-9])?)"`)

// namespaceFromPrompt reads the namespace the subject is being asked about.
//
// A tool subject has to be told where to look, and the prompt is the only
// channel it shares with an agent subject. Every shipped scenario quotes its
// namespace, and pkg/scenario tests that the prompt names it — so a prompt
// this cannot read is a scenario-authoring bug, and it is reported as one
// rather than quietly widened into a whole-cluster scan.
func namespaceFromPrompt(prompt string) (string, error) {
	m := quotedToken.FindStringSubmatch(prompt)
	if m == nil {
		return "", fmt.Errorf("no quoted namespace in the prompt, so there is nowhere to scan: %q", head([]byte(prompt), 200))
	}
	return m[1], nil
}
