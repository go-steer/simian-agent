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

package lease

import (
	"context"
	"errors"
	"reflect"
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

// engineDriver is a fakeDriver-equivalent that lets the test pin a
// specific Engine value. Used to verify the Reaper routes Clear calls
// to the right driver in multi-engine installs.
type engineDriver struct {
	engine  simian.Engine
	cleared []string
}

func (d *engineDriver) Apply(context.Context, simian.FaultManifest) (string, error) { return "", nil }
func (d *engineDriver) Clear(_ context.Context, engineUID string) error {
	d.cleared = append(d.cleared, engineUID)
	return nil
}
func (d *engineDriver) Catalog(context.Context) ([]simian.CatalogEntry, error) { return nil, nil }
func (d *engineDriver) Engine() simian.Engine                                  { return d.engine }

func newManifestWithEngine(uid, ns, name string, engine simian.Engine) simian.FaultManifest {
	m := newManifest(uid, ns, name)
	m.Engine = engine
	return m
}

func TestReaperRoutesClearByEngine(t *testing.T) {
	r := NewRegistry("holder-1")
	past := time.Now().Add(-time.Minute)
	r.Register("f-cm", "cm-engine-uid", newManifestWithEngine("f-cm", "ns-a", "wf", simian.EngineChaosMesh), past)
	r.Register("f-np", "np-engine-uid", newManifestWithEngine("f-np", "ns-a", "wf", simian.EngineNetworkPolicy), past)

	cmDriver := &engineDriver{engine: simian.EngineChaosMesh}
	npDriver := &engineDriver{engine: simian.EngineNetworkPolicy}
	rp := &Reaper{
		Registry: r,
		Drivers: map[simian.Engine]simian.ChaosDriver{
			simian.EngineChaosMesh:     cmDriver,
			simian.EngineNetworkPolicy: npDriver,
		},
		Interval: time.Second,
		Auditor:  &fakeAuditor{},
	}
	rp.Sweep(context.Background())

	if len(cmDriver.cleared) != 1 || cmDriver.cleared[0] != "cm-engine-uid" {
		t.Errorf("chaos-mesh driver should have received its own engineUID; got %v", cmDriver.cleared)
	}
	if len(npDriver.cleared) != 1 || npDriver.cleared[0] != "np-engine-uid" {
		t.Errorf("network-policy driver should have received its own engineUID; got %v", npDriver.cleared)
	}
}

func TestReaperUnknownEngineAuditsButContinues(t *testing.T) {
	r := NewRegistry("holder-1")
	past := time.Now().Add(-time.Minute)
	r.Register("f-mystery", "mystery-uid",
		newManifestWithEngine("f-mystery", "ns", "wf", simian.Engine("not-registered")), past)

	auditor := &fakeAuditor{}
	rp := &Reaper{
		Registry: r,
		Drivers:  map[simian.Engine]simian.ChaosDriver{simian.EngineChaosMesh: &fakeDriver{}},
		Interval: time.Second,
		Auditor:  auditor,
	}
	rp.Sweep(context.Background())

	// Should emit a lease.cleared with reason driver-clear-failed and the
	// engine in the payload. Should NOT call Forget — leaving the lease
	// in the registry is right because the failure wasn't a partial
	// clear, just a routing problem; operator inspection can decide.
	found := false
	for _, e := range auditor.events {
		if e.Event == "lease.cleared" && e.Reason == "driver-clear-failed" {
			if eng, _ := e.Payload["engine"].(string); eng == "not-registered" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected lease.cleared driver-clear-failed event with engine=not-registered; got %+v", auditor.events)
	}
	if _, ok := r.Get("f-mystery"); !ok {
		t.Errorf("unrouted fault should remain in registry for operator inspection")
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

// reapingDriver is a driver that can also find its own leaks — the shape the
// network-policy driver has.
type reapingDriver struct {
	fakeDriver
	engine     simian.Engine
	namespaces []string
	calls      int
	cleared    []string
	err        error
}

func (d *reapingDriver) Engine() simian.Engine { return d.engine }

func (d *reapingDriver) ReapExpired(_ context.Context, namespaces []string, _ time.Time) ([]string, error) {
	d.calls++
	d.namespaces = namespaces
	return d.cleared, d.err
}

// The orphan scan is what makes a partition survive a Simian restart at all.
// It has to run on the normal sweep path, not only where it was wired by hand.
func TestSweepAsksOrphanCapableDriversToClearWhatTheRegistryCannotSee(t *testing.T) {
	np := &reapingDriver{engine: simian.EngineNetworkPolicy, cleared: []string{"ns-a/simian-np-01"}}
	cm := &fakeDriver{} // no ReapExpired; must be skipped, not crashed on
	aud := &fakeAuditor{}
	rp := &Reaper{
		Registry: NewRegistry("holder-1"),
		Drivers: map[simian.Engine]simian.ChaosDriver{
			simian.EngineChaosMesh:     cm,
			simian.EngineNetworkPolicy: np,
		},
		Interval:   time.Second,
		Auditor:    aud,
		Namespaces: []string{"ns-a", "ns-b"},
	}

	rp.Sweep(context.Background())

	if np.calls != 1 {
		t.Fatalf("ReapExpired called %d times, want 1", np.calls)
	}
	if got, want := np.namespaces, []string{"ns-a", "ns-b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("swept namespaces = %v, want %v", got, want)
	}
	var found bool
	for _, e := range aud.events {
		if e.Reason == "orphan-reaped" && e.Payload["engine_uid"] == "ns-a/simian-np-01" {
			found = true
		}
	}
	if !found {
		t.Errorf("a reaped orphan must be audited; events = %+v", aud.events)
	}
}

// Simian may only touch namespaces an operator declared as arenas. An empty
// list is not "everywhere", it is "nowhere".
func TestSweepSkipsTheOrphanScanWhenNoArenasAreConfigured(t *testing.T) {
	np := &reapingDriver{engine: simian.EngineNetworkPolicy}
	rp := &Reaper{
		Registry: NewRegistry("holder-1"),
		Drivers:  map[simian.Engine]simian.ChaosDriver{simian.EngineNetworkPolicy: np},
		Interval: time.Second,
		Auditor:  &fakeAuditor{},
		// Namespaces intentionally empty
	}
	rp.Sweep(context.Background())
	if np.calls != 0 {
		t.Errorf("orphan scan ran with no arenas configured (%d calls)", np.calls)
	}
}

// A driver that cannot list one namespace must not silence the sweep: the
// operator needs to know the leak check is not running.
func TestSweepAuditsAnOrphanScanFailure(t *testing.T) {
	np := &reapingDriver{engine: simian.EngineNetworkPolicy, err: errors.New("forbidden")}
	aud := &fakeAuditor{}
	rp := &Reaper{
		Registry:   NewRegistry("holder-1"),
		Drivers:    map[simian.Engine]simian.ChaosDriver{simian.EngineNetworkPolicy: np},
		Interval:   time.Second,
		Auditor:    aud,
		Namespaces: []string{"ns-a"},
	}
	rp.Sweep(context.Background())

	for _, e := range aud.events {
		if e.Reason == "orphan-reap-failed" {
			return
		}
	}
	t.Errorf("a failed orphan scan must be audited; events = %+v", aud.events)
}

// The startup scan is the whole point: the registry is empty after a restart,
// so a leaked partition is only ever found this way.
func TestSweepOrphansRunsWithoutTouchingTheRegistry(t *testing.T) {
	r := NewRegistry("holder-1")
	r.Register("f-live", "e1", newManifest("f-live", "ns-a", "x"), time.Now().Add(-time.Minute))
	np := &reapingDriver{engine: simian.EngineNetworkPolicy}
	rp := &Reaper{
		Registry:   r,
		Drivers:    map[simian.Engine]simian.ChaosDriver{simian.EngineNetworkPolicy: np},
		Interval:   time.Second,
		Auditor:    &fakeAuditor{},
		Namespaces: []string{"ns-a"},
	}

	rp.SweepOrphans(context.Background())

	if np.calls != 1 {
		t.Errorf("SweepOrphans made %d ReapExpired calls, want 1", np.calls)
	}
	if _, ok := r.Get("f-live"); !ok {
		t.Error("SweepOrphans must not clear registry-tracked faults")
	}
}

// Single-driver installs use the Driver convenience field; the scan has to
// reach it too.
func TestSweepOrphansHonoursTheSingleDriverField(t *testing.T) {
	np := &reapingDriver{engine: simian.EngineNetworkPolicy}
	rp := &Reaper{
		Registry:   NewRegistry("holder-1"),
		Driver:     np,
		Interval:   time.Second,
		Auditor:    &fakeAuditor{},
		Namespaces: []string{"ns-a"},
	}
	rp.SweepOrphans(context.Background())
	if np.calls != 1 {
		t.Errorf("ReapExpired called %d times via Driver, want 1", np.calls)
	}
}
