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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/simian-agent/pkg/scenario"
)

// A real transcript from `sre-agent -namespace … -out …`, trimmed to the keys
// this adapter reads and keeping the ones it does not so the narrowing is
// exercised. Note the capital "Health": the agent's field carries no json tag,
// so the key is the Go field name, and a lowercase guess would decode to nil
// and be reported as an agent that never answered.
const sreAgentAssessment = `[
  {
    "namespace": "lookout-batch",
    "attempt": 1,
    "prompt": "Assess the health of the \"lookout-batch\" namespace",
    "elapsed": "30.3s",
    "run": {
      "Health": {
        "overall_severity": "critical",
        "findings": [
          {
            "kind": "Pod",
            "resource_name": "worker-7c9d4-x2k",
            "reason": "CrashLoopBackOff",
            "severity": "critical",
            "namespace": "lookout-batch"
          }
        ]
      }
    }
  }
]`

// fakeSREAgent is a stand-in agent on disk. It records its argv, then writes
// the given transcript to whatever -out it was handed — which is the half of
// the contract that matters here, since the adapter names that file and reads
// the score back out of it.
func fakeSREAgent(t *testing.T, transcript string) (*SREAgent, func() string) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on PATH")
	}
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	script := filepath.Join(dir, "sre-agent")
	body := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" > " + argvFile + "\n" +
		"out=\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  case \"$1\" in -out) out=$2; shift ;; esac\n" +
		"  shift\n" +
		"done\n" +
		"echo 'the namespace is unwell'\n" +
		"cat > \"$out\" <<'TRANSCRIPT'\n" + transcript + "\nTRANSCRIPT\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write fake agent: %v", err)
	}
	calledWith := func() string {
		b, err := os.ReadFile(argvFile)
		if err != nil {
			t.Fatalf("the fake agent was never called: %v", err)
		}
		return strings.TrimSpace(string(b))
	}
	return &SREAgent{Argv: []string{script}, Timeout: time.Minute}, calledWith
}

func TestSREAgentReadsItsReportOutOfTheTranscript(t *testing.T) {
	s, _ := fakeSREAgent(t, sreAgentAssessment)
	r, err := s.Investigate(context.Background(), `Assess the health of the "lookout-batch" namespace and report what you find.`)
	if err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	want := scenario.Finding{
		Kind:         "Pod",
		ResourceName: "worker-7c9d4-x2k",
		Reason:       "CrashLoopBackOff",
		Severity:     scenario.SeverityCritical,
		Namespace:    "lookout-batch",
	}
	if len(r.Findings) != 1 || r.Findings[0] != want {
		t.Errorf("Findings = %+v, want exactly %+v", r.Findings, want)
	}
	// Unlike the detector, the agent states a report-level severity itself, and
	// the scenario's `severity:` is graded against that statement rather than
	// against the worst finding.
	if r.OverallSeverity != scenario.SeverityCritical {
		t.Errorf("OverallSeverity = %q, want the severity the agent stated", r.OverallSeverity)
	}
}

// The namespace comes from the prompt and the flags this adapter owns are
// appended before the operator's, so a spec cannot shadow them by position.
func TestSREAgentAsksAboutTheNamespaceThePromptNames(t *testing.T) {
	s, calledWith := fakeSREAgent(t, sreAgentAssessment)
	s.Argv = append(s.Argv, "-kubeconfig", "/tmp/kubeconfig")
	if _, err := s.Investigate(context.Background(), `Assess the health of the "lookout-batch" namespace in this Kubernetes cluster and report what you find.`); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	got := calledWith()
	if !strings.HasPrefix(got, "-namespace lookout-batch -out ") {
		t.Errorf("agent called with %q, want it pointed at lookout-batch", got)
	}
	// A minute is not long enough to take the margin off — see agentTimeout —
	// so the agent gets the whole of it, and then the spec's own flags.
	if !strings.HasSuffix(got, " -timeout 1m0s -kubeconfig /tmp/kubeconfig") {
		t.Errorf("agent called with %q, want its own deadline and then the spec's flags", got)
	}
}

// A transcript kept next to the run file is the only record of what the agent
// did: which tools it called, what each returned, what it cost. The scorecard
// has none of that, so a 0.00 with the transcript deleted is a number nobody
// can act on.
func TestSREAgentKeepsTheTranscriptWhenGivenSomewhereToPutIt(t *testing.T) {
	s, _ := fakeSREAgent(t, sreAgentAssessment)
	s.ArtifactDir = t.TempDir()
	if _, err := s.Investigate(context.Background(), `the "lookout-batch" namespace`); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	// Named for the namespace: one arena per scenario, so the name says which
	// scenario the evidence belongs to and two concurrent agents cannot collide.
	kept := filepath.Join(s.ArtifactDir, "transcript-lookout-batch.json")
	b, err := os.ReadFile(kept) //nolint:gosec // a path this test chose
	if err != nil {
		t.Fatalf("the transcript was not kept at %s: %v", kept, err)
	}
	if !strings.Contains(string(b), "CrashLoopBackOff") {
		t.Errorf("kept transcript = %q, want the agent's own output", b)
	}
}

func TestSREAgentDiscardsTheTranscriptWhenThereIsNowhereToKeepIt(t *testing.T) {
	s, calledWith := fakeSREAgent(t, sreAgentAssessment)
	if _, err := s.Investigate(context.Background(), `the "lookout-batch" namespace`); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	_, rest, _ := strings.Cut(calledWith(), "-out ")
	out, _, _ := strings.Cut(rest, " ")
	if _, err := os.Stat(filepath.Dir(out)); !os.IsNotExist(err) {
		t.Errorf("the temporary transcript directory %s survived the run: %v", filepath.Dir(out), err)
	}
}

