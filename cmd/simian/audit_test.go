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
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/lease"
	"github.com/go-steer/simian-agent/pkg/simian"
)

// A controller log as `kubectl logs` prints it: start-up noise, then one
// fault's life.
const controllerLog = `{"time":"2026-09-28T12:00:00Z","level":"INFO","msg":"simian serve: starting"}
{"time":"2026-09-28T12:00:01Z","level":"INFO","msg":"audit","component":"audit","event":"executor.received","ts":"2026-09-28T12:00:01Z","fault_uid":"f-1","mode":"autonomous","payload":{"kind":"DNSChaos"}}
{"time":"2026-09-28T12:00:02Z","level":"INFO","msg":"audit","component":"audit","event":"driver.applied","ts":"2026-09-28T12:00:02Z","fault_uid":"f-1","payload":{"engine":"chaos-mesh","kind":"DNSChaos","targets":[{"namespace":"bank","name":"userservice"}],"spec":{"action":"error","patterns":["*"]},"deadline":"2026-09-28T12:10:02Z"}}
{"time":"2026-09-28T12:00:04Z","level":"INFO","msg":"audit","component":"audit","event":"fault.injected","ts":"2026-09-28T12:00:04Z","fault_uid":"f-1","payload":{"passed":false}}
{"time":"2026-09-28T12:00:04Z","level":"INFO","msg":"audit","component":"audit","event":"lease.cleared","ts":"2026-09-28T12:00:04Z","fault_uid":"f-1","reason":"injection-failed"}
{"time":"2026-09-28T12:05:00Z","level":"INFO","msg":"audit","component":"audit","event":"executor.rejected","ts":"2026-09-28T12:05:00Z","fault_uid":"f-2","mode":"autonomous","reason":"schema-invalid","payload":{"error":"spec.port: required"}}
`

func runExport(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	cmd := newAuditCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(append([]string{"export"}, args...))
	if err := cmd.Execute(); err != nil {
		t.Fatalf("audit export %v: %v", args, err)
	}
	return out.String()
}

func TestAuditExportTableIsOneRowPerFault(t *testing.T) {
	out := runExport(t, controllerLog)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want a header and two faults:\n%s", len(lines), out)
	}
	for _, want := range []string{"f-1", "DNSChaos", "bank/userservice", "cleared", "injection-failed", "no", `"action":"error"`} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("f-1 row missing %q: %s", want, lines[1])
		}
	}
	for _, want := range []string{"f-2", "refused", "schema-invalid"} {
		if !strings.Contains(lines[2], want) {
			t.Errorf("f-2 row missing %q: %s", want, lines[2])
		}
	}
}

func TestAuditExportJSONCarriesTheWholeSpec(t *testing.T) {
	out := runExport(t, controllerLog, "--format", "json", "--since", "2026-09-28T12:03:00Z")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d rows, want only f-2 after the cutoff:\n%s", len(lines), out)
	}
	var row audit.FaultRow
	if err := json.Unmarshal([]byte(lines[0]), &row); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if row.FaultUID != "f-2" || row.Outcome != audit.OutcomeRefused || row.Error != "spec.port: required" {
		t.Errorf("row = %+v", row)
	}
}

