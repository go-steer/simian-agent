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
	"bufio"
	"encoding/json"
	"io"
	"sort"
	"time"
)

// ReasonUntrackedAfterRestart closes a fault that a previous controller
// process applied and never closed. See ClosingEvents.
const ReasonUntrackedAfterRestart = "untracked-after-restart"

// ReadRecords reads audit records from r, one JSON object per line. It
// accepts a FileAuditor's file and the controller's own JSON log (`kubectl
// logs`) alike, skipping every line that is not an audit event — other log
// lines, blank lines, a truncated last line — rather than failing on it.
func ReadRecords(r io.Reader) ([]Record, error) {
	var out []Record
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var rec struct {
			Record
			Component string `json:"component"`
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.Event == "" || (rec.Component != "" && rec.Component != "audit") {
			continue
		}
		out = append(out, rec.Record)
	}
	return out, sc.Err()
}

// Fault outcomes, as FaultRow.Outcome.
const (
	OutcomeRefused      = "refused"       // the executor would not run it
	OutcomeDriverFailed = "driver-failed" // the engine would not take it
	OutcomeCleared      = "cleared"       // taken out before its deadline
	OutcomeExpired      = "expired"       // ran to its deadline
	OutcomeOpen         = "open"          // no closing event in the records
)

// FaultRow is everything the audit trail says about one fault: what was
// asked for, what reached the cluster, whether it took, and how it ended.
type FaultRow struct {
	FaultUID   string    `json:"fault_uid"`
	PlanID     string    `json:"plan_id,omitempty"`
	ScenarioID string    `json:"scenario_id,omitempty"`
	Source     string    `json:"source,omitempty"`
	ReceivedAt time.Time `json:"received_at,omitzero"`

	Engine   string `json:"engine,omitempty"`
	Kind     string `json:"kind,omitempty"`
	Targets  []any  `json:"targets,omitempty"`
	Spec     any    `json:"spec,omitempty"`
	Duration string `json:"duration,omitempty"`
	Tier     string `json:"blast_radius_tier,omitempty"`

	AppliedAt time.Time `json:"applied_at,omitzero"`
	EngineUID string    `json:"engine_uid,omitempty"`
	Deadline  string    `json:"deadline,omitempty"`
	// Injected is the engine's own word on whether the fault took; nil when
	// the engine was not asked. Efficacy is the settle probes' verdict.
	Injected *bool `json:"injected,omitempty"`
	Efficacy *bool `json:"efficacy,omitempty"`

	Outcome string    `json:"outcome"`
	Reason  string    `json:"reason,omitempty"`
	EndedAt time.Time `json:"ended_at,omitzero"`
	Error   string    `json:"error,omitempty"`
}

// Faults folds records into one row per fault, in the order the faults were
// first seen. Records without a fault UID are ignored.
func Faults(records []Record) []FaultRow {
	byUID := map[string]*FaultRow{}
	var order []string
	for _, r := range records {
		if r.FaultUID == "" {
			continue
		}
		row, ok := byUID[r.FaultUID]
		if !ok {
			row = &FaultRow{FaultUID: r.FaultUID, Outcome: OutcomeOpen}
			byUID[r.FaultUID] = row
			order = append(order, r.FaultUID)
		}
		if row.PlanID == "" {
			row.PlanID = r.PlanID
		}
		if row.ScenarioID == "" {
			row.ScenarioID = r.ScenarioID
		}
		if row.Source == "" {
			row.Source = r.Mode
		}
		apply(row, r)
	}
	out := make([]FaultRow, 0, len(order))
	for _, uid := range order {
		out = append(out, *byUID[uid])
	}
	return out
}

