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
	"slices"
	"testing"
	"time"

	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/simian"
)

func mkRF(uid, ns string, at time.Time) RecentFault {
	return RecentFault{
		FaultUID:  uid,
		AppliedAt: at,
		Manifest: simian.FaultManifest{
			UID:     uid,
			Targets: []simian.TargetRef{{Namespace: ns, Name: "x"}},
		},
	}
}

func TestHistory_PushAndList(t *testing.T) {
	h := NewHistory(10)
	now := time.Now().UTC()
	h.Push(mkRF("f-1", "ns-a", now.Add(-2*time.Minute)))
	h.Push(mkRF("f-2", "ns-b", now.Add(-1*time.Minute)))
	h.Push(mkRF("f-3", "ns-a", now))

	all := h.List("", 0)
	if len(all) != 3 {
		t.Fatalf("List all = %d, want 3", len(all))
	}
	// Newest first.
	if all[0].FaultUID != "f-3" || all[2].FaultUID != "f-1" {
		t.Errorf("ordering wrong: %v", []string{all[0].FaultUID, all[1].FaultUID, all[2].FaultUID})
	}

	nsA := h.List("ns-a", 0)
	if len(nsA) != 2 {
		t.Fatalf("List ns-a = %d, want 2", len(nsA))
	}
}

func TestHistory_LimitTruncates(t *testing.T) {
	h := NewHistory(10)
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		h.Push(mkRF(string(rune('a'+i)), "ns", now.Add(time.Duration(i)*time.Second)))
	}
	got := h.List("", 2)
	if len(got) != 2 {
		t.Fatalf("List(_, 2) = %d, want 2", len(got))
	}
}

func TestHistory_CapacityEvictsOldest(t *testing.T) {
	h := NewHistory(3)
	now := time.Now().UTC()
	h.Push(mkRF("f-1", "ns", now.Add(-3*time.Second)))
	h.Push(mkRF("f-2", "ns", now.Add(-2*time.Second)))
	h.Push(mkRF("f-3", "ns", now.Add(-1*time.Second)))
	h.Push(mkRF("f-4", "ns", now)) // evicts f-1

	all := h.List("", 0)
	if len(all) != 3 {
		t.Fatalf("len=%d, want 3", len(all))
	}
	for _, rf := range all {
		if rf.FaultUID == "f-1" {
			t.Errorf("f-1 should have been evicted")
		}
	}

	// UpdateCleared on the evicted UID is a no-op (must not panic).
	h.UpdateCleared("f-1", now, "deadline-reached")
}

func TestHistory_UpdateCleared(t *testing.T) {
	h := NewHistory(5)
	now := time.Now().UTC()
	h.Push(mkRF("f-1", "ns", now))
	h.UpdateCleared("f-1", now.Add(time.Second), "deadline-reached")

	got := h.List("", 0)
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1", len(got))
	}
	if got[0].ClearReason != "deadline-reached" {
		t.Errorf("ClearReason=%q, want deadline-reached", got[0].ClearReason)
	}
	if got[0].ClearedAt.IsZero() {
		t.Errorf("ClearedAt should be set")
	}
}

func TestHistory_EmptyUIDIsNoOp(t *testing.T) {
	h := NewHistory(3)
	h.Push(RecentFault{FaultUID: ""})
	if got := h.List("", 0); len(got) != 0 {
		t.Errorf("empty UID should not be recorded, got %d entries", len(got))
	}
}

func TestHistory_DefaultCapacityWhenZero(t *testing.T) {
	h := NewHistory(0)
	if h.capacity != DefaultHistoryCapacity {
		t.Errorf("capacity=%d, want %d", h.capacity, DefaultHistoryCapacity)
	}
}

// The history is what the planner reads to avoid repeating itself. A fault
// aimed at two namespaces belongs in the history of both: asked about the
// second one, a Targets[0] filter said the namespace had never been touched.
func TestHistory_ListFindsAFaultByItsSecondTargetNamespace(t *testing.T) {
	h := NewHistory(10)
	now := time.Now().UTC()
	h.Push(RecentFault{
		FaultUID:  "f-wide",
		AppliedAt: now,
		Manifest: simian.FaultManifest{
			UID: "f-wide",
			Targets: []simian.TargetRef{
				{Namespace: "ns-a", Name: "frontend"},
				{Namespace: "ns-b", Name: "payments"},
			},
		},
	})

	for _, ns := range []string{"ns-a", "ns-b"} {
		got := h.List(ns, 0)
		if len(got) != 1 {
			t.Errorf("List(%q) = %d, want 1", ns, len(got))
			continue
		}
		if got[0].FaultUID != "f-wide" {
			t.Errorf("List(%q) returned %q, want f-wide", ns, got[0].FaultUID)
		}
	}
	if got := h.List("ns-c", 0); len(got) != 0 {
		t.Errorf("List(\"ns-c\") = %d, want 0", len(got))
	}
}

