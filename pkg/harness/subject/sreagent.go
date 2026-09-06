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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"context"

	"github.com/go-steer/simian-agent/pkg/eval"
	"github.com/go-steer/simian-agent/pkg/scenario"
)

// sreAgentMargin is taken off the harness's timeout when telling the agent how
// long it has. An agent that gives up on its own still writes its transcript,
// and a partial answer with a recorded reason is worth more than a killed
// process and a subject error.
const sreAgentMargin = 30 * time.Second

// sreAgentFallbackTimeout is what the agent is given when the harness is
// running it unbounded. Its own default is eight minutes.
const sreAgentFallbackTimeout = 8 * time.Minute

// sreAgentOwnedFlags are the flags this adapter sets and an operator may not.
//
// Rejected rather than overridden, because each one silently breaks something
// different and none of them fails loudly. -out would send the transcript
// somewhere this adapter does not read, and the run would score as though the
// agent had answered nothing. -namespace would point the agent at a namespace
// the scenario did not break. -repeat would produce several assessments where
// the scorer grades one, and the one it grades would be arbitrary.
var sreAgentOwnedFlags = []string{"-namespace", "-out", "-repeat"}

// SREAgent runs core-sre-agent's one-shot assessment as a subject.
//
// # Why this is not an exec: subject
//
// It nearly is. The agent's schema.HealthReport is already the graded triple
// and then some — severity, kind, resource_name, reason, namespace, with the
// same severity vocabulary — so no field translation is needed at all, which
// is the strongest evidence yet that the triple is the right boundary.
//
// What does not line up is where the report comes out. `sre-agent` prints a
// human-readable assessment on stdout and writes its machine-readable
// transcript to a file named by -out, as an array of per-namespace assessments
// with the report nested inside each one. ParseReport looks for a JSON object
// with a "findings" key on stdout and would find prose. So this adapter names
// the file, runs the agent, and reads the report back out of it.
//
// That is a shape translation and nothing more. It lives here for the same
// reason Lookout's does: the alternative is an agent growing a Simian-shaped
// output mode, and a subject that had to be modified to be graded is a subject
// whose score is about the modification.
//
// # What it costs
//
// Unlike every other subject in this package, one Investigate spends real
// model tokens against a real cluster. The agent's own -max-cost and -max-turns
// are the bounds for that and are passed through untouched — this adapter does
// not set them, because a ceiling Simian chose would be a ceiling nobody
// reading the scorecard could account for.
type SREAgent struct {
	// Argv is the agent's command line: the binary, plus the flags it needs to
	// reach a cluster (-kubeconfig and -context, both of which it requires).
	// Empty argv is an error rather than a default, for the same reason as
	// Lookout: a scorecard attributed to whichever binary was on PATH is a
	// number nobody can reproduce.
	Argv []string

	// Timeout bounds one assessment. Zero means unbounded.
	Timeout time.Duration

	// Dir is the working directory. Empty inherits the harness's.
	Dir string

	// Env is appended to the parent environment. The provider credentials
	// belong here.
	Env []string

	// ArtifactDir is where the agent's transcript is kept. Empty discards it
	// once the report has been read out.
	//
	// Keeping it is the default in practice and should be: the transcript is
	// the only record of which tools the agent called, what each returned and
	// what it cost, and none of that is recoverable from the scorecard. A run
	// that scores 0.00 and cannot say whether the agent looked in the right
	// place has measured something without learning anything.
	ArtifactDir string
}

// Name implements eval.Subject.
func (a *SREAgent) Name() string {
	if len(a.Argv) == 0 {
		return "sre-agent"
	}
	return filepath.Base(a.Argv[0])
}

// Investigate runs one namespace-scoped assessment and reads its report.
//
// The namespace comes out of the prompt, the same place the agent's own
// operator-facing entry point puts it: `sre-agent` builds the identical
// sentence from -namespace. So Simian is not handing this subject anything a
// prompt-driven subject would not get, and the scorecard's rows stay
// comparable.
func (a *SREAgent) Investigate(ctx context.Context, prompt string) (eval.Report, error) {
	if len(a.Argv) == 0 {
		return eval.Report{}, errors.New("subject: sre-agent adapter has no command")
	}
	if err := checkSREAgentFlags(a.Argv[1:]); err != nil {
		return eval.Report{}, fmt.Errorf("subject %s: %w", a.Name(), err)
	}
	ns, err := namespaceFromPrompt(prompt)
	if err != nil {
		return eval.Report{}, fmt.Errorf("subject %s: %w", a.Name(), err)
	}

	out, cleanup, err := a.transcriptPath(ns)
	if err != nil {
		return eval.Report{}, fmt.Errorf("subject %s: %w", a.Name(), err)
	}
	defer cleanup()

	argv := append([]string{a.Argv[0],
		"-namespace", ns,
		"-out", out,
		"-timeout", a.agentTimeout().String(),
	}, a.Argv[1:]...)

	if _, err := (child{
		name:    a.Name(),
		argv:    argv,
		dir:     a.Dir,
		env:     append(slices.Clone(a.Env), PromptEnv+"="+prompt),
		timeout: a.Timeout,
	}).run(ctx); err != nil {
		return eval.Report{}, err
	}

	blob, err := os.ReadFile(out) //nolint:gosec // a path this adapter just made
	if err != nil {
		return eval.Report{}, fmt.Errorf("subject %s: the agent exited 0 but wrote no transcript to %s: %w", a.Name(), out, err)
	}
	report, err := parseSREAgentTranscript(blob, ns)
	if err != nil {
		return eval.Report{}, fmt.Errorf("subject %s: %w", a.Name(), err)
	}
	return report, nil
}

