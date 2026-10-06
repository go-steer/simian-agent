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
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/go-steer/simian-agent/pkg/simian"
)

// Cycle outcomes, as CycleRow.Outcome.
const (
	CycleCompleted  = "completed"  // planned and executed, whatever it applied
	CycleSkipped    = "skipped"    // stopped before or instead of applying; Reason says why
	CycleUnfinished = "unfinished" // the next cycle in the namespace started first (a restart)
	CycleOpen       = "open"       // still running when the records end
)

// CycleRow is everything the audit trail says about one autonomous-mode
// cycle in one namespace: what it decided and why. Faults answers what
// happened to the cluster; this answers what the loop was thinking.
type CycleRow struct {
	Namespace  string        `json:"namespace"`
	StartedAt  time.Time     `json:"started_at,omitzero"`
	EndedAt    time.Time     `json:"ended_at,omitzero"`
	Outcome    string        `json:"outcome"`
	Reason     string        `json:"reason,omitempty"` // a skip's reason: health-gate, budget-full, no-valid-plan, …
	Detail     string        `json:"detail,omitempty"` // what the gate, the LLM or the validator said
	PlanID     string        `json:"plan_id,omitempty"`
	Hypothesis string        `json:"hypothesis,omitempty"`
	Steps      []CycleStep   `json:"steps,omitempty"`
	Applied    []string      `json:"applied,omitempty"` // fault UIDs
	Refused    []StepRefusal `json:"refused,omitempty"`
}

// CycleStep is one step of the plan a cycle generated.
type CycleStep struct {
	Order             int    `json:"order"`
	Kind              string `json:"kind,omitempty"`
	Target            string `json:"target,omitempty"`
	Duration          string `json:"duration,omitempty"`
	Rationale         string `json:"rationale,omitempty"`
	DurationRationale string `json:"duration_rationale,omitempty"`
}

// StepRefusal is a plan step that was not applied, and why.
type StepRefusal struct {
	Order  int    `json:"order,omitempty"`
	Reason string `json:"reason"`
	Error  string `json:"error,omitempty"`
}

// cycleEvents are the records Cycles reads.
var cycleEvents = map[string]bool{
	EventCycleStarted:     true,
	EventCycleSkipped:     true,
	EventCycleCompleted:   true,
	EventHealthGateFailed: true,
	EventLLMUnavailable:   true,
	EventStepSkipped:      true,
	EventPlanGenerated:    true,
}

// IsCycleEvent reports whether Cycles reads records of this event.
func IsCycleEvent(event string) bool { return cycleEvents[event] }

// Cycles folds records into one row per autonomous-mode cycle, oldest first.
// Records that are not cycle events are ignored, so a whole audit file or
// controller log can be passed in.
func Cycles(records []Record) []CycleRow {
	var (
		rows []*CycleRow
		open = map[string]*CycleRow{} // namespace → the cycle in progress
	)
	get := func(ns string, ts time.Time) *CycleRow {
		if c, ok := open[ns]; ok {
			return c
		}
		// The records began mid-cycle: start one from what is here.
		c := &CycleRow{Namespace: ns, StartedAt: ts, Outcome: CycleOpen}
		rows = append(rows, c)
		open[ns] = c
		return c
	}
	closeAs := func(c *CycleRow, outcome string, ts time.Time) {
		c.Outcome, c.EndedAt = outcome, ts
		delete(open, c.Namespace)
	}
	for _, r := range records {
		if !cycleEvents[r.Event] {
			continue
		}
		ns, _ := r.Payload["namespace"].(string)
		if ns == "" {
			continue
		}
		switch r.Event {
		case EventCycleStarted:
			if prev, ok := open[ns]; ok {
				closeAs(prev, CycleUnfinished, r.TS)
			}
			c := &CycleRow{Namespace: ns, StartedAt: r.TS, Outcome: CycleOpen}
			rows = append(rows, c)
			open[ns] = c
		case EventHealthGateFailed, EventLLMUnavailable:
			get(ns, r.TS).Detail = r.Reason
		case EventPlanGenerated:
			c := get(ns, r.TS)
			c.PlanID = r.PlanID
			c.Hypothesis, _ = r.Payload["hypothesis"].(string)
			c.Steps = cycleSteps(r.Payload["steps"])
		case EventStepSkipped:
			c := get(ns, r.TS)
			ref := StepRefusal{Reason: r.Reason}
			if o, ok := r.Payload["order"].(float64); ok {
				ref.Order = int(o)
			}
			ref.Error, _ = r.Payload["error"].(string)
			c.Refused = append(c.Refused, ref)
		case EventCycleSkipped:
			c := get(ns, r.TS)
			c.Reason = r.Reason
			if e, ok := r.Payload["error"].(string); ok && c.Detail == "" {
				c.Detail = e
			}
			closeAs(c, CycleSkipped, r.TS)
		case EventCycleCompleted:
			c := get(ns, r.TS)
			if c.PlanID == "" {
				c.PlanID = r.PlanID
			}
			if uids, ok := r.Payload["applied_uids"].([]any); ok {
				for _, u := range uids {
					if s, ok := u.(string); ok {
						c.Applied = append(c.Applied, s)
					}
				}
			}
			closeAs(c, CycleCompleted, r.TS)
		}
	}
	out := make([]CycleRow, 0, len(rows))
	for _, c := range rows {
		out = append(out, *c)
	}
	return out
}

