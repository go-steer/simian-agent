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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/go-steer/simian-agent/pkg/eval"
)

// RunFileName and AuditFileName are what a run writes into its output
// directory. Fixed names, because `simian evaluate --audit x/audit.log --run
// x/run.json` should be typeable from memory.
const (
	RunFileName   = "run.json"
	AuditFileName = "audit.log"
)

// RunFile builds the subject's half of the artifacts.
//
// What is deliberately not here: InjectError, Manifested and InjectedAt. The
// Runner knows all three, and writing them would make the run file a place
// where Simian grades its own homework — a harness that both breaks the
// cluster and certifies that it broke it can be wrong in one direction and
// self-consistent about it. Those three come from the audit log, which is
// written by the executor and the probes as they go, and the scorer takes
// them from there.
func RunFile(subject string, runs []eval.Run) eval.RunFile {
	out := eval.RunFile{Subject: subject, Runs: make([]eval.RunRecord, 0, len(runs))}
	for _, r := range runs {
		out.Runs = append(out.Runs, eval.RunRecord{
			ScenarioID:   r.ScenarioID,
			Report:       r.Report,
			SubjectError: r.SubjectError,
			DetectedAt:   r.DetectedAt,
			ClearedAt:    r.ClearedAt,
		})
	}
	return out
}

// WriteRunFile writes the subject's half as JSON.
//
// It refuses an unnamed subject and a duplicated scenario for the same reason
// ReadRunFile does: an artifact that cannot be read back is not an artifact,
// and finding that out at scoring time is finding out after the cluster has
// been torn down and the run cannot be repeated.
func WriteRunFile(w io.Writer, rf eval.RunFile) error {
	if rf.Subject == "" {
		return errors.New("harness: run file names no subject")
	}
	seen := map[string]bool{}
	for i, rec := range rf.Runs {
		switch {
		case rec.ScenarioID == "":
			return fmt.Errorf("harness: run %d has no scenario id", i)
		case seen[rec.ScenarioID]:
			return fmt.Errorf("harness: two runs for scenario %q", rec.ScenarioID)
		}
		seen[rec.ScenarioID] = true
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rf); err != nil {
		return fmt.Errorf("harness: writing run file: %w", err)
	}
	return nil
}

// WriteRunFileTo writes the run file into dir under RunFileName and returns
// the path it wrote.
//
// The write is atomic — a temporary file in the same directory, renamed into
// place. A suite that is interrupted while serialising should leave either the
// previous run file or none, never a truncated one that reads as a subject
// which answered half the pack.
func WriteRunFileTo(dir string, rf eval.RunFile) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("harness: output directory: %w", err)
	}
	path := filepath.Join(dir, RunFileName)

	tmp, err := os.CreateTemp(dir, RunFileName+".*")
	if err != nil {
		return "", fmt.Errorf("harness: output directory: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op once the rename has happened

	if err := WriteRunFile(tmp, rf); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("harness: writing run file: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("harness: writing run file: %w", err)
	}
	return path, nil
}