func apply(row *FaultRow, r Record) {
	str := func(k string) string { s, _ := r.Payload[k].(string); return s }
	switch r.Event {
	case EventExecutorReceived:
		row.ReceivedAt = r.TS
		describe(row, r.Payload)
	case EventDriverApplied:
		row.AppliedAt = r.TS
		// What reached the cluster wins over what was asked for: narrowing
		// may have rewritten the spec in between.
		describe(row, r.Payload)
		row.EngineUID = str("engine_uid")
		row.Deadline = str("deadline")
	case EventFaultInjected:
		if b, ok := r.Payload["passed"].(bool); ok {
			row.Injected = &b
		}
	case EventFaultEfficacy:
		if b, ok := r.Payload["passed"].(bool); ok {
			// Every probe has to pass; one failure is the verdict.
			if row.Efficacy == nil || !b {
				row.Efficacy = &b
			}
		}
	case EventExecutorRejected:
		end(row, r, OutcomeRefused)
	case EventDriverFailed:
		end(row, r, OutcomeDriverFailed)
	case EventLeaseCleared:
		if left, _ := r.Payload["left_to_reaper"].(bool); left || r.Reason == "driver-clear-failed" {
			return // still in the cluster; the reaper will close it
		}
		end(row, r, OutcomeCleared)
	case EventLeaseExpired:
		end(row, r, OutcomeExpired)
		if r.Reason == ReasonUntrackedAfterRestart {
			// Emitted at the next start, which may be hours on. The engine's
			// own duration ended the fault, at its deadline if that passed.
			if d, err := time.Parse(time.RFC3339, row.Deadline); err == nil && d.Before(r.TS) {
				row.EndedAt = d
			}
		}
	}
}

func describe(row *FaultRow, p map[string]any) {
	if s, ok := p["engine"].(string); ok {
		row.Engine = s
	}
	if s, ok := p["kind"].(string); ok {
		row.Kind = s
	}
	if t, ok := p["targets"].([]any); ok {
		row.Targets = t
	}
	if s, ok := p["spec"]; ok {
		row.Spec = s
	}
	if s, ok := p["duration"].(string); ok {
		row.Duration = s
	}
	if s, ok := p["blast_radius_tier"].(string); ok {
		row.Tier = s
	}
}

func end(row *FaultRow, r Record, outcome string) {
	row.Outcome, row.Reason, row.EndedAt = outcome, r.Reason, r.TS
	if s, ok := r.Payload["error"].(string); ok {
		row.Error = s
	}
}

// Since keeps the rows for faults first seen at or after t.
func Since(rows []FaultRow, t time.Time) []FaultRow {
	out := rows[:0:0]
	for _, r := range rows {
		first := r.ReceivedAt
		if first.IsZero() {
			first = r.AppliedAt
		}
		if first.IsZero() || !first.Before(t) {
			out = append(out, r)
		}
	}
	return out
}

// ClosingEvents returns one lease.expired event, reason
// ReasonUntrackedAfterRestart, for each fault in rows that reached the
// cluster and was never closed.
//
// A controller holds its leases in memory. When it restarts, whatever it had
// applied is no longer tracked by anyone: a Chaos Mesh fault runs out its own
// duration, a NetworkPolicy is left to the orphan reaper. Either way the
// trail used to stop at driver.applied, and a reader could not tell a fault
// still running from one that ended hours ago. Emitted once at start-up from
// the retained file, this gives every such fault an end.
func ClosingEvents(rows []FaultRow, now time.Time) []Record {
	var out []Record
	for _, r := range rows {
		if r.Outcome != OutcomeOpen || r.AppliedAt.IsZero() {
			continue
		}
		payload := map[string]any{"engine_uid": r.EngineUID, "deadline": r.Deadline}
		if d, err := time.Parse(time.RFC3339, r.Deadline); err == nil && d.After(now) {
			payload["still_running"] = true
		}
		out = append(out, Record{
			TS: now, Event: EventLeaseExpired, FaultUID: r.FaultUID, PlanID: r.PlanID,
			ScenarioID: r.ScenarioID, Mode: r.Source, Reason: ReasonUntrackedAfterRestart, Payload: payload,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].FaultUID < out[j].FaultUID })
	return out
}
