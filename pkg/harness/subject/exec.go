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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-steer/simian-agent/pkg/eval"
	"github.com/go-steer/simian-agent/pkg/scenario"
)

// PromptPlaceholder is substituted in argv, when it appears there, with the
// scenario's prompt.
const PromptPlaceholder = "{prompt}"

// PromptEnv carries the prompt to subjects that would rather read it from the
// environment than from stdin — a shell script, mostly.
const PromptEnv = "SIMIAN_PROMPT"

// maxCaptured bounds what is kept from either stream. An agent that decides to
// stream a million tokens of reasoning must not take the harness with it.
const maxCaptured = 8 << 20 // 8 MiB

// stderrTail is how much of stderr is quoted back in an error. Enough to see
// the panic, not enough to bury the scorecard.
const stderrTail = 2000

// waitDelay is how long a killed subject has to let go of its output pipes.
//
// Killing the process does not close them: a grandchild that inherited stdout
// holds the pipe open, and cmd.Run blocks reading it long after the subject
// that timed out is gone. Without this, a subject that shells out to something
// slow makes its own timeout meaningless — the harness waits for the
// grandchild instead, still holding a fault in a live cluster.
const waitDelay = 5 * time.Second

// Exec runs a subject as a child process and reads a JSON report off stdout.
//
// This is the adapter that matters: it covers k8s-lookout, core-sre-agent,
// mast workload bundles, `claude -p`, `gemini-cli`, and a shell script, which
// is to say every subject anyone has actually asked to benchmark. `http:` and
// `mcp:` are conveniences on top of a boundary this one already establishes.
type Exec struct {
	// Argv is the command and its arguments, already split. An element equal
	// to or containing PromptPlaceholder receives the prompt.
	Argv []string

	// Timeout bounds one investigation. Zero means unbounded.
	Timeout time.Duration

	// Dir is the working directory. Empty inherits the harness's.
	Dir string

	// Env is appended to the parent environment.
	Env []string
}

// Name implements eval.Subject. The base name of the binary is what a reader
// of a scorecard recognises; the full argv is noise in a column header.
func (e *Exec) Name() string {
	if len(e.Argv) == 0 {
		return "exec"
	}
	return filepath.Base(e.Argv[0])
}

// Investigate runs the subject once and parses its answer.
//
// The prompt is delivered exactly one way. If argv mentions the placeholder it
// goes there and stdin is closed empty; otherwise it goes on stdin. Delivering
// it twice would look harmless and would quietly double the task for any
// subject that reads both.
func (e *Exec) Investigate(ctx context.Context, prompt string) (eval.Report, error) {
	if len(e.Argv) == 0 {
		return eval.Report{}, errors.New("subject: exec adapter has no command")
	}

	if e.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.Timeout)
		defer cancel()
	}

	argv, inArgv := substitutePrompt(e.Argv, prompt)

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // running the operator's named subject is the whole job
	cmd.WaitDelay = waitDelay
	cmd.Dir = e.Dir
	cmd.Env = append(append(os.Environ(), e.Env...), PromptEnv+"="+prompt)
	if !inArgv {
		cmd.Stdin = strings.NewReader(prompt)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &capped{buf: &stdout, limit: maxCaptured}
	cmd.Stderr = &capped{buf: &stderr, limit: maxCaptured}

	err := cmd.Run()
	// The context is checked before the exit status. A killed process exits
	// non-zero, and reporting "exited 1" for a subject that ran out of time
	// sends whoever reads it looking for a bug in the subject.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return eval.Report{}, fmt.Errorf("subject %s: %w after %s%s", e.Name(), ctxErr, e.Timeout, quoted(stderr.String()))
	}
	if err != nil {
		return eval.Report{}, fmt.Errorf("subject %s: %w%s", e.Name(), err, quoted(stderr.String()))
	}

	report, err := ParseReport(stdout.Bytes())
	if err != nil {
		return eval.Report{}, fmt.Errorf("subject %s: %w", e.Name(), err)
	}
	return report, nil
}