func TestAuditExportReadsFilesInAnyOrder(t *testing.T) {
	dir := t.TempDir()
	lines := strings.SplitAfter(controllerLog, "\n")
	older, newer := filepath.Join(dir, "audit.jsonl.1"), filepath.Join(dir, "audit.jsonl")
	if err := os.WriteFile(older, []byte(strings.Join(lines[:3], "")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newer, []byte(strings.Join(lines[3:], "")), 0o600); err != nil {
		t.Fatal(err)
	}
	out := runExport(t, "", "--format", "json", newer, older)
	if !strings.Contains(out, `"outcome":"cleared"`) {
		t.Errorf("f-1 not folded across generations:\n%s", out)
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	if got, err := parseSince("24h", now); err != nil || !got.Equal(now.Add(-24*time.Hour)) {
		t.Errorf("24h = %v, %v", got, err)
	}
	if got, err := parseSince("2026-09-27T08:00:00Z", now); err != nil || got.Hour() != 8 {
		t.Errorf("RFC 3339 = %v, %v", got, err)
	}
	if _, err := parseSince("yesterday", now); err == nil {
		t.Error("parseSince accepted nonsense")
	}
}

// A restart that finds a fault the last process applied and never ended
// closes it, in the same file, once.
func TestServeClosesFaultsAPreviousProcessLeftOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	fa, _, closeFaults, err := openAuditFile(path, 0, quietLogger())
	if err != nil {
		t.Fatalf("openAuditFile: %v", err)
	}
	closeFaults(context.Background(), fa, nil) // empty file: nothing to close
	fa.Emit(context.Background(), simian.AuditEvent{Event: audit.EventDriverApplied, FaultUID: "f-left",
		Payload: map[string]any{"engine_uid": "chaos-mesh|bank|simian-x", "deadline": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)}})
	_ = fa.Close()

	for range 2 {
		fa, _, closeFaults, err = openAuditFile(path, 0, quietLogger())
		if err != nil {
			t.Fatalf("openAuditFile: %v", err)
		}
		closeFaults(context.Background(), fa, nil)
		_ = fa.Close()
	}

	f, _ := os.Open(path)
	defer func() { _ = f.Close() }()
	recs, _ := audit.ReadRecords(f)
	var closing []audit.Record
	for _, r := range recs {
		if r.Event == audit.EventLeaseExpired {
			closing = append(closing, r)
		}
	}
	if len(closing) != 1 || closing[0].FaultUID != "f-left" || closing[0].Reason != audit.ReasonUntrackedAfterRestart {
		t.Errorf("closing events = %+v, want one for f-left", closing)
	}
}

// #172: a fault the last process left running is adopted rather than only
// closed on the record, so the new process counts it against the budget and
// clears it at its deadline. One whose deadline has passed is closed as before.
func TestServeAdoptsAFaultAPreviousProcessLeftRunning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	fa, _, _, err := openAuditFile(path, 0, quietLogger())
	if err != nil {
		t.Fatalf("openAuditFile: %v", err)
	}
	deadline := time.Now().Add(2 * time.Minute).UTC().Truncate(time.Second)
	for uid, d := range map[string]time.Time{"f-running": deadline, "f-ended": time.Now().Add(-time.Minute).UTC()} {
		fa.Emit(context.Background(), simian.AuditEvent{Event: audit.EventDriverApplied, FaultUID: uid, Mode: simian.SourceAutonomous,
			Payload: map[string]any{
				"engine": "chaos-mesh", "kind": "PodChaos", "duration": "3m0s",
				"targets":    []any{map[string]any{"namespace": "bank", "name": "ledgerwriter"}},
				"spec":       map[string]any{"action": "pod-kill"},
				"engine_uid": "chaos-mesh|bank|simian-" + uid, "deadline": d.Format(time.RFC3339),
			}})
	}
	_ = fa.Close()

	fa, _, takeOver, err := openAuditFile(path, 0, quietLogger())
	if err != nil {
		t.Fatalf("openAuditFile: %v", err)
	}
	registry := lease.NewRegistry("new-holder")
	takeOver(context.Background(), fa, func(r audit.FaultRow) bool {
		af, ok := r.ActiveFault(time.Now().UTC())
		if ok {
			registry.Adopt(af)
		}
		return ok
	})
	_ = fa.Close()

	active := registry.List("bank")
	if len(active) != 1 || active[0].FaultUID != "f-running" {
		t.Fatalf("leases in bank = %+v, want f-running alone", active)
	}
	if af := active[0]; af.EngineUID != "chaos-mesh|bank|simian-f-running" || !af.Deadline.Equal(deadline) ||
		af.Holder != "new-holder" || af.Manifest.ResourceKind != "PodChaos" {
		t.Errorf("adopted lease = %+v", af)
	}

	f, _ := os.Open(path)
	defer func() { _ = f.Close() }()
	recs, _ := audit.ReadRecords(f)
	got := map[string]string{}
	for _, r := range recs {
		if r.Event == audit.EventLeaseAdopted || r.Event == audit.EventLeaseExpired {
			got[r.FaultUID] += r.Event + " "
		}
	}
	if got["f-running"] != audit.EventLeaseAdopted+" " || got["f-ended"] != audit.EventLeaseExpired+" " {
		t.Errorf("events = %v, want f-running adopted and f-ended closed", got)
	}
	// Adopted is not ended: the row stays open until this process clears it.
	for _, r := range audit.Faults(recs) {
		if r.FaultUID == "f-running" && r.Outcome != audit.OutcomeOpen {
			t.Errorf("adopted fault reads as %s", r.Outcome)
		}
	}
}
