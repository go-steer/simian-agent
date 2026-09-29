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

package audit

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/simian-agent/pkg/simian"
)

func readFile(t *testing.T, path string) []Record {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	recs, err := ReadRecords(f)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return recs
}

func TestTheFileSinkWritesWhatTheReaderReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	a, err := OpenFile(path, 0, func(err error) { t.Errorf("sink error: %v", err) })
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	ctx := WithScenarioID(context.Background(), "s-1")
	a.Emit(ctx, simian.AuditEvent{Event: EventDriverApplied, FaultUID: "f-1", Mode: simian.SourceAutonomous,
		Payload: map[string]any{"kind": "PodChaos", "spec": map[string]any{"action": "pod-kill"}}})
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopening appends rather than truncating: a restart must not erase
	// the trail it is about to close.
	a, _ = OpenFile(path, 0, nil)
	a.Emit(context.Background(), simian.AuditEvent{Event: EventLeaseExpired, FaultUID: "f-1"})
	_ = a.Close()

	recs := readFile(t, path)
	if len(recs) != 2 {
		t.Fatalf("records = %d, want 2", len(recs))
	}
	r := recs[0]
	if r.Event != EventDriverApplied || r.FaultUID != "f-1" || r.ScenarioID != "s-1" || r.Mode != "autonomous" || r.TS.IsZero() {
		t.Errorf("record = %+v", r)
	}
	if spec, _ := r.Payload["spec"].(map[string]any); spec["action"] != "pod-kill" {
		t.Errorf("payload = %v", r.Payload)
	}
}

func TestTheFileSinkRotatesAndKeepsOneGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	a, err := OpenFile(path, 200, nil)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	for range 10 {
		a.Emit(context.Background(), simian.AuditEvent{Event: EventLeaseHeartbeat, FaultUID: "f-1234567890"})
	}
	_ = a.Close()
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("no previous generation: %v", err)
	}
	if st, _ := os.Stat(path); st.Size() > 200 {
		t.Errorf("current file is %d bytes, past its 200-byte limit", st.Size())
	}
	total := len(readFile(t, path+".1")) + len(readFile(t, path))
	if total == 0 || total > 10 {
		t.Errorf("records across both generations = %d", total)
	}
}

// `kubectl logs` output is the other input: the SLogAuditor's JSON lines
// among everything else the controller logs.
func TestTheReaderTakesTheControllersOwnLog(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("simian serve: starting", slog.String("event", "not-an-audit-event"), slog.String("component", "serve"))
	New(logger).Emit(context.Background(), simian.AuditEvent{Event: EventExecutorRejected, FaultUID: "f-9", Reason: "schema-invalid",
		Payload: map[string]any{"error": "spec.port: required"}})
	buf.WriteString("plain text line\n{\"truncated\": \n")

	recs, err := ReadRecords(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("ReadRecords: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("records = %+v, want only the audit event", recs)
	}
	if recs[0].FaultUID != "f-9" || recs[0].Reason != "schema-invalid" || recs[0].Payload["error"] != "spec.port: required" || recs[0].TS.IsZero() {
		t.Errorf("record = %+v", recs[0])
	}
}
