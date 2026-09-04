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

package audit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/simian"
)

type recorder struct{ events []simian.AuditEvent }

func (r *recorder) Emit(_ context.Context, ev simian.AuditEvent) {
	r.events = append(r.events, ev)
}

func TestWithScenarioIDRoundTrips(t *testing.T) {
	ctx := audit.WithScenarioID(context.Background(), "s-1")
	if got := audit.ScenarioIDFrom(ctx); got != "s-1" {
		t.Errorf("ScenarioIDFrom = %q, want %q", got, "s-1")
	}
}

func TestScenarioIDFromAnUnstampedContextIsEmpty(t *testing.T) {
	if got := audit.ScenarioIDFrom(context.Background()); got != "" {
		t.Errorf("ScenarioIDFrom = %q, want empty", got)
	}
}

// An empty ID must not install a value. Otherwise a caller that passes "" for
// a non-scenario run would shadow an outer scenario ID with a blank.
func TestWithScenarioIDIgnoresAnEmptyID(t *testing.T) {
	ctx := audit.WithScenarioID(context.Background(), "s-1")
	ctx = audit.WithScenarioID(ctx, "")
	if got := audit.ScenarioIDFrom(ctx); got != "s-1" {
		t.Errorf("an empty ID shadowed the outer one: got %q", got)
	}
}

func TestScenarioStamperStampsFromContext(t *testing.T) {
	rec := &recorder{}
	st := audit.NewScenarioStamper(rec)
	st.Emit(audit.WithScenarioID(context.Background(), "s-42"), simian.AuditEvent{Event: "x"})

	if len(rec.events) != 1 {
		t.Fatalf("got %d events, want 1", len(rec.events))
	}
	if rec.events[0].ScenarioID != "s-42" {
		t.Errorf("ScenarioID = %q, want %q", rec.events[0].ScenarioID, "s-42")
	}
}

// The lease reaper outlives the context that applied the fault, so it stamps
// the ID onto the event itself. An ambient ID must not overwrite that.
func TestScenarioStamperDoesNotOverwriteAnExplicitID(t *testing.T) {
	rec := &recorder{}
	st := audit.NewScenarioStamper(rec)
	ctx := audit.WithScenarioID(context.Background(), "ambient")
	st.Emit(ctx, simian.AuditEvent{Event: "x", ScenarioID: "explicit"})

	if got := rec.events[0].ScenarioID; got != "explicit" {
		t.Errorf("ScenarioID = %q, want %q", got, "explicit")
	}
}

func TestScenarioStamperLeavesUnstampedEventsAlone(t *testing.T) {
	rec := &recorder{}
	audit.NewScenarioStamper(rec).Emit(context.Background(), simian.AuditEvent{Event: "x"})
	if got := rec.events[0].ScenarioID; got != "" {
		t.Errorf("ScenarioID = %q, want empty outside a scenario", got)
	}
}

func TestScenarioStamperToleratesANilSink(t *testing.T) {
	audit.NewScenarioStamper(nil).Emit(context.Background(), simian.AuditEvent{Event: "x"})
	var nilStamper *audit.ScenarioStamper
	nilStamper.Emit(context.Background(), simian.AuditEvent{Event: "x"})
}

// The default sink stamps on its own, so the ~30 Emit call sites across the
// executor, the loop and the lease reaper need no changes and no discipline.
func TestSLogAuditorStampsTheScenarioIDFromContext(t *testing.T) {
	var buf bytes.Buffer
	a := audit.New(slog.New(slog.NewJSONHandler(&buf, nil)))
	a.Emit(audit.WithScenarioID(context.Background(), "s-7"), simian.AuditEvent{Event: "fault.applied"})

	var got map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &got); err != nil {
		t.Fatalf("parse log line: %v (%q)", err, buf.String())
	}
	if got["scenario_id"] != "s-7" {
		t.Errorf("scenario_id = %v, want s-7; line was %s", got["scenario_id"], buf.String())
	}
}

func TestSLogAuditorOmitsTheScenarioIDOutsideAScenario(t *testing.T) {
	var buf bytes.Buffer
	a := audit.New(slog.New(slog.NewJSONHandler(&buf, nil)))
	a.Emit(context.Background(), simian.AuditEvent{Event: "fault.applied"})

	if strings.Contains(buf.String(), "scenario_id") {
		t.Errorf("scenario_id leaked into a non-scenario line: %s", buf.String())
	}
}
