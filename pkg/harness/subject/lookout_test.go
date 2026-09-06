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

// A real scan of a namespace with one crash-looping Deployment: the category
// scorecard lines, the findings, and the summary object the detector prints
// last. Copied from `lookout health --namespace … --format=json` rather than
// hand-written, because the point of this test is the shape of somebody
// else's output.
const lookoutScan = `{"kind":"health.category","severity":"info","category":"nodes","status":"healthy"}
{"kind":"health.category","severity":"critical","category":"crashloops","status":"degraded","total":"1","top":"pod.crashloop lookout-batch/worker-7c9"}
{"kind":"pod.crashloop","severity":"critical","namespace":"lookout-batch","kind_of_object":"Pod","name":"worker-7c9d4-x2k","reason":"CrashLoopBackOff","fingerprint":"sha256:abc","category":"crashloops","container":"worker","restarts":"5"}
{"kind":"workload.unavailable","severity":"warning","namespace":"lookout-batch","kind_of_object":"Deployment","name":"worker","reason":"MinimumReplicasUnavailable","fingerprint":"sha256:def","category":"rollouts"}
{"scanned":4,"findings":2,"elapsed":"0.31s"}`

// fakeLookout is a stand-in detector on disk: it writes the canned scan to
// stdout and records the argv it was called with, so a test can assert on both
// what came back and how it was invoked. A script rather than `sh -c` because
// the argv order is part of what is being tested.
func fakeLookout(t *testing.T, stdout string, timeout time.Duration) (*Lookout, func() string) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on PATH")
	}
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	script := filepath.Join(dir, "lookout")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" > " + argvFile + "\ncat <<'SCAN'\n" + stdout + "\nSCAN\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write fake detector: %v", err)
	}
	calledWith := func() string {
		b, err := os.ReadFile(argvFile)
		if err != nil {
			t.Fatalf("the fake detector was never called: %v", err)
		}
		return strings.TrimSpace(string(b))
	}
	return &Lookout{Argv: []string{script}, Timeout: timeout}, calledWith
}

func TestLookoutTranslatesAScanIntoAReport(t *testing.T) {
	s, _ := fakeLookout(t, lookoutScan, time.Minute)
	r, err := s.Investigate(context.Background(), `Assess the health of the "lookout-batch" namespace and report what you find.`)
	if err != nil {
		t.Fatalf("Investigate: %v", err)
	}

	if len(r.Findings) != 2 {
		t.Fatalf("Findings = %+v, want the two object findings and neither the category lines nor the summary", r.Findings)
	}
	want := scenario.Finding{
		Kind:         "Pod",
		ResourceName: "worker-7c9d4-x2k",
		Reason:       "CrashLoopBackOff",
		Severity:     scenario.SeverityCritical,
		Namespace:    "lookout-batch",
	}
	if r.Findings[0] != want {
		t.Errorf("first finding = %+v, want %+v", r.Findings[0], want)
	}
	// The scenario's expected severity is graded against this, and a detector
	// that answers per category never states one.
	if r.OverallSeverity != scenario.SeverityCritical {
		t.Errorf("OverallSeverity = %q, want critical — the worst of the findings", r.OverallSeverity)
	}
}

// The namespace is read out of the prompt, which is the only channel a tool
// subject shares with an agent subject.
func TestLookoutScansTheNamespaceThePromptNames(t *testing.T) {
	s, calledWith := fakeLookout(t, lookoutScan, time.Minute)
	s.Argv = append(s.Argv, "--context", "kind-simian")
	if _, err := s.Investigate(context.Background(), `Assess the health of the "lookout-batch" namespace in this Kubernetes cluster and report what you find.`); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	want := "health --namespace lookout-batch --format json --timeout 55s --context kind-simian"
	if got := calledWith(); got != want {
		t.Errorf("detector called with %q, want %q", got, want)
	}
}

