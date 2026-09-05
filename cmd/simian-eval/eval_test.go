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
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/eval"
	"github.com/go-steer/simian-agent/pkg/harness"
	"github.com/go-steer/simian-agent/pkg/scenario"
	"github.com/go-steer/simian-agent/pkg/simian"
)

const parityScenario = `id: parity-0001
name: checkout image pull
prompt: Something is wrong in namespace shop. Investigate and report what you find.
source: pack:parity
severity: critical
faults:
  - engine: kube-state
    api_version: apps/v1
    resource_kind: ImageUnresolvable
    duration: 30m
    spec:
      name: catalog-sync
    targets:
      - namespace: shop
expect:
  - kind: Pod
    name: catalog-sync
    reasons: ["ImagePullBackOff"]
`

const lookoutScenario = `id: lookout-0001
name: payments cannot pull its image
prompt: Something is wrong in namespace pay. Investigate and report what you find.
source: pack:lookout
severity: critical
faults:
  - engine: kube-state
    api_version: apps/v1
    resource_kind: ImageUnresolvable
    duration: 30m
    spec:
      name: payments
    targets:
      - namespace: pay
expect:
  - kind: Pod
    name: payments
    reasons: ["ImagePullBackOff"]
`

// packDir writes one scenario file into a named pack directory and returns the
// directory.
func packDir(t *testing.T, name, body string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "s.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write scenario: %v", err)
	}
	return dir
}

func testOptions(packs ...string) *options {
	o := defaultOptions()
	o.packDirs = packs
	o.subject = "noop:"
	return o
}

// The flags that decide whether a number means anything default to the strict
// answer: the efficacy gates are on, the score is checked against a floor, and
// the cluster is the one the operator already has rather than one this rig
// decided to create.
func TestTheDefaultsAreTheCarefulOnes(t *testing.T) {
	o := defaultOptions()
	if o.cluster != ClusterCurrent {
		t.Errorf("cluster = %q, want %q; a rig must not create clusters unasked", o.cluster, ClusterCurrent)
	}
	if !o.defaultProbes {
		t.Error("efficacy probes are off by default; a fault the cluster silently drops would score as landed")
	}
	if !o.score {
		t.Error("scoring is off by default")
	}
	if o.minEfficacy != eval.DefaultMinEfficacy {
		t.Errorf("minEfficacy = %v, want %v", o.minEfficacy, eval.DefaultMinEfficacy)
	}
	if o.keepArenas || o.skipDurationOK {
		t.Errorf("options = %+v, want the leave-nothing-behind defaults", o)
	}
}