func cycleSteps(raw any) []CycleStep {
	list, _ := raw.([]any)
	out := make([]CycleStep, 0, len(list))
	for _, item := range list {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		s := CycleStep{}
		if o, ok := m["order"].(float64); ok {
			s.Order = int(o)
		}
		s.Rationale, _ = m["rationale"].(string)
		s.DurationRationale, _ = m["duration_rationale"].(string)
		if f, ok := m["fault"].(map[string]any); ok {
			s.Kind, _ = f["kind"].(string)
			s.Duration, _ = f["duration"].(string)
			if ts, ok := f["targets"].([]any); ok && len(ts) > 0 {
				if t, ok := ts[0].(map[string]any); ok {
					s.Target, _ = t["name"].(string)
				}
			}
		}
		out = append(out, s)
	}
	return out
}

// CycleLog keeps the recent cycle events a controller has emitted, so its
// cycles can be read back while it runs. It is a simian.Auditor — put it in
// the controller's audit.Multi — and safe for concurrent use.
type CycleLog struct {
	mu      sync.Mutex
	records []Record
	max     int
}

// NewCycleLog keeps up to max cycle records; 0 means 5000, a few days of a
// two-arena loop.
func NewCycleLog(max int) *CycleLog {
	if max <= 0 {
		max = 5000
	}
	return &CycleLog{max: max}
}

// Emit implements simian.Auditor. The payload goes through JSON, so a live
// event folds exactly as the same event read back from the audit file does.
func (l *CycleLog) Emit(_ context.Context, e simian.AuditEvent) {
	if !cycleEvents[e.Event] {
		return
	}
	var payload map[string]any
	if b, err := json.Marshal(e.Payload); err == nil {
		_ = json.Unmarshal(b, &payload)
	}
	l.Add(Record{TS: time.Now().UTC(), Event: e.Event, FaultUID: e.FaultUID, PlanID: e.PlanID,
		ScenarioID: e.ScenarioID, Mode: string(e.Mode), Reason: e.Reason, Payload: payload})
}

// Add records r if it is a cycle event, dropping the oldest past the limit.
func (l *CycleLog) Add(r Record) {
	if !cycleEvents[r.Event] {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, r)
	if over := len(l.records) - l.max; over > 0 {
		l.records = append(l.records[:0:0], l.records[over:]...)
	}
}

// Recent returns up to limit of the newest cycles, newest first, optionally
// in one namespace. limit <= 0 returns all of them.
func (l *CycleLog) Recent(namespace string, limit int) []CycleRow {
	l.mu.Lock()
	recs := append([]Record(nil), l.records...)
	l.mu.Unlock()
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].TS.Before(recs[j].TS) })
	rows := Cycles(recs)
	out := make([]CycleRow, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		if namespace != "" && rows[i].Namespace != namespace {
			continue
		}
		out = append(out, rows[i])
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}
