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

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/simian-agent/pkg/eval"
	"github.com/go-steer/simian-agent/pkg/scenario"
)

const scenarioYAML = `id: s-1
name: checkout image pull
prompt: Something is wrong in namespace shop. Investigate and report what you find.
source: pack:parity
severity: critical
faults:
  - engine: kube-state
    api_version: apps/v1
    resource_kind: ImageUnresolvable
    duration: 5m
    spec:
      name: catalog-sync
    targets:
      - namespace: shop
expect:
  - kind: Pod
    name: catalog-sync
    reasons: ["ImagePullBackOff"]
`

// auditLog is the shape SLogAuditor writes: one JSON object per line, with the
// scenario ID the audit sink stamps from the context.
const auditLog = `{"time":"2026-09-05T11:59:58Z","level":"INFO","msg":"audit","component":"audit","event":"executor.received","ts":"2026-09-05T11:59:58Z","fault_uid":"f-1","scenario_id":"s-1"}
{"time":"2026-09-05T11:59:59Z","level":"INFO","msg":"audit","component":"audit","event":"driver.applied","ts":"2026-09-05T11:59:59Z","fault_uid":"f-1","scenario_id":"s-1","payload":{"engine_uid":"e-1"}}
{"time":"2026-09-05T12:00:04Z","level":"INFO","msg":"audit","component":"audit","event":"fault.efficacy","ts":"2026-09-05T12:00:04Z","fault_uid":"f-1","scenario_id":"s-1","payload":{"probe":"image-pull-failed","mode":"Settle","passed":true,"observed":"ImagePullBackOff","expected":"ImagePullBackOff"}}
{"time":"2026-09-05T12:05:00Z","level":"INFO","msg":"audit","component":"audit","event":"lease.expired","ts":"2026-09-05T12:05:00Z","fault_uid":"f-1","reason":"deadline-reached"}
`

const reportJSON = `{
  "subject": "core-sre-agent",
  "runs": [{
    "scenario_id": "s-1",
    "detected_at": "2026-09-05T12:00:51Z",
    "report": {
      "overall_severity": "critical",
      "findings": [
        {"kind":"Pod","resource_name":"catalog-sync-4kd2","reason":"ImagePullBackOff","severity":"critical","namespace":"shop"},
        {"kind":"Deployment","resource_name":"catalog-sync","reason":"NoResourceLimits","severity":"warning","namespace":"shop"}
      ]
    }
  }]
}`

// artifacts writes a complete run to a temp dir and returns options pointing
// at it. audit defaults to the clean log when empty.
func artifacts(t *testing.T, auditBody string) evaluateOptions {
	t.Helper()
	dir := t.TempDir()
	pack := filepath.Join(dir, "parity")
	if err := os.MkdirAll(pack, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	write := func(path, body string) string {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		return path
	}
	write(filepath.Join(pack, "s-1.yaml"), scenarioYAML)
	if auditBody == "" {
		auditBody = auditLog
	}
	return evaluateOptions{
		packRef:     pack,
		auditPath:   write(filepath.Join(dir, "run.log"), auditBody),
		reportPath:  write(filepath.Join(dir, "agent.json"), reportJSON),
		format:      "text",
		minEfficacy: eval.DefaultMinEfficacy,
	}
}

func TestEvaluateScoresARunFromItsArtifacts(t *testing.T) {
	var out bytes.Buffer
	if err := runEvaluate(artifacts(t, ""), &out); err != nil {
		t.Fatalf("runEvaluate: %v", err)
	}
	got := out.String()

	for _, want := range []string{
		"subject=core-sre-agent",
		"pack=parity",
		"efficacy rate    1.00",
		"recall",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("scorecard is missing %q:\n%s", want, got)
		}
	}
	// 11:59:58 received, gate passed 12:00:04, report back 12:00:51 — the
	// clock starts at the gate, not at apply, so 47s and not 53s.
	if !strings.Contains(got, "47.0s") {
		t.Errorf("time to detect is not measured from the efficacy gate:\n%s", got)
	}
}

// The same run through --format json is the same numbers, because both come
// off one Summary. This is the property that lets a scorecard be re-rendered
// or diffed long after the run.
func TestEvaluateJSONAndTextAgree(t *testing.T) {
	opts := artifacts(t, "")

	var textOut bytes.Buffer
	if err := runEvaluate(opts, &textOut); err != nil {
		t.Fatalf("text: %v", err)
	}

	opts.format = "json"
	var jsonOut bytes.Buffer
	if err := runEvaluate(opts, &jsonOut); err != nil {
		t.Fatalf("json: %v", err)
	}

	var summary eval.Summary
	if err := json.Unmarshal(jsonOut.Bytes(), &summary); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if summary.Subject != "core-sre-agent" || summary.EfficacyRate != 1 {
		t.Errorf("summary = %+v", summary)
	}
	if summary.Means["recall"] != 1 {
		t.Errorf("recall = %v, want 1", summary.Means["recall"])
	}
	if !strings.Contains(textOut.String(), "recall") {
		t.Errorf("text render lost the measure:\n%s", textOut.String())
	}
}

// Scoring is pure, so the same artifacts produce byte-identical output. That
// is the whole claim the offline scorer rests on.
func TestEvaluateIsReproducible(t *testing.T) {
	opts := artifacts(t, "")
	var first bytes.Buffer
	if err := runEvaluate(opts, &first); err != nil {
		t.Fatalf("runEvaluate: %v", err)
	}
	for i := range 5 {
		var again bytes.Buffer
		if err := runEvaluate(opts, &again); err != nil {
			t.Fatalf("runEvaluate: %v", err)
		}
		if again.String() != first.String() {
			t.Fatalf("run %d differed:\n%s\n---\n%s", i, first.String(), again.String())
		}
	}
}

