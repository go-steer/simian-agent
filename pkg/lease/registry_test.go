package lease

import (
	"context"
	"testing"
	"time"

	"github.com/go-steer/simian-agent/pkg/simian"
)

func newManifest(uid, ns, name string) simian.FaultManifest {
	return simian.FaultManifest{
		UID:          uid,
		Engine:       simian.EngineChaosMesh,
		APIVersion:   "chaos-mesh.org/v1alpha1",
		ResourceKind: "NetworkChaos",
		Spec:         map[string]any{},
		Targets:      []simian.TargetRef{{Namespace: ns, Name: name}},
		Duration:     30 * time.Second,
	}
}

func TestRegistryRegisterListForget(t *testing.T) {
	r := NewRegistry("holder-1")
	deadline := time.Now().Add(30 * time.Second)
	r.Register("f-1", "engine-1", newManifest("f-1", "ns-a", "paymentservice"), deadline)
	r.Register("f-2", "engine-2", newManifest("f-2", "ns-b", "cartservice"), deadline)

	if got, want := len(r.List("")), 2; got != want {
		t.Fatalf("List(all)=%d, want %d", got, want)
	}
	if got, want := len(r.List("ns-a")), 1; got != want {
		t.Fatalf("List(ns-a)=%d, want %d", got, want)
	}
	if _, ok := r.Get("f-1"); !ok {
		t.Fatal("Get(f-1) missing")
	}
	if err := r.Forget("f-1"); err != nil {
		t.Fatalf("Forget(f-1): %v", err)
	}
	if err := r.Forget("does-not-exist"); err == nil {
		t.Fatal("Forget(unknown) should error")
	}
}

func TestRegistryExpired(t *testing.T) {
	r := NewRegistry("holder-1")
	past := time.Now().Add(-1 * time.Minute)
	future := time.Now().Add(1 * time.Minute)
	r.Register("expired", "e1", newManifest("expired", "ns-a", "wf"), past)
	r.Register("ok", "e2", newManifest("ok", "ns-a", "wf"), future)

	exp := r.Expired(time.Now())
	if got, want := len(exp), 1; got != want {
		t.Fatalf("Expired count=%d, want %d", got, want)
	}
	if exp[0].FaultUID != "expired" {
		t.Fatalf("Expired[0]=%s, want expired", exp[0].FaultUID)
	}
}

// fakeDriver / fakeAuditor are local stubs to avoid an internal/testutil
// import cycle (testutil already depends on pkg/simian, lease/Reaper takes
// the same interfaces).
type fakeDriver struct{ cleared []string }

func (d *fakeDriver) Apply(context.Context, simian.FaultManifest) (string, error) {
	return "", nil
}
func (d *fakeDriver) Clear(_ context.Context, engineUID string) error {
	d.cleared = append(d.cleared, engineUID)
	return nil
}
func (d *fakeDriver) Catalog(context.Context) ([]simian.CatalogEntry, error) { return nil, nil }
func (d *fakeDriver) Engine() simian.Engine                                  { return simian.EngineChaosMesh }

type fakeAuditor struct{ events []simian.AuditEvent }

func (a *fakeAuditor) Emit(_ context.Context, e simian.AuditEvent) { a.events = append(a.events, e) }

func TestReaperOnExpireFiresWithDeadlineReason(t *testing.T) {
	r := NewRegistry("holder-1")
	past := time.Now().Add(-1 * time.Minute)
	r.Register("f-expired", "engine-1", newManifest("f-expired", "ns-a", "wf"), past)

	driver := &fakeDriver{}
	auditor := &fakeAuditor{}
	var seenUID, seenReason string
	rp := &Reaper{
		Registry: r,
		Driver:   driver,
		Interval: time.Second,
		Auditor:  auditor,
		OnExpire: func(af simian.ActiveFault, reason string) {
			seenUID = af.FaultUID
			seenReason = reason
		},
	}
	rp.Sweep(context.Background())

	if seenUID != "f-expired" || seenReason != "deadline-reached" {
		t.Errorf("OnExpire(uid=%q, reason=%q), want (f-expired, deadline-reached)", seenUID, seenReason)
	}
	if len(driver.cleared) != 1 {
		t.Errorf("driver.cleared=%d, want 1", len(driver.cleared))
	}
	if _, ok := r.Get("f-expired"); ok {
		t.Errorf("expected fault forgotten after sweep")
	}
}

func TestReaperOnExpireNilIsSafe(t *testing.T) {
	r := NewRegistry("holder-1")
	r.Register("f-1", "e1", newManifest("f-1", "ns", "x"), time.Now().Add(-time.Second))
	rp := &Reaper{
		Registry: r,
		Driver:   &fakeDriver{},
		Interval: time.Second,
		Auditor:  &fakeAuditor{},
		// OnExpire intentionally nil
	}
	rp.Sweep(context.Background()) // must not panic
}