func TestValidateRefusesInvocationsThatCannotBeScored(t *testing.T) {
	cases := []struct {
		name string
		edit func(*options)
		want string
	}{
		{"no pack", func(o *options) { o.packDirs = nil }, "--pack is required"},
		{"no subject", func(o *options) { o.subject = "" }, "--subject is required"},
		{"bad format", func(o *options) { o.format = "yaml" }, "want text or json"},
		{"bad cluster", func(o *options) { o.cluster = "gke" }, "want current or kind"},
		{"negative concurrency", func(o *options) { o.concurrency = -1 }, "must not be negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := testOptions("packs/parity")
			tc.edit(o)
			err := o.validate()
			if err == nil {
				t.Fatalf("validate accepted %+v", o)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
	if err := testOptions("packs/parity").validate(); err != nil {
		t.Errorf("validate rejected a complete invocation: %v", err)
	}
}

func TestLoadPacksNamesASinglePackAfterItsDirectory(t *testing.T) {
	dir := packDir(t, "parity", parityScenario)
	pack, err := loadPacks([]string{dir + string(filepath.Separator)})
	if err != nil {
		t.Fatalf("loadPacks: %v", err)
	}
	if pack.Name != "parity" || pack.Len() != 1 {
		t.Errorf("pack = %q with %d scenarios, want parity with 1", pack.Name, pack.Len())
	}
}

// Two packs run as one suite rather than back to back, because the namespace
// fencing, the concurrency ceiling and the scorecard are all defined over one
// set of scenarios.
func TestLoadPacksMergesSeveralDirectoriesIntoOneSuite(t *testing.T) {
	pack, err := loadPacks([]string{
		packDir(t, "parity", parityScenario),
		packDir(t, "lookout", lookoutScenario),
	})
	if err != nil {
		t.Fatalf("loadPacks: %v", err)
	}
	if pack.Name != "parity+lookout" {
		t.Errorf("pack name = %q, want parity+lookout; the scorecard has to say what was run", pack.Name)
	}
	if pack.Len() != 2 {
		t.Fatalf("merged pack has %d scenarios, want 2", pack.Len())
	}
	if _, ok := pack.ByID("lookout-0001"); !ok {
		t.Error("the second pack's scenarios did not survive the merge")
	}
}

// Scenario IDs are the key the audit log joins to the run file on. Two packs
// sharing one is not a merge conflict to resolve quietly — it silently pools
// two scenarios' evidence into a score that looks entirely plausible.
func TestLoadPacksRefusesTwoPacksThatShareAScenarioID(t *testing.T) {
	_, err := loadPacks([]string{
		packDir(t, "parity", parityScenario),
		packDir(t, "copy", parityScenario),
	})
	if err == nil {
		t.Fatal("loadPacks merged two packs with a duplicate scenario ID")
	}
	if !strings.Contains(err.Error(), "duplicate scenario ID") {
		t.Errorf("error = %v, want it to name the duplicate", err)
	}
}

func TestLoadPacksRefusesWhatItCannotRead(t *testing.T) {
	dir := packDir(t, "parity", parityScenario)
	cases := []struct {
		name string
		dirs []string
		want string
	}{
		{"none", nil, "no pack directories"},
		{"missing", []string{filepath.Join(dir, "nope")}, "open pack"},
		{"a file", []string{filepath.Join(dir, "s.yaml")}, "not a directory"},
		{"one of several missing", []string{dir, filepath.Join(dir, "nope")}, "open pack"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := loadPacks(tc.dirs); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("loadPacks(%v) = %v, want an error mentioning %q", tc.dirs, err, tc.want)
			}
		})
	}
}

// A fault that expires mid-investigation is cleared by the reaper, and the
// harness — watching for exactly that disappearance — records it as the
// subject having remediated something it never touched. Refused up front,
// because the alternative is finding it in a suspiciously good scorecard.
func TestFaultsShorterThanTheSubjectTimeoutAreRefused(t *testing.T) {
	pack := scenario.Pack{Name: "p", Scenarios: []scenario.Scenario{{
		ID:     "s-1",
		Faults: []simian.FaultManifest{{Duration: 2 * time.Minute}},
	}}}

	err := checkFaultDurations(pack, 10*time.Minute)
	if err == nil {
		t.Fatal("a 2m fault was accepted against a 10m subject timeout")
	}
	for _, want := range []string{"s-1 (2m0s)", "--allow-short-faults"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}

	if err := checkFaultDurations(pack, time.Minute); err != nil {
		t.Errorf("a 2m fault was refused against a 1m subject timeout: %v", err)
	}
	if err := checkFaultDurations(pack, 0); err != nil {
		t.Errorf("an unbounded subject timeout has nothing to compare against: %v", err)
	}
}

// An unset duration is the executor's business, not this check's: it applies
// the ceiling, and guessing here would refuse scenarios that are fine.
func TestAnUnsetFaultDurationIsNotShort(t *testing.T) {
	pack := scenario.Pack{Name: "p", Scenarios: []scenario.Scenario{{
		ID:     "s-1",
		Faults: []simian.FaultManifest{{}},
	}}}
	if err := checkFaultDurations(pack, time.Hour); err != nil {
		t.Errorf("checkFaultDurations refused a fault with no duration set: %v", err)
	}
}

// writeArtifacts writes the pair of files a completed run leaves behind, the
// same way runEval writes them: the audit log through the audit sink, the run
// file through the harness writer.
func writeArtifacts(t *testing.T, dir string, passed bool) (auditPath, runPath string) {
	t.Helper()
	auditPath = filepath.Join(dir, harness.AuditFileName)
	f, err := os.Create(auditPath)
	if err != nil {
		t.Fatalf("create audit log: %v", err)
	}
	defer func() { _ = f.Close() }()

	auditor := audit.New(slog.New(slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo})))
	ctx := t.Context()
	auditor.Emit(ctx, simian.AuditEvent{Event: audit.EventEvalScenarioStarted, ScenarioID: "parity-0001"})
	auditor.Emit(ctx, simian.AuditEvent{Event: audit.EventDriverApplied, ScenarioID: "parity-0001", FaultUID: "f-1"})
	auditor.Emit(ctx, simian.AuditEvent{
		Event: audit.EventFaultEfficacy, ScenarioID: "parity-0001", FaultUID: "f-1",
		Payload: map[string]any{"probe": "image-pull-failed", "passed": passed, "expected": "ImagePullBackOff", "observed": "Running"},
	})

	runPath, err = harness.WriteRunFileTo(dir, harness.RunFile("agent", []eval.Run{{
		ScenarioID: "parity-0001",
		Report: &eval.Report{
			OverallSeverity: scenario.SeverityCritical,
			Findings: []scenario.Finding{{
				Kind: "Pod", ResourceName: "catalog-sync-4kd2", Namespace: "shop",
				Reason: "ImagePullBackOff", Severity: scenario.SeverityCritical,
			}},
		},
		DetectedAt: time.Now(),
	}}))
	if err != nil {
		t.Fatalf("WriteRunFileTo: %v", err)
	}
	return auditPath, runPath
}