// The agent is told it has slightly less time than the harness will give it,
// so that it gives up on its own terms and still writes a transcript saying
// why. Killed by the harness instead, all that survives is a subject error.
func TestSREAgentGivesTheAgentItsOwnDeadline(t *testing.T) {
	cases := []struct {
		harness time.Duration
		want    time.Duration
	}{
		{0, sreAgentFallbackTimeout},
		{12 * time.Minute, 11*time.Minute + 30*time.Second},
		{20 * time.Second, 20 * time.Second}, // too short to take a margin off
	}
	for _, tc := range cases {
		s := &SREAgent{Argv: []string{"sre-agent"}, Timeout: tc.harness}
		if got := s.agentTimeout(); got != tc.want {
			t.Errorf("Timeout %s: agentTimeout() = %s, want %s", tc.harness, got, tc.want)
		}
	}
}

// Each owned flag breaks the measurement in its own quiet way, so the spec is
// refused rather than the flag overridden.
func TestSREAgentRefusesASpecThatSetsAFlagItOwns(t *testing.T) {
	for _, arg := range []string{"-namespace", "--namespace", "-out=/tmp/x", "--out", "-repeat", "--repeat=3"} {
		s := &SREAgent{Argv: []string{"sre-agent", arg, "whatever"}}
		_, err := s.Investigate(context.Background(), `the "lookout-batch" namespace`)
		if err == nil || !strings.Contains(err.Error(), "which this adapter sets itself") {
			t.Errorf("Investigate with %s = %v, want a refusal", arg, err)
		}
	}
}

func TestSREAgentRefusesAPromptWithNoNamespace(t *testing.T) {
	s, _ := fakeSREAgent(t, sreAgentAssessment)
	_, err := s.Investigate(context.Background(), "Have a look around and tell me what you think.")
	if err == nil || !strings.Contains(err.Error(), "no quoted namespace") {
		t.Errorf("Investigate = %v, want a refusal naming the missing namespace", err)
	}
}

// Everything below is the difference between "the agent says the namespace is
// fine" and "the agent did not answer". Only the first is a report, and only
// the first should be able to score full marks on a control.
func TestSREAgentTellsAnEmptyReportFromNoReport(t *testing.T) {
	healthy := `[{"namespace":"lookout-batch","run":{"Health":{"overall_severity":"ok","findings":[]}}}]`
	s, _ := fakeSREAgent(t, healthy)
	r, err := s.Investigate(context.Background(), `the "lookout-batch" namespace`)
	if err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if len(r.Findings) != 0 || r.OverallSeverity != scenario.SeverityOK {
		t.Errorf("report = %+v, want an empty report at ok", r)
	}
}

func TestSREAgentTranscriptRefusals(t *testing.T) {
	cases := []struct {
		name       string
		transcript string
		want       string
	}{{
		name:       "an assessment the agent itself failed",
		transcript: `[{"namespace":"lookout-batch","error":"vertex: quota exhausted","run":{}}]`,
		want:       "recorded an error",
	}, {
		name:       "no assessment at all",
		transcript: `[]`,
		want:       "no assessments",
	}, {
		name:       "several assessments, none of them canonical",
		transcript: `[{"namespace":"lookout-batch","run":{"Health":{"overall_severity":"ok"}}},{"namespace":"lookout-batch","run":{"Health":{"overall_severity":"critical"}}}]`,
		want:       "holds 2 assessments",
	}, {
		name:       "an answer about somewhere else",
		transcript: `[{"namespace":"kube-system","run":{"Health":{"overall_severity":"ok"}}}]`,
		want:       `is about namespace "kube-system"`,
	}, {
		name:       "a run that produced no structured report",
		transcript: `[{"namespace":"lookout-batch","run":{}}]`,
		want:       "without a structured report",
	}, {
		name:       "something that is not a transcript",
		transcript: `the namespace looks unwell to me`,
		want:       "decode transcript",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := fakeSREAgent(t, tc.transcript)
			_, err := s.Investigate(context.Background(), `the "lookout-batch" namespace`)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Investigate = %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
}

// A subject error, not an empty report: the agent exiting 0 without writing
// anything is a broken invocation, and scoring it as "reported nothing" would
// put a zero on the board that reads as a wrong diagnosis.
func TestSREAgentRefusesASilentRun(t *testing.T) {
	if _, err := exec.LookPath("true"); err != nil {
		t.Skip("no true on PATH")
	}
	s := &SREAgent{Argv: []string{"true"}, Timeout: time.Minute}
	_, err := s.Investigate(context.Background(), `the "lookout-batch" namespace`)
	if err == nil || !strings.Contains(err.Error(), "wrote no transcript") {
		t.Errorf("Investigate = %v, want a refusal naming the missing transcript", err)
	}
}

func TestParseSubjectSpecKnowsTheSREAgent(t *testing.T) {
	got, err := Parse("sre-agent:./bin/sre-agent -context kind-simian", Options{Timeout: time.Minute, ArtifactDir: "runs/x"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	a, ok := got.(*SREAgent)
	if !ok {
		t.Fatalf("Parse returned %T, want *SREAgent", got)
	}
	if want := "./bin/sre-agent -context kind-simian"; strings.Join(a.Argv, " ") != want {
		t.Errorf("Argv = %v, want %q", a.Argv, want)
	}
	if a.ArtifactDir != "runs/x" {
		t.Errorf("ArtifactDir = %q, want the run's own directory", a.ArtifactDir)
	}
	if a.Name() != "sre-agent" {
		t.Errorf("Name() = %q, want sre-agent", a.Name())
	}
	if _, err := Parse("sre-agent:", Options{}); err == nil {
		t.Error("Parse accepted an sre-agent subject with no binary; the build scored has to be one somebody chose")
	}
}