// The acceptance criterion, at the command level: a scenario with no passing
// efficacy record does not quietly become a subject miss.
func TestEvaluateFailsLoudlyWithNoEfficacyRecord(t *testing.T) {
	var kept []string
	for _, line := range strings.Split(strings.TrimSpace(auditLog), "\n") {
		if !strings.Contains(line, "fault.efficacy") {
			kept = append(kept, line)
		}
	}

	var out bytes.Buffer
	err := runEvaluate(artifacts(t, strings.Join(kept, "\n")+"\n"), &out)
	if err == nil {
		t.Fatal("a run that never manifested was scored without complaint")
	}
	for _, want := range []string{"efficacy rate 0.00", "measures the harness and not the subject"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}

	// Printed before it is refused: the rows are what explain the refusal.
	got := out.String()
	if !strings.Contains(got, "NOT SCORED") || !strings.Contains(got, "no passing efficacy record") {
		t.Errorf("the scorecard did not explain itself before failing:\n%s", got)
	}
	// And the subject is not charged for it.
	if strings.Contains(got, "recall              0.00") {
		t.Errorf("an uninjected scenario was scored as a miss:\n%s", got)
	}
}

// A rig with a known-flaky gate can still ask for the numbers, but has to say
// so out loud rather than getting them by default.
func TestMinEfficacyCanBeLowered(t *testing.T) {
	var kept []string
	for _, line := range strings.Split(strings.TrimSpace(auditLog), "\n") {
		if !strings.Contains(line, "fault.efficacy") {
			kept = append(kept, line)
		}
	}
	opts := artifacts(t, strings.Join(kept, "\n")+"\n")
	opts.minEfficacy = 0

	var out bytes.Buffer
	if err := runEvaluate(opts, &out); err != nil {
		t.Fatalf("runEvaluate with --min-efficacy 0: %v", err)
	}
	// Lowering the bar suppresses the exit code, never the warning.
	if !strings.Contains(out.String(), "unmeasured, not as poor") {
		t.Errorf("the warning was suppressed along with the error:\n%s", out.String())
	}
}

func TestEvaluateRejectsIncompleteInvocations(t *testing.T) {
	base := artifacts(t, "")
	for _, tc := range []struct {
		name string
		edit func(*evaluateOptions)
		want string
	}{
		{"no pack", func(o *evaluateOptions) { o.packRef = "" }, "--pack is required"},
		{"no audit", func(o *evaluateOptions) { o.auditPath = "" }, "--audit is required"},
		{"no report", func(o *evaluateOptions) { o.reportPath = "" }, "--report is required"},
		{"bad format", func(o *evaluateOptions) { o.format = "yaml" }, "want text or json"},
		{"missing pack dir", func(o *evaluateOptions) { o.packRef = filepath.Join(o.packRef, "nope") }, "open pack"},
		{"pack is a file", func(o *evaluateOptions) { o.packRef = o.reportPath }, "not a directory"},
		{"missing audit", func(o *evaluateOptions) { o.auditPath += ".gone" }, "open audit log"},
		{"missing report", func(o *evaluateOptions) { o.reportPath += ".gone" }, "open report"},
	} {
		opts := base
		tc.edit(&opts)
		err := runEvaluate(opts, &bytes.Buffer{})
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
}

// A trailing slash is how a shell completes a directory, and it must not turn
// the pack's name into "".
func TestLoadPackDirToleratesATrailingSlash(t *testing.T) {
	opts := artifacts(t, "")
	pack, err := scenario.LoadRef(opts.packRef + string(filepath.Separator))
	if err != nil {
		t.Fatalf("LoadRef: %v", err)
	}
	if pack.Name != "parity" {
		t.Errorf("pack name = %q, want parity", pack.Name)
	}
}

// The Cobra wiring is the part a user actually types, so it gets exercised as
// typed: flags parsed off an argv, output on the command's own writer.
func TestEvaluateCmdWiresItsFlags(t *testing.T) {
	opts := artifacts(t, "")
	cmd := newEvaluateCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{
		"--pack", opts.packRef,
		"--audit", opts.auditPath,
		"--report", opts.reportPath,
		"--format", "json",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var summary eval.Summary
	if err := json.Unmarshal(out.Bytes(), &summary); err != nil {
		t.Fatalf("unmarshal %q: %v", out.String(), err)
	}
	if summary.Subject != "core-sre-agent" {
		t.Errorf("summary = %+v", summary)
	}
}

// An audit log routinely arrives down a pipe — `kubectl logs ... | simian
// evaluate --audit -` is the shortest path from a live controller to a
// scorecard.
func TestEvaluateReadsTheAuditLogFromStdin(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	go func() {
		defer func() { _ = w.Close() }()
		_, _ = io.WriteString(w, auditLog)
	}()

	stdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = stdin; _ = r.Close() })

	opts := artifacts(t, "")
	opts.auditPath = "-"
	var out bytes.Buffer
	if err := runEvaluate(opts, &out); err != nil {
		t.Fatalf("runEvaluate: %v", err)
	}
	if !strings.Contains(out.String(), "efficacy rate    1.00") {
		t.Errorf("the piped log was not scored:\n%s", out.String())
	}
}

// Nothing in the command touches a cluster, so it must work with no
// kubeconfig at all — that is what makes it usable where the clusters are
// long-lived and nobody will stand up a harness.
func TestEvaluateNeedsNoCluster(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "does-not-exist"))
	t.Setenv("HOME", t.TempDir())
	if err := runEvaluate(artifacts(t, ""), &bytes.Buffer{}); err != nil {
		t.Fatalf("runEvaluate without a kubeconfig: %v", err)
	}
}