// The scorecard printed at the end of a run comes off the files, not off the
// runs still in memory. Going through the artifacts is what proves they are
// scoreable — if `simian evaluate` could not reproduce this number tomorrow,
// the run finds out now, while the cluster is still there to look at.
func TestScoreArtifactsReadsBackWhatTheRunWrote(t *testing.T) {
	dir := t.TempDir()
	auditPath, runPath := writeArtifacts(t, dir, true)
	pack, err := loadPacks([]string{packDir(t, "parity", parityScenario)})
	if err != nil {
		t.Fatalf("loadPacks: %v", err)
	}

	var out bytes.Buffer
	if err := scoreArtifacts(testOptions(), pack, auditPath, runPath, &out); err != nil {
		t.Fatalf("scoreArtifacts: %v", err)
	}
	for _, want := range []string{"subject=agent", "pack=parity", "efficacy rate    1.00", "recall"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("scorecard is missing %q:\n%s", want, out.String())
		}
	}
}

func TestScoreArtifactsRendersJSON(t *testing.T) {
	dir := t.TempDir()
	auditPath, runPath := writeArtifacts(t, dir, true)
	pack, err := loadPacks([]string{packDir(t, "parity", parityScenario)})
	if err != nil {
		t.Fatalf("loadPacks: %v", err)
	}
	o := testOptions()
	o.format = "json"

	var out bytes.Buffer
	if err := scoreArtifacts(o, pack, auditPath, runPath, &out); err != nil {
		t.Fatalf("scoreArtifacts: %v", err)
	}
	var summary eval.Summary
	if err := json.Unmarshal(out.Bytes(), &summary); err != nil {
		t.Fatalf("unmarshal %q: %v", out.String(), err)
	}
	if summary.Subject != "agent" || summary.Pack != "parity" {
		t.Errorf("summary = %+v, want the subject and pack carried through", summary)
	}
}

// Below the floor the command fails — but it prints first. The rows that
// explain a low efficacy rate are exactly what an operator needs to see, and
// exiting before printing them hides the evidence.
func TestAScorecardBelowTheFloorIsPrintedAndThenRefused(t *testing.T) {
	dir := t.TempDir()
	auditPath, runPath := writeArtifacts(t, dir, false)
	pack, err := loadPacks([]string{packDir(t, "parity", parityScenario)})
	if err != nil {
		t.Fatalf("loadPacks: %v", err)
	}

	var out bytes.Buffer
	err = scoreArtifacts(testOptions(), pack, auditPath, runPath, &out)
	if err == nil {
		t.Fatal("a run where nothing manifested was reported as a pass")
	}
	if !strings.Contains(err.Error(), "--min-efficacy") {
		t.Errorf("error = %v, want it to name the floor it fell below", err)
	}
	if out.Len() == 0 {
		t.Error("the scorecard was suppressed along with the exit code")
	}

	// And the same artifacts pass once the rig says out loud that it knows.
	o := testOptions()
	o.minEfficacy = 0
	if err := scoreArtifacts(o, pack, auditPath, runPath, &bytes.Buffer{}); err != nil {
		t.Errorf("--min-efficacy 0 still refused the run: %v", err)
	}
}

