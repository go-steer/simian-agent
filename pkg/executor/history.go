package executor

import (
	"sort"
	"sync"
	"time"

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

// History is a bounded, in-memory record of recently-applied faults. Safe
// for concurrent use. Lost on process restart — that's intentional for v1
// (R-FAULT-05's durable history is the SimianLease CR design, deferred).
type History struct {
	mu       sync.RWMutex
	capacity int
	items    []RecentFault  // ring buffer; head is items[0] when len < cap
	byUID    map[string]int // index into items by FaultUID for UpdateCleared
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
	}
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
		if namespace != "" {
			if len(rf.Manifest.Targets) == 0 || rf.Manifest.Targets[0].Namespace != namespace {
				continue
			}
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