// transcriptPath decides where this assessment's transcript goes, and returns
// the cleanup that keeps or discards it.
//
// Named after the namespace rather than given a random name, because a
// transcript sitting next to the run file is only useful if a reader can tell
// which scenario it belongs to. The namespace is unique within a run — one
// arena per scenario — so it also cannot collide under --concurrency, which a
// fixed name would.
func (a *SREAgent) transcriptPath(ns string) (string, func(), error) {
	if a.ArtifactDir != "" {
		return filepath.Join(a.ArtifactDir, "transcript-"+ns+".json"), func() {}, nil
	}
	dir, err := os.MkdirTemp("", "simian-sre-agent-")
	if err != nil {
		return "", nil, fmt.Errorf("transcript directory: %w", err)
	}
	return filepath.Join(dir, "transcript.json"), func() { _ = os.RemoveAll(dir) }, nil
}

// agentTimeout is what the agent is told it has, which is a little less than
// what it actually has.
func (a *SREAgent) agentTimeout() time.Duration {
	switch {
	case a.Timeout <= 0:
		return sreAgentFallbackTimeout
	case a.Timeout > 2*sreAgentMargin:
		return a.Timeout - sreAgentMargin
	default:
		return a.Timeout
	}
}

// checkSREAgentFlags refuses a spec that sets a flag this adapter owns.
//
// Both spellings, because Go's flag package accepts either and an operator who
// wrote --namespace and got a silently ignored warning would be right to be
// annoyed.
func checkSREAgentFlags(args []string) error {
	for _, arg := range args {
		name, _, _ := strings.Cut(arg, "=")
		for _, owned := range sreAgentOwnedFlags {
			if name == owned || name == "-"+owned {
				return fmt.Errorf("the subject spec sets %s, which this adapter sets itself; drop it from the spec", owned)
			}
		}
	}
	return nil
}

// sreAgentTranscript is the subset of the agent's transcript Simian reads.
// Everything else in it — the tool trajectory, the delegations, the token
// usage, the elapsed time — is richer than the graded triple and deliberately
// unread here. It is not unimportant; it is the agent repo's own instrument.
type sreAgentTranscript []struct {
	Namespace string `json:"namespace"`
	Error     string `json:"error"`
	Run       struct {
		// No json tag on the agent's side, so the key is the field name.
		Health *sreAgentHealth `json:"Health"`
	} `json:"run"`
}

// sreAgentHealth is schema.HealthReport reduced to what is graded. The field
// tags match it exactly, so this is a narrowing rather than a translation.
type sreAgentHealth struct {
	OverallSeverity scenario.Severity  `json:"overall_severity"`
	Findings        []scenario.Finding `json:"findings"`
}

// parseSREAgentTranscript reads the one assessment out of the transcript.
//
// One, and it insists on it. The agent's -repeat runs a namespace several times
// into one file, and a scorer handed several answers would grade an arbitrary
// one; the flag is refused up front, and this is the check that the refusal
// worked.
func parseSREAgentTranscript(blob []byte, ns string) (eval.Report, error) {
	var t sreAgentTranscript
	if err := json.Unmarshal(blob, &t); err != nil {
		return eval.Report{}, fmt.Errorf("decode transcript: %w", err)
	}
	switch len(t) {
	case 1:
	case 0:
		return eval.Report{}, errors.New("transcript holds no assessments")
	default:
		return eval.Report{}, fmt.Errorf("transcript holds %d assessments, want exactly 1", len(t))
	}
	a := t[0]

	// A recorded error is a subject error, not an empty report. The agent
	// timing out or losing its provider is a run that did not happen, and
	// scoring it as "reported nothing" would put a hard zero on the board that
	// reads as a diagnosis the agent got wrong.
	if a.Error != "" {
		return eval.Report{}, fmt.Errorf("the agent recorded an error for %s: %s", a.Namespace, a.Error)
	}
	if a.Namespace != ns {
		return eval.Report{}, fmt.Errorf("transcript is about namespace %q, asked about %q", a.Namespace, ns)
	}
	// Likewise nil: the agent finished without producing a structured report,
	// which is a different thing from producing an empty one. Only the second
	// is the subject saying the namespace is fine, and only the second should
	// score full marks on a control.
	if a.Run.Health == nil {
		return eval.Report{}, fmt.Errorf("the agent finished without a structured report for %s", a.Namespace)
	}

	findings := a.Run.Health.Findings
	if findings == nil {
		findings = []scenario.Finding{}
	}
	return eval.Report{
		Findings:        findings,
		OverallSeverity: a.Run.Health.OverallSeverity,
	}, nil
}