func TestScoreArtifactsRefusesFilesItCannotRead(t *testing.T) {
	dir := t.TempDir()
	auditPath, runPath := writeArtifacts(t, dir, true)
	pack, err := loadPacks([]string{packDir(t, "parity", parityScenario)})
	if err != nil {
		t.Fatalf("loadPacks: %v", err)
	}
	cases := []struct {
		name, audit, run, want string
	}{
		{"missing audit", auditPath + ".gone", runPath, "open audit log"},
		{"missing run file", auditPath, runPath + ".gone", "open run file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := scoreArtifacts(testOptions(), pack, tc.audit, tc.run, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("scoreArtifacts = %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
}

// Everything that can be refused without a cluster is refused without a
// cluster: standing one up to discover the subject was misspelled costs
// minutes, and on a shared cluster it also creates namespaces.
func TestABadInvocationIsRefusedBeforeTheClusterIsTouched(t *testing.T) {
	dir := t.TempDir()
	pack := packDir(t, "parity", parityScenario)

	cases := []struct {
		name string
		edit func(*options)
		want string
	}{
		{"unusable subject", func(o *options) { o.subject = "grpc://agent" }, "unknown scheme"},
		{"pack that does not exist", func(o *options) { o.packDirs = []string{filepath.Join(dir, "nope")} }, "open pack"},
		{"short faults", func(o *options) { o.subjectTimeout = time.Hour }, "--allow-short-faults"},
		{"a format nobody can print", func(o *options) { o.format = "yaml" }, "want text or json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "run")
			o := testOptions(pack)
			o.out = out
			o.kubeconfig = filepath.Join(dir, "there-is-no-cluster")
			tc.edit(o)

			err := runEval(t.Context(), o, &bytes.Buffer{}, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runEval = %v, want an error mentioning %q", err, tc.want)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Errorf("the output directory was created before the invocation was checked: %v", err)
			}
		})
	}
}

// Two runs must not write over each other's evidence, so the default output
// directory is per run. The audit log is opened before the cluster is touched
// for the same reason: an event emitted while the log is still a plan is an
// event nobody can score.
func TestTheDefaultOutputDirectoryIsPerRunAndOpenedFirst(t *testing.T) {
	t.Chdir(t.TempDir())

	o := testOptions(packDir(t, "parity", parityScenario))
	o.kubeconfig = filepath.Join(t.TempDir(), "there-is-no-cluster")
	if err := runEval(t.Context(), o, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("runEval succeeded without a cluster")
	}

	entries, err := os.ReadDir("runs")
	if err != nil {
		t.Fatalf("the run wrote nothing under runs/: %v", err)
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		t.Fatalf("runs/ holds %v, want one directory named for the run", entries)
	}
	if _, err := time.Parse("20060102-150405", entries[0].Name()); err != nil {
		t.Errorf("runs/%s is not named for its run: %v", entries[0].Name(), err)
	}
	if _, err := os.Stat(filepath.Join("runs", entries[0].Name(), harness.AuditFileName)); err != nil {
		t.Errorf("the audit log was not opened before the cluster was reached for: %v", err)
	}
}

// The Cobra wiring is the part a user actually types, so it is exercised as
// typed: an argv parsed onto the options the command will run with.
func TestTheFlagsBindToTheOptionsTheRunUses(t *testing.T) {
	o := defaultOptions()
	cmd := newRootCmd(o)
	err := cmd.ParseFlags([]string{
		"--pack", "packs/parity,packs/lookout",
		"--subject", "exec:./bin/lookout --json",
		"--only", "parity-0001",
		"--out", "runs/now",
		"--cluster", "kind",
		"--concurrency", "4",
		"--subject-timeout", "90s",
		"--subject-env", "API_KEY=x",
		"--eligible-namespace", "shop,pay",
		"--keep-arenas",
		"--format", "json",
		"--min-efficacy", "0.5",
		"--score=false",
	})
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}

	if got, want := strings.Join(o.packDirs, ","), "packs/parity,packs/lookout"; got != want {
		t.Errorf("packDirs = %q, want %q", got, want)
	}
	if o.subject != "exec:./bin/lookout --json" {
		t.Errorf("subject = %q, want the whole command line", o.subject)
	}
	if len(o.only) != 1 || o.only[0] != "parity-0001" {
		t.Errorf("only = %v", o.only)
	}
	if o.cluster != ClusterKind || o.concurrency != 4 || o.subjectTimeout != 90*time.Second {
		t.Errorf("options = %+v", o)
	}
	if len(o.subjectEnv) != 1 || len(o.eligibleNS) != 2 {
		t.Errorf("subjectEnv = %v, eligibleNS = %v", o.subjectEnv, o.eligibleNS)
	}
	if !o.keepArenas || o.format != "json" || o.minEfficacy != 0.5 || o.score {
		t.Errorf("options = %+v", o)
	}
	if err := o.validate(); err != nil {
		t.Errorf("a plausible argv did not validate: %v", err)
	}
}

// A run that dies halfway leaves an arena behind. The run ID is on the
// namespace, in the output directory's name and on the kind cluster, so any
// one of the three leads back to the other two.
func TestTheRunIDTiesAnAbandonedArenaBackToItsRun(t *testing.T) {
	id := newRunID()
	if _, err := time.Parse("20060102-150405", id); err != nil {
		t.Errorf("run ID %q is not the sortable stamp: %v", id, err)
	}
	if strings.ContainsAny(id, "/:. ") {
		t.Errorf("run ID %q is not safe as a filename, a cluster name and an annotation value", id)
	}
	if got := arenaAnnotations(id)[EvalRunAnnotation]; got != id {
		t.Errorf("arena annotations = %v, want the run ID under %s", arenaAnnotations(id), EvalRunAnnotation)
	}
}
