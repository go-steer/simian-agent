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
