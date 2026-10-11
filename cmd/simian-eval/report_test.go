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
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/simian-agent/pkg/eval"
	"github.com/go-steer/simian-agent/pkg/scenario"
)

// Real runs, trimmed; see pkg/eval/archive_test.go.
const fixtureRuns = "../../pkg/eval/testdata/archive"

func fixtureDir(name string) string { return filepath.Join(fixtureRuns, name) }

// The report a user runs is the document BuildArchive makes, and each run in
// it carries the scorecard simian-eval printed for that run.
func TestReportMatchesEachRunsOwnScorecard(t *testing.T) {
	out := filepath.Join(t.TempDir(), "scorecard.json")
	dirs := []string{fixtureDir("dataplane-lookout"), fixtureDir("dataplane-vantage")}
	cmd := newRootCmd(defaultOptions())
	cmd.SetArgs([]string{"report", "--run", dirs[0], "--run", dirs[1], "--out", out})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("report: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var doc eval.Archive
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(doc.Generator, "simian-eval ") || len(doc.Runs) != 2 {
		t.Fatalf("generator=%q runs=%d", doc.Generator, len(doc.Runs))
	}

	pack := scenario.MustBuiltin(scenario.PackDataplane)
	for i, dir := range dirs {
		var own bytes.Buffer
		o := &options{format: "json"}
		if err := scoreArtifacts(o, pack, filepath.Join(dir, "audit.log"), filepath.Join(dir, "run.json"), &own); err != nil {
			t.Fatalf("scoreArtifacts(%s): %v", dir, err)
		}
		var sum eval.Summary
		if err := json.Unmarshal(own.Bytes(), &sum); err != nil {
			t.Fatal(err)
		}
		run := doc.Runs[i]
		if run.Label != filepath.Base(dir) || run.EfficacyRate != sum.EfficacyRate || !reflect.DeepEqual(run.Means, sum.Means) {
			t.Errorf("%s: run = %+v, want the scorecard %+v", dir, run, sum)
		}
		for _, res := range sum.Results {
			for _, s := range doc.Scenarios {
				if s.ID == res.ScenarioID && !reflect.DeepEqual(s.Results[run.ID].Scores, res.Scores) {
					t.Errorf("%s/%s: scores differ from the run's own scorecard", dir, s.ID)
				}
			}
		}
	}
}

// Other files in a run directory — a subject's transcripts — are listed so a
// reader knows they exist; the two artifacts are not.
func TestReportListsEvidenceFiles(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"audit.log", "run.json"} {
		b, err := os.ReadFile(filepath.Join(fixtureDir("dataplane-vantage"), f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "transcript-flow-payments.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	in, err := readRunDir(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in.Evidence, []string{"transcript-flow-payments.json"}) {
		t.Errorf("evidence = %v", in.Evidence)
	}
}

func TestReportWritesToStdoutAndRefusesBadInput(t *testing.T) {
	var stdout bytes.Buffer
	o := &reportOptions{runs: []string{fixtureDir("dataplane-lookout")}, minEfficacy: 0.8, now: func() time.Time { return time.Unix(0, 0) }}
	if err := runReport(o, &stdout); err != nil {
		t.Fatalf("runReport: %v", err)
	}
	if !json.Valid(stdout.Bytes()) || !bytes.Contains(stdout.Bytes(), []byte(`"format": "simian.scorecard"`)) {
		t.Errorf("stdout is not the document: %.200s", stdout.String())
	}

	for name, o := range map[string]*reportOptions{
		"no runs":     {},
		"missing dir": {runs: []string{filepath.Join(t.TempDir(), "gone")}},
	} {
		o.now = time.Now
		if err := runReport(o, &bytes.Buffer{}); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestResolveRunPack(t *testing.T) {
	p, err := resolveRunPack("parity+dataplane", nil)
	if err != nil {
		t.Fatalf("parity+dataplane: %v", err)
	}
	if p.Name != "parity+dataplane" || len(p.Scenarios) != len(scenario.MustBuiltin("parity").Scenarios)+len(scenario.MustBuiltin("dataplane").Scenarios) {
		t.Errorf("merged pack = %s with %d scenarios", p.Name, len(p.Scenarios))
	}
	custom := scenario.Pack{Name: "custom"}
	if p, err := resolveRunPack("custom", map[string]scenario.Pack{"custom": custom}); err != nil || p.Name != "custom" {
		t.Errorf("a --pack by name: %v %v", p.Name, err)
	}
	if p, err := resolveRunPack("", map[string]scenario.Pack{"custom": custom}); err != nil || p.Name != "custom" {
		t.Errorf("no recorded pack, one --pack: %v %v", p.Name, err)
	}
	if _, err := resolveRunPack("", nil); err == nil {
		t.Error("no recorded pack and no --pack: no error")
	}
}
