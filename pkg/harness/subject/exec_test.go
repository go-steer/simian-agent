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
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/simian-agent/pkg/scenario"
)

// sh runs the given script as an Exec subject. Every process test goes through
// /bin/sh, which is also the most likely real subject after a compiled agent.
func sh(t *testing.T, script string, timeout time.Duration) *Exec {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on PATH")
	}
	return &Exec{Argv: []string{"sh", "-c", script}, Timeout: timeout}
}

func TestExecReadsAReportOffStdout(t *testing.T) {
	s := sh(t, `echo '{"findings":[{"kind":"Pod","resource_name":"api","reason":"CrashLoopBackOff","severity":"critical"}]}'`, time.Minute)
	r, err := s.Investigate(context.Background(), "what is wrong?")
	if err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if len(r.Findings) != 1 {
		t.Fatalf("Findings = %v, want one", r.Findings)
	}
	got := r.Findings[0]
	if got.Kind != "Pod" || got.ResourceName != "api" || got.Severity != scenario.SeverityCritical {
		t.Errorf("finding = %+v, want the one the subject printed", got)
	}
}

// The prompt arrives exactly once, by exactly one route. A subject that reads
// both stdin and the placeholder must not be handed the task twice.
func TestThePromptGoesToStdinWhenArgvDoesNotAskForIt(t *testing.T) {
	s := sh(t, `printf '{"findings":[],"echo":"%s"}' "$(cat)"`, time.Minute)
	r, err := s.Investigate(context.Background(), "the prompt")
	if err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if r.Findings == nil {
		t.Fatal("Findings is nil, want the subject's empty answer")
	}
}

