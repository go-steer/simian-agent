// Package lease tracks active faults and reaps them when their lease expires
// or the holder process dies. M1 ships with an in-memory registry plus a
// label-driven orphan scan on startup; the full SimianLease CR design is
// scheduled for a later milestone.
package lease

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/go-steer/simian-agent/pkg/simian"
)

// ErrNotFound is returned when ListByUID / Forget cannot find the requested fault.
var ErrNotFound = errors.New("lease: fault not found")

// Registry is the in-memory map of leased faults. Safe for concurrent use.
type Registry struct {
	mu     sync.RWMutex
	holder string
	items  map[string]simian.ActiveFault // keyed by simian fault UID
}

// NewRegistry constructs an empty registry. holderID identifies this controller
// instance (typically pod name + UID); it is recorded on every active fault
// so a crash-recovery scan can identify orphans.
func NewRegistry(holderID string) *Registry {
	return &Registry{
		holder: holderID,
		items:  map[string]simian.ActiveFault{},
	}
}

// Register records a newly-applied fault and returns the stored ActiveFault.
func (r *Registry) Register(faultUID, engineUID string, m simian.FaultManifest, deadline time.Time) simian.ActiveFault {
	now := time.Now().UTC()
	af := simian.ActiveFault{
		FaultUID:  faultUID,
		EngineUID: engineUID,
		Manifest:  m,
		AppliedAt: now,
		Deadline:  deadline,
		Holder:    r.holder,
		LastBeat:  now,
	}
	r.mu.Lock()
	r.items[faultUID] = af
	r.mu.Unlock()
	return af
}

// Get returns the ActiveFault for a given simian fault UID.
func (r *Registry) Get(faultUID string) (simian.ActiveFault, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	af, ok := r.items[faultUID]
	return af, ok
}

// Forget removes a fault from the registry. Used by Clear and by the reaper
// after a successful driver.Clear call.
func (r *Registry) Forget(faultUID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.items[faultUID]; !ok {
		return ErrNotFound
	}
	delete(r.items, faultUID)
	return nil
}

// List returns a snapshot of all currently leased faults, optionally filtered
// by namespace. Returned slice is sorted by AppliedAt for stable output.
func (r *Registry) List(namespace string) []simian.ActiveFault {
	r.mu.RLock()
	out := make([]simian.ActiveFault, 0, len(r.items))
	for _, af := range r.items {
		if namespace != "" && (len(af.Manifest.Targets) == 0 || af.Manifest.Targets[0].Namespace != namespace) {
			continue
		}
		out = append(out, af)
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].AppliedAt.Before(out[j].AppliedAt) })
	return out
}

// Heartbeat refreshes the LastBeat timestamp for a fault. Reserved for the
// CR-backed implementation in a later milestone; in-memory mode is a no-op
// when the holder is alive.
func (r *Registry) Heartbeat(faultUID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if af, ok := r.items[faultUID]; ok {
		af.LastBeat = time.Now().UTC()
		r.items[faultUID] = af
	}
}

// Expired returns the slice of faults whose deadline has passed. Used by the
// reaper sweep.
func (r *Registry) Expired(now time.Time) []simian.ActiveFault {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []simian.ActiveFault
	for _, af := range r.items {
		if !af.Deadline.IsZero() && now.After(af.Deadline) {
			out = append(out, af)
		}
	}
	return out
}

// HolderID returns the controller identity recorded on registry entries.
func (r *Registry) HolderID() string { return r.holder }

// Reaper periodically clears expired faults via the driver. Run in its own
// goroutine for the controller lifetime.
type Reaper struct {
	Registry *Registry
	Driver   simian.ChaosDriver
	Interval time.Duration
	Auditor  simian.Auditor
}

// Run blocks until ctx is done, calling Sweep at every tick.
func (rp *Reaper) Run(ctx context.Context) {
	ticker := time.NewTicker(rp.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rp.Sweep(ctx)
		}
	}
}

// Sweep clears any expired faults. Errors per-fault are audited but do not
// abort the sweep.
func (rp *Reaper) Sweep(ctx context.Context) {
	now := time.Now().UTC()
	for _, af := range rp.Registry.Expired(now) {
		if err := rp.Driver.Clear(ctx, af.EngineUID); err != nil {
			rp.Auditor.Emit(ctx, simian.AuditEvent{
				Event:    "lease.cleared",
				FaultUID: af.FaultUID,
				Reason:   "driver-clear-failed",
				Payload:  map[string]any{"error": err.Error()},
			})
			continue
		}
		_ = rp.Registry.Forget(af.FaultUID)
		rp.Auditor.Emit(ctx, simian.AuditEvent{
			Event:    "lease.expired",
			FaultUID: af.FaultUID,
			Reason:   "deadline-reached",
		})
	}
}
