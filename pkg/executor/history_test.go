package executor

import (
	"testing"
	"time"

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