func TestThePromptGoesToArgvWhenThePlaceholderIsThere(t *testing.T) {
	s := &Exec{
		Argv:    []string{"sh", "-c", `printf '{"findings":[],"seen":"%s"}' "$1"`, "sh", PromptPlaceholder},
		Timeout: time.Minute,
	}
	r, err := s.Investigate(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if r.Findings == nil {
		t.Fatal("Findings is nil, want the subject's empty answer")
	}
}

// Stdin is closed when the placeholder took the prompt. A subject that reads
// stdin anyway must see EOF rather than block forever holding a fault in
// somebody's cluster.
func TestStdinIsEmptyWhenThePlaceholderTookThePrompt(t *testing.T) {
	s := &Exec{
		Argv:    []string{"sh", "-c", `test -z "$(cat)" && printf '{"findings":[]}' || printf '{"findings":[{"kind":"X"}]}'`, "sh", PromptPlaceholder},
		Timeout: 30 * time.Second,
	}
	r, err := s.Investigate(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if len(r.Findings) != 0 {
		t.Fatalf("the subject saw %v on stdin; it should have seen nothing", r.Findings)
	}
}

// Every subject gets the prompt in the environment too, because a shell
// script would rather read a variable than parse stdin.
func TestThePromptIsAlwaysInTheEnvironment(t *testing.T) {
	s := sh(t, `printf '{"findings":[{"kind":"Env","resource_name":"%s"}]}' "$SIMIAN_PROMPT"`, time.Minute)
	r, err := s.Investigate(context.Background(), "seen-in-env")
	if err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if len(r.Findings) != 1 || r.Findings[0].ResourceName != "seen-in-env" {
		t.Fatalf("findings = %+v, want the prompt read out of %s", r.Findings, PromptEnv)
	}
}

func TestAnExitCodeIsASubjectFailure(t *testing.T) {
	s := sh(t, `echo "it broke" >&2; exit 3`, time.Minute)
	_, err := s.Investigate(context.Background(), "p")
	if err == nil {
		t.Fatal("a subject that exited 3 was reported as having answered")
	}
	// The stderr tail is quoted back, because "exit status 3" on its own
	// sends whoever reads the scorecard to go and re-run the subject by hand.
	if !strings.Contains(err.Error(), "it broke") {
		t.Errorf("error = %v, want the subject's stderr quoted into it", err)
	}
}

// A killed process exits non-zero, and reporting that as "exit status 1"
// sends the reader looking for a bug in a subject that was simply too slow.
func TestATimeoutIsReportedAsATimeoutNotAnExitCode(t *testing.T) {
	t.Parallel()
	s := sh(t, `sleep 30`, 100*time.Millisecond)
	start := time.Now()
	_, err := s.Investigate(context.Background(), "p")
	if err == nil {
		t.Fatal("a subject that ran out of time was reported as having answered")
	}
	if !strings.Contains(err.Error(), "deadline exceeded") {
		t.Errorf("error = %v, want it to say the deadline was exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Errorf("Investigate took %s; the timeout did not kill the subject", elapsed)
	}
}

func TestACancelledRunStopsTheSubject(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	s := sh(t, `sleep 30`, time.Minute)
	if _, err := s.Investigate(ctx, "p"); err == nil {
		t.Fatal("a cancelled subject was reported as having answered")
	}
}

func TestAnEmptyStdoutIsNotACleanBillOfHealth(t *testing.T) {
	s := sh(t, `exit 0`, time.Minute)
	_, err := s.Investigate(context.Background(), "p")
	if err == nil {
		t.Fatal("a subject that printed nothing was read as reporting nothing wrong")
	}
	if !strings.Contains(err.Error(), "printed nothing") {
		t.Errorf("error = %v, want it to say the subject printed nothing", err)
	}
}

func TestAnExecSubjectWithNoCommandRefuses(t *testing.T) {
	if _, err := (&Exec{}).Investigate(context.Background(), "p"); err == nil {
		t.Fatal("an Exec with no argv ran something")
	}
	if got := (&Exec{}).Name(); got != "exec" {
		t.Errorf("Name = %q, want a placeholder rather than a panic", got)
	}
}

func TestParseReportTakesTheLastObjectFromANarratingSubject(t *testing.T) {
	out := []byte(`Thinking about the cluster...
{"progress":"listing pods"}
Here is what I found:
{"findings":[{"kind":"Deployment","resource_name":"web"}]}
`)
	r, err := ParseReport(out)
	if err != nil {
		t.Fatalf("ParseReport: %v", err)
	}
	if len(r.Findings) != 1 || r.Findings[0].ResourceName != "web" {
		t.Fatalf("findings = %+v, want the last object", r.Findings)
	}
}

// A progress object with no findings key must not decode as a clean report.
// Without the key check it reads as the subject calmly reporting a healthy
// cluster, which is a wrong answer that is indistinguishable from a right one
// on a control.
func TestStrayJSONIsNotAReport(t *testing.T) {
	for _, out := range []string{
		`{"progress":"listing pods"}`,
		`{"status":"ok"}` + "\n" + `{"elapsed_ms":42}`,
		`[1,2,3]`,
		`not json at all`,
	} {
		if _, err := ParseReport([]byte(out)); err == nil {
			t.Errorf("ParseReport(%q) succeeded; that is not a report", out)
		}
	}
}

func TestNullFindingsIsAnAnswerNotAnAbsence(t *testing.T) {
	r, err := ParseReport([]byte(`{"findings":null}`))
	if err != nil {
		t.Fatalf("ParseReport: %v", err)
	}
	if r.Findings == nil {
		t.Fatal("Findings is nil; the subject said nothing is wrong, which is an answer")
	}
}

// A subject's native report is richer than the graded triple. Refusing to read
// the triple out of it would force every subject to grow a Simian-shaped
// output mode, which is exactly the coupling this package exists to avoid.
func TestUnknownFieldsAreAllowed(t *testing.T) {
	r, err := ParseReport([]byte(`{"summary":"api is down","confidence":0.8,"findings":[{"kind":"Pod","resource_name":"api","remediation":"restart it"}]}`))
	if err != nil {
		t.Fatalf("ParseReport: %v", err)
	}
	if len(r.Findings) != 1 || r.Findings[0].ResourceName != "api" {
		t.Fatalf("findings = %+v, want the one finding", r.Findings)
	}
}

// Braces inside strings are not structure. A narrating subject that prints
// `{"log":"applied {chaos}"}` before its report must not have its report
// truncated by a brace inside a quoted string.
func TestBracesInsideStringsDoNotCount(t *testing.T) {
	out := []byte(`{"log":"applied {chaos} to \"api\""}
{"findings":[{"kind":"Pod","resource_name":"api"}]}`)
	r, err := ParseReport(out)
	if err != nil {
		t.Fatalf("ParseReport: %v", err)
	}
	if len(r.Findings) != 1 {
		t.Fatalf("findings = %+v, want one", r.Findings)
	}
}

func TestAStrayClosingBraceInProseIsIgnored(t *testing.T) {
	r, err := ParseReport([]byte("all done} \n" + `{"findings":[]}`))
	if err != nil {
		t.Fatalf("ParseReport: %v", err)
	}
	if r.Findings == nil {
		t.Fatal("Findings is nil")
	}
}

// An agent that streams a million tokens of reasoning must not take the
// harness with it.
func TestCapturedOutputIsBounded(t *testing.T) {
	c := &capped{buf: &bytes.Buffer{}, limit: 10}
	n, err := c.Write([]byte("0123456789abcdef"))
	if err != nil || n != 16 {
		t.Fatalf("Write = %d, %v; a capped writer must claim the whole write", n, err)
	}
	if got := c.buf.Len(); got != 10 {
		t.Errorf("buffered %d bytes, want the limit of 10", got)
	}
	if n, err := c.Write([]byte("more")); err != nil || n != 4 {
		t.Fatalf("Write past the limit = %d, %v", n, err)
	}
	if got := c.buf.Len(); got != 10 {
		t.Errorf("buffered %d bytes after a second write, want 10", got)
	}
}