// The detector is told it has slightly less time than the harness will give
// it, so that a scan which runs long gives up on its own terms. Killed by the
// harness instead, it leaves an error that reads like a harness fault.
func TestLookoutGivesTheDetectorItsOwnDeadline(t *testing.T) {
	cases := []struct {
		harness time.Duration
		want    time.Duration
	}{
		{0, lookoutFallbackTimeout}, // unbounded: its own 10s default is short for a broken namespace
		{10 * time.Minute, 9*time.Minute + 55*time.Second},
		{3 * time.Second, 3 * time.Second}, // too short to take a margin off
	}
	for _, tc := range cases {
		s := &Lookout{Argv: []string{"lookout"}, Timeout: tc.harness}
		if got := s.scanTimeout(); got != tc.want {
			t.Errorf("Timeout %s: scanTimeout() = %s, want %s", tc.harness, got, tc.want)
		}
	}
}

// A prompt with no namespace in it is a scenario-authoring bug. Widening the
// scan to the whole cluster instead would score the subject on faults it was
// never asked about, and on a control it would import somebody else's broken
// namespace as a hallucination.
func TestLookoutRefusesAPromptWithNoNamespace(t *testing.T) {
	s, _ := fakeLookout(t, lookoutScan, time.Minute)
	_, err := s.Investigate(context.Background(), "Have a look around and tell me what you think.")
	if err == nil || !strings.Contains(err.Error(), "no quoted namespace") {
		t.Errorf("Investigate = %v, want a refusal naming the missing namespace", err)
	}
}

// A scan that printed nothing did not answer. Scored as an empty report it
// would read as "the namespace is fine" — the right answer on a control, for
// entirely the wrong reason.
func TestLookoutRefusesASilentScan(t *testing.T) {
	s, _ := fakeLookout(t, "", time.Minute)
	_, err := s.Investigate(context.Background(), `the "lookout-batch" namespace`)
	if err == nil || !strings.Contains(err.Error(), "no JSON records") {
		t.Errorf("Investigate = %v, want a refusal about the empty stream", err)
	}
}

// A healthy namespace is an answer, not an absence: an empty finding list with
// an explicit ok verdict, which is what a control scenario grades.
func TestLookoutReportsAHealthyNamespaceAsOK(t *testing.T) {
	healthy := `{"kind":"health.category","severity":"info","category":"crashloops","status":"healthy"}
{"scanned":3,"findings":0,"elapsed":"0.10s"}`
	s, _ := fakeLookout(t, healthy, time.Minute)
	r, err := s.Investigate(context.Background(), `the "lookout-status" namespace`)
	if err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if r.Findings == nil {
		t.Fatal("Findings is nil, want an empty non-nil list — the detector answered")
	}
	if len(r.Findings) != 0 {
		t.Errorf("Findings = %+v, want none", r.Findings)
	}
	if r.OverallSeverity != scenario.SeverityOK {
		t.Errorf("OverallSeverity = %q, want ok", r.OverallSeverity)
	}
}

// A severity nobody in this vocabulary can read is refused rather than
// defaulted. Defaulting it to info would silently drop the finding out of
// every measure, and the subject would score a miss it did not earn.
func TestLookoutRefusesASeverityItCannotRead(t *testing.T) {
	odd := `{"kind":"pod.crashloop","severity":"fatal","namespace":"lookout-batch","kind_of_object":"Pod","name":"worker-1","reason":"CrashLoopBackOff"}`
	s, _ := fakeLookout(t, odd, time.Minute)
	_, err := s.Investigate(context.Background(), `the "lookout-batch" namespace`)
	if err == nil || !strings.Contains(err.Error(), `severity "fatal"`) {
		t.Errorf("Investigate = %v, want a refusal naming the severity", err)
	}
}

func TestParseSubjectSpecKnowsLookout(t *testing.T) {
	got, err := Parse("lookout:./bin/lookout --context kind-simian", Options{Timeout: time.Minute})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	l, ok := got.(*Lookout)
	if !ok {
		t.Fatalf("Parse returned %T, want *Lookout", got)
	}
	if want := []string{"./bin/lookout", "--context", "kind-simian"}; strings.Join(l.Argv, " ") != strings.Join(want, " ") {
		t.Errorf("Argv = %v, want %v", l.Argv, want)
	}
	if l.Name() != "lookout" {
		t.Errorf("Name() = %q, want lookout", l.Name())
	}
	if _, err := Parse("lookout:", Options{}); err == nil {
		t.Error("Parse accepted a lookout subject with no binary; the version scored has to be one somebody chose")
	}
}
