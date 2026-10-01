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

package executor

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/simian"
)

// DefaultHistoryCapacity is the bounded buffer size used when WithHistory
// is supplied with a nil History — large enough to survive a planner cycle
// or two of churn, small enough that bounded-memory tests are deterministic.
const DefaultHistoryCapacity = 100

// RecentFault is a structured record of a fault the executor handled. The
// autonomous-mode planner reads these via the get_recent_faults MCP tool
// so it can avoid pointless repetition and learn from past attempts.
type RecentFault struct {
	FaultUID    string               `json:"fault_uid"`
	Manifest    simian.FaultManifest `json:"manifest"`
	AppliedAt   time.Time            `json:"applied_at"`
	ClearedAt   time.Time            `json:"cleared_at,omitempty"` // zero = still active
	ClearReason string               `json:"clear_reason,omitempty"`
}

// RefusedFault is a fault the executor would not run, or ran and backed out:
// rejected by validation or safety, refused by its own precheck, failed by the
// driver, or never injected or never verified.
//
// Kept apart from RecentFault because it means the opposite. A recent fault
// happened; a refused one did not, and a planner shown only the first has no
// way to know the second was ever tried. In a 39-hour trial that is how the
// same invalid HTTPChaos was proposed 220 times.
type RefusedFault struct {
	FaultUID  string                 `json:"fault_uid"`
	Manifest  simian.FaultManifest   `json:"manifest"`
	RefusedAt time.Time              `json:"refused_at"`
	Stage     simian.ExecutorStage   `json:"stage,omitempty"`
	Reason    simian.RejectionReason `json:"reason,omitempty"`
	Error     string                 `json:"error"`
}

// RefusalWorthRemembering reports whether a refusal says something about the
// fault, rather than about the moment it arrived. A budget refusal — the
// concurrency cap, a cooldown — is the latter: the same fault submitted a
// minute later may well run, so telling a planner not to repeat it would be
// wrong.
func RefusalWorthRemembering(err error) bool {
	var ee *simian.ExecutorError
	if errors.As(err, &ee) && ee.Reason == simian.ReasonBudgetExceeded {
		return false
	}
	return err != nil
}

// RefusalsFromAudit rebuilds the refusals an audit trail records since the
// given time, oldest first, for a restarted controller to push back into its
// history (#173). Without them a planner is told nothing was refused, and on
// the 2026-10-01 trial it proposed the same incompatible IOChaos again right
// after each restart.
//
// Only refusals before anything reached the cluster are rebuilt: what the
// executor rejected and what the driver would not take. Budget refusals are
// left out, as RefusalWorthRemembering leaves them out.
func RefusalsFromAudit(rows []audit.FaultRow, since time.Time) []RefusedFault {
	var out []RefusedFault
	for _, r := range rows {
		if r.Outcome != audit.OutcomeRefused && r.Outcome != audit.OutcomeDriverFailed {
			continue
		}
		if r.Reason == string(simian.ReasonBudgetExceeded) || r.EndedAt.Before(since) {
			continue
		}
		m, ok := r.Manifest()
		if !ok {
			continue
		}
		out = append(out, RefusedFault{
			FaultUID: r.FaultUID, Manifest: m, RefusedAt: r.EndedAt,
			Reason: simian.RejectionReason(r.Reason), Error: r.Error,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].RefusedAt.Before(out[j].RefusedAt) })
	return out
}

// History is a bounded, in-memory record of recently-applied faults, and a
// second ring of recently-refused ones. Safe for concurrent use. Lost on
// process restart — that's intentional for v1 (R-FAULT-05's durable history
// is the SimianLease CR design, deferred) — except that serve pushes the
// refusals back from its audit file at start-up; see RefusalsFromAudit.
type History struct {
	mu       sync.RWMutex
	capacity int
	items    []RecentFault  // ring buffer; head is items[0] when len < cap
	byUID    map[string]int // index into items by FaultUID for UpdateCleared
	refused  []RefusedFault // ring buffer, same capacity, oldest first
}

// NewHistory constructs a bounded ring with the given capacity. Capacity ≤ 0
// uses DefaultHistoryCapacity.
func NewHistory(capacity int) *History {
	if capacity <= 0 {
		capacity = DefaultHistoryCapacity
	}
	return &History{
		capacity: capacity,
		items:    make([]RecentFault, 0, capacity),
		byUID:    make(map[string]int, capacity),
		refused:  make([]RefusedFault, 0, capacity),
	}
}

// PushRefused records a fault the executor refused or backed out.
func (h *History) PushRefused(rf RefusedFault) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.refused) >= h.capacity {
		h.refused = h.refused[1:]
	}
	h.refused = append(h.refused, rf)
}

// ListRefused returns up to limit most-recent refusals, newest first,
// optionally filtered to one namespace. limit ≤ 0 returns all of them.
func (h *History) ListRefused(namespace string, limit int) []RefusedFault {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]RefusedFault, 0, len(h.refused))
	for i := len(h.refused) - 1; i >= 0; i-- {
		rf := h.refused[i]
		if !rf.Manifest.TargetsNamespace(namespace) {
			continue
		}
		out = append(out, rf)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// Push records a newly-applied fault. ClearedAt is expected to be zero;
// callers update it later via UpdateCleared.
func (h *History) Push(rf RecentFault) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if rf.FaultUID == "" {
		return
	}
	if len(h.items) >= h.capacity {
		// Evict the oldest.
		evicted := h.items[0]
		h.items = h.items[1:]
		delete(h.byUID, evicted.FaultUID)
		// Indices shifted by one.
		for k, v := range h.byUID {
			h.byUID[k] = v - 1
		}
	}
	h.items = append(h.items, rf)
	h.byUID[rf.FaultUID] = len(h.items) - 1
}

// UpdateCleared marks the fault with the given UID as cleared. No-op if
// the fault is not (or no longer) in the buffer.
func (h *History) UpdateCleared(faultUID string, at time.Time, reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	idx, ok := h.byUID[faultUID]
	if !ok {
		return
	}
	h.items[idx].ClearedAt = at
	h.items[idx].ClearReason = reason
}

// List returns up to `limit` most-recent faults, optionally filtered to one
// namespace. Newest first. limit ≤ 0 returns all matching entries.
func (h *History) List(namespace string, limit int) []RecentFault {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]RecentFault, 0, len(h.items))
	for i := len(h.items) - 1; i >= 0; i-- {
		rf := h.items[i]
		// Any target namespace matches, not just the first: a fault aimed at
		// two namespaces belongs in the history of both.
		if !rf.Manifest.TargetsNamespace(namespace) {
			continue
		}
		out = append(out, rf)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	// Stable secondary sort by AppliedAt desc — items are already newest-first
	// from the ring, but if Push happens out-of-order (test fixtures) this
	// keeps the public contract clean.
	sort.SliceStable(out, func(i, j int) bool { return out[i].AppliedAt.After(out[j].AppliedAt) })
	return out
}
