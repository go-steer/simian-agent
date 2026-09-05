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

package harness

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/simian-agent/pkg/eval"
	"github.com/go-steer/simian-agent/pkg/scenario"
)

func TestRunFileCarriesTheSubjectsSideOfTheRun(t *testing.T) {
	at := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	report := &eval.Report{Findings: []scenario.Finding{{Kind: "Pod", ResourceName: "api"}}}

	rf := RunFile("agent", []eval.Run{{
		ScenarioID: "s-1",
		Subject:    "agent",
		Report:     report,
		DetectedAt: at,
		ClearedAt:  at.Add(time.Minute),
	}})

	if rf.Subject != "agent" {
		t.Errorf("Subject = %q, want agent", rf.Subject)
	}
	if len(rf.Runs) != 1 {
		t.Fatalf("got %d records, want 1", len(rf.Runs))
	}
	rec := rf.Runs[0]
	if rec.ScenarioID != "s-1" || rec.Report != report {
		t.Errorf("record = %+v, want the subject's report under s-1", rec)
	}
	if !rec.DetectedAt.Equal(at) || !rec.ClearedAt.Equal(at.Add(time.Minute)) {
		t.Errorf("timestamps = %s / %s, want them carried through", rec.DetectedAt, rec.ClearedAt)
	}
}

// The three facts about whether the cluster was actually broken are not in the
// run file. Writing them would make it a place where Simian grades its own
// homework: a harness that both breaks the cluster and certifies that it broke
// it can be wrong in one direction and self-consistent about it. They come
// from the audit log, which the executor and the probes write as they go.
func TestTheRunFileDoesNotCertifyItsOwnInjection(t *testing.T) {
	rf := RunFile("agent", []eval.Run{{
		ScenarioID:  "s-1",
		Manifested:  true,
		InjectedAt:  time.Now(),
		InjectError: "this should not travel",
	}})

	var buf bytes.Buffer
	if err := WriteRunFile(&buf, rf); err != nil {
		t.Fatalf("WriteRunFile: %v", err)
	}
	for _, forbidden := range []string{"manifested", "injected_at", "inject_error", "this should not travel"} {
		if strings.Contains(buf.String(), forbidden) {
			t.Errorf("the run file contains %q; that fact belongs to the audit log", forbidden)
		}
	}
}

func TestASubjectErrorTravelsInTheRunFile(t *testing.T) {
	rf := RunFile("agent", []eval.Run{{ScenarioID: "s-1", SubjectError: "it panicked"}})
	var buf bytes.Buffer
	if err := WriteRunFile(&buf, rf); err != nil {
		t.Fatalf("WriteRunFile: %v", err)
	}
	if !strings.Contains(buf.String(), "it panicked") {
		t.Errorf("run file = %s, want the subject's failure in it", buf.String())
	}
}

// An artifact that cannot be read back is not an artifact, and finding that
// out at scoring time is finding out after the cluster has been torn down.
func TestWriteRunFileRefusesWhatCannotBeScored(t *testing.T) {
	cases := []struct {
		name string
		rf   eval.RunFile
		want string
	}{
		{"no subject", eval.RunFile{Runs: []eval.RunRecord{{ScenarioID: "s-1"}}}, "names no subject"},
		{"a record with no scenario", eval.RunFile{Subject: "a", Runs: []eval.RunRecord{{}}}, "no scenario id"},
		{
			"two records for one scenario",
			eval.RunFile{Subject: "a", Runs: []eval.RunRecord{{ScenarioID: "s-1"}, {ScenarioID: "s-1"}}},
			"two runs for scenario",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := WriteRunFile(&bytes.Buffer{}, tc.rf)
			if err == nil {
				t.Fatalf("WriteRunFile accepted %+v", tc.rf)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestWriteRunFileToLandsUnderTheFixedName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "run")
	path, err := WriteRunFileTo(dir, RunFile("agent", []eval.Run{{ScenarioID: "s-1"}}))
	if err != nil {
		t.Fatalf("WriteRunFileTo: %v", err)
	}
	if got := filepath.Base(path); got != RunFileName {
		t.Errorf("wrote %q, want %q", got, RunFileName)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	back, err := eval.ReadRunFile(f)
	if err != nil {
		t.Fatalf("the file the harness wrote does not read back: %v", err)
	}
	if back.Subject != "agent" || len(back.Runs) != 1 {
		t.Errorf("read back %+v, want one run for agent", back)
	}
}

// A refused run file must not leave a partial one behind. The next reader
// would take a truncated artifact for a subject that answered half the pack.
func TestARefusedRunFileLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	if _, err := WriteRunFileTo(dir, eval.RunFile{Runs: []eval.RunRecord{{ScenarioID: "s-1"}}}); err == nil {
		t.Fatal("WriteRunFileTo accepted a run file with no subject")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the directory holds %v after a refused write, want nothing", entries)
	}
}