// substitutePrompt replaces the placeholder wherever it appears in argv, and
// reports whether it appeared at all.
func substitutePrompt(argv []string, prompt string) ([]string, bool) {
	out := make([]string, len(argv))
	found := false
	for i, a := range argv {
		if strings.Contains(a, PromptPlaceholder) {
			found = true
			a = strings.ReplaceAll(a, PromptPlaceholder, prompt)
		}
		out[i] = a
	}
	return out, found
}

// ParseReport reads a subject's answer out of whatever it printed.
//
// Strict first: the clean case is a subject whose entire stdout is the report.
// Failing that, the last complete JSON object in the stream is tried, because
// an LLM subject narrates. `claude -p` explaining itself before it answers is
// not a malformed subject, and a harness that could only grade subjects which
// print nothing else would exclude most of the ones worth grading.
//
// The object must actually carry a "findings" key. Without that check, any
// stray JSON — a log line, a progress object — decodes into an empty Report
// and reads as the subject calmly reporting a healthy cluster, which is a
// wrong answer indistinguishable from a right one on a control.
func ParseReport(out []byte) (eval.Report, error) {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return eval.Report{}, errors.New("printed nothing on stdout; expected a JSON report")
	}

	if r, err := decodeReport(trimmed); err == nil {
		return r, nil
	}

	objects := topLevelObjects(trimmed)
	for i := len(objects) - 1; i >= 0; i-- {
		if r, err := decodeReport(objects[i]); err == nil {
			return r, nil
		}
	}
	return eval.Report{}, fmt.Errorf("no JSON report with a \"findings\" key on stdout; got %d byte(s) starting %q", len(trimmed), head(trimmed, 120))
}

func decodeReport(b []byte) (eval.Report, error) {
	// Unknown fields are allowed on purpose. A subject's native report is
	// richer than the graded triple — titles, remediation advice, its own
	// confidence — and refusing to read the triple out of it would force
	// every subject to grow a Simian-shaped output mode.
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		return eval.Report{}, err
	}
	if _, ok := keys["findings"]; !ok {
		return eval.Report{}, errors.New("JSON object has no \"findings\" key")
	}

	var r eval.Report
	if err := json.Unmarshal(b, &r); err != nil {
		return eval.Report{}, err
	}
	if r.Findings == nil {
		// "findings": null is the subject saying nothing is wrong. Kept
		// non-nil so a caller can tell it apart from a subject that never
		// answered, which is a different score entirely.
		r.Findings = []scenario.Finding{}
	}
	return r, nil
}

// topLevelObjects returns every balanced { ... } run at nesting depth zero, in
// the order they appear. Braces inside JSON strings do not count, which is the
// entire reason this is not a regexp.
func topLevelObjects(b []byte) [][]byte {
	var (
		out      [][]byte
		depth    int
		start    int
		inString bool
		escaped  bool
	)
	for i := range b {
		c := b[i]
		switch {
		case escaped:
			escaped = false
		case inString:
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
		case c == '"':
			inString = true
		case c == '{':
			if depth == 0 {
				start = i
			}
			depth++
		case c == '}':
			if depth == 0 {
				continue // a stray brace in prose
			}
			depth--
			if depth == 0 {
				out = append(out, b[start:i+1])
			}
		}
	}
	return out
}

// capped is an io.Writer that stops accepting past a limit rather than
// growing without bound. Truncation is silent because the alternative — an
// error mid-stream — kills the subject for being verbose.
type capped struct {
	buf   *bytes.Buffer
	limit int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.limit - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func quoted(stderr string) string {
	s := strings.TrimSpace(stderr)
	if s == "" {
		return ""
	}
	if len(s) > stderrTail {
		s = "..." + s[len(s)-stderrTail:]
	}
	return ": " + s
}

func head(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
