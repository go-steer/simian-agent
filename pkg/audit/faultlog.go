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

// FaultLog keeps the recent fault events a controller has emitted, so its
// faults can be read back while it runs — how each ended, whether it took,
// whether the workload recovered — exactly as simian audit export folds
// them. A simian.Auditor; safe for concurrent use.
type FaultLog struct {
	mu      sync.Mutex
	records []Record
	max     int
}

// NewFaultLog keeps up to max fault records; 0 means 20000, a few days of a
// two-arena loop at a dozen events per fault.
func NewFaultLog(max int) *FaultLog {
	if max <= 0 {
		max = 20000
	}
	return &FaultLog{max: max}
}

// Emit implements simian.Auditor, normalizing the payload through JSON as
// CycleLog does.
func (l *FaultLog) Emit(_ context.Context, e simian.AuditEvent) {
	if e.FaultUID == "" {
		return
	}
	var payload map[string]any
	if b, err := json.Marshal(e.Payload); err == nil {
		_ = json.Unmarshal(b, &payload)
	}
	l.Add(Record{TS: time.Now().UTC(), Event: e.Event, FaultUID: e.FaultUID, PlanID: e.PlanID,
		ScenarioID: e.ScenarioID, Mode: string(e.Mode), Reason: e.Reason, Payload: payload})
}

// Add records r if it is about a fault, dropping the oldest past the limit.
func (l *FaultLog) Add(r Record) {
	if r.FaultUID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, r)
	if over := len(l.records) - l.max; over > 0 {
		l.records = append(l.records[:0:0], l.records[over:]...)
	}
}

// Recent returns up to limit of the newest faults, newest first, optionally
// those aimed at one namespace. limit <= 0 returns all of them.
func (l *FaultLog) Recent(namespace string, limit int) []FaultRow {
	l.mu.Lock()
	recs := append([]Record(nil), l.records...)
	l.mu.Unlock()
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].TS.Before(recs[j].TS) })
	rows := Faults(recs)
	out := make([]FaultRow, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		if namespace != "" && !rowTargets(rows[i], namespace) {
			continue
		}
		out = append(out, rows[i])
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func rowTargets(r FaultRow, namespace string) bool {
	for _, t := range r.Targets {
		if m, ok := t.(map[string]any); ok && m["namespace"] == namespace {
			return true
		}
	}
	return false
}