// #173: what a restart carries over is the refusals that say something about
// the fault, inside the window, oldest first.
func TestRefusalsFromAuditKeepsWhatIsWorthRemembering(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 17, 0, 0, 0, time.UTC)
	row := func(uid, outcome, reason string, minute int) audit.FaultRow {
		return audit.FaultRow{FaultUID: uid, Outcome: outcome, Reason: reason, EndedAt: t0.Add(time.Duration(minute) * time.Minute),
			Engine: "chaos-mesh", Kind: "IOChaos", Targets: []any{map[string]any{"namespace": "boutique", "name": "redis-cart"}}}
	}
	got := RefusalsFromAudit([]audit.FaultRow{
		row("late", audit.OutcomeRefused, "target-incompatible", 30),
		row("early", audit.OutcomeDriverFailed, "driver-failed", 10),
		row("budget", audit.OutcomeRefused, "budget-exceeded", 20),
		row("stale", audit.OutcomeRefused, "tier-not-permitted", -90),
		row("ran", audit.OutcomeExpired, "deadline-reached", 20),
	}, t0.Add(-time.Hour))
	var uids []string
	for _, rf := range got {
		uids = append(uids, rf.FaultUID)
	}
	if want := []string{"early", "late"}; !slices.Equal(uids, want) {
		t.Fatalf("carried over %v, want %v", uids, want)
	}
	if got[1].Reason != simian.ReasonTargetIncompatible || got[1].Manifest.Targets[0].Name != "redis-cart" {
		t.Errorf("late = %+v", got[1])
	}
}

// #178: a restart carries over what the last process ran, so the planner's
// recent faults do not start empty, and shows what is still running as such.
func TestRecentFromAuditRebuildsWhatRan(t *testing.T) {
	t0 := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	now := t0.Add(30 * time.Minute)
	row := func(uid, outcome, reason string, appliedMin, deadlineMin int) audit.FaultRow {
		r := audit.FaultRow{FaultUID: uid, Outcome: outcome, Reason: reason, Engine: "chaos-mesh", Kind: "PodChaos",
			Targets:  []any{map[string]any{"namespace": "bank", "name": "userservice"}},
			Deadline: t0.Add(time.Duration(deadlineMin) * time.Minute).Format(time.RFC3339)}
		if appliedMin >= -1000 {
			r.AppliedAt = t0.Add(time.Duration(appliedMin) * time.Minute)
		}
		if outcome != audit.OutcomeOpen {
			r.EndedAt = t0.Add(time.Duration(deadlineMin) * time.Minute)
		}
		return r
	}
	got := RecentFromAudit([]audit.FaultRow{
		row("running", audit.OutcomeOpen, "", 28, 31),
		row("ran", audit.OutcomeExpired, "deadline-reached", 10, 13),
		row("left-open", audit.OutcomeOpen, "", 20, 23),
		row("backed-out", audit.OutcomeCleared, "injection-failed", 5, 6),
		row("refused", audit.OutcomeRefused, "target-incompatible", -2000, 0),
		row("old", audit.OutcomeExpired, "deadline-reached", -120, -117),
	}, t0.Add(-time.Hour), now)

	byUID := map[string]RecentFault{}
	var order []string
	for _, rf := range got {
		byUID[rf.FaultUID] = rf
		order = append(order, rf.FaultUID)
	}
	if want := []string{"ran", "left-open", "running"}; !slices.Equal(order, want) {
		t.Fatalf("carried over %v, want %v (oldest first)", order, want)
	}
	if rf := byUID["ran"]; rf.ClearReason != "deadline-reached" || !rf.ClearedAt.Equal(t0.Add(13*time.Minute)) || rf.Manifest.Targets[0].Name != "userservice" {
		t.Errorf("ran = %+v", rf)
	}
	if rf := byUID["running"]; !rf.ClearedAt.IsZero() {
		t.Errorf("a fault before its deadline reads as cleared: %+v", rf)
	}
	if rf := byUID["left-open"]; rf.ClearReason != audit.ReasonUntrackedAfterRestart || !rf.ClearedAt.Equal(t0.Add(23*time.Minute)) {
		t.Errorf("left-open = %+v, want ended at its deadline", rf)
	}

	h := NewHistory(10)
	for _, rf := range got {
		h.Push(rf)
	}
	if !h.Has("running") || h.Has("backed-out") {
		t.Error("Has disagrees with what was pushed")
	}
}

// #178: a fault applied and then backed out because it did not take is
// carried over as a refusal; one interrupted by shutdown is not.
func TestRefusalsFromAuditIncludesWhatWasBackedOut(t *testing.T) {
	t0 := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	row := func(uid, reason string) audit.FaultRow {
		return audit.FaultRow{FaultUID: uid, Outcome: audit.OutcomeCleared, Reason: reason, EndedAt: t0, AppliedAt: t0,
			Error: "AllInjected=False", Engine: "chaos-mesh", Kind: "IOChaos",
			Targets: []any{map[string]any{"namespace": "boutique", "name": "redis-cart"}}}
	}
	got := RefusalsFromAudit([]audit.FaultRow{
		row("not-injected", "injection-failed"),
		row("no-effect", "probe-failed"),
		row("interrupted", "interrupted"),
		row("cleared-by-hand", "explicit-clear"),
	}, t0.Add(-time.Hour))
	var uids []string
	for _, rf := range got {
		uids = append(uids, rf.FaultUID)
	}
	slices.Sort(uids)
	if want := []string{"no-effect", "not-injected"}; !slices.Equal(uids, want) {
		t.Fatalf("carried over %v, want %v", uids, want)
	}
	if got[0].Error != "AllInjected=False" {
		t.Errorf("refusal error = %q", got[0].Error)
	}
}
