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

// listingDriver can list its own live faults.
type listingDriver struct {
	fakeDriver
	live       []simian.ActiveFault
	err        error
	namespaces []string
}

func (d *listingDriver) ListLive(_ context.Context, namespaces []string, _ time.Time) ([]simian.ActiveFault, error) {
	d.namespaces = namespaces
	return d.live, d.err
}

// #177: what is still running in the cluster is adopted even when nothing on
// disk remembers it, and what the audit file already adopted is not adopted
// twice.
func TestAdoptLiveTakesOverWhatTheRegistryDoesNotHold(t *testing.T) {
	now := time.Now().UTC()
	live := func(uid, ns string) simian.ActiveFault {
		return simian.ActiveFault{FaultUID: uid, EngineUID: ns + "/simian-" + uid, AppliedAt: now.Add(-time.Minute), Deadline: now.Add(2 * time.Minute),
			Manifest: simian.FaultManifest{UID: uid, Engine: simian.EngineChaosMesh, Targets: []simian.TargetRef{{Namespace: ns}}}}
	}
	r := NewRegistry("new-holder")
	r.Adopt(live("f-from-file", "bank")) // adopted from the audit file first
	cm := &listingDriver{live: []simian.ActiveFault{live("f-from-file", "bank"), live("f-lost", "boutique")}}
	np := &listingDriver{err: errors.New("forbidden")}
	aud := &fakeAuditor{}

	n := AdoptLive(context.Background(), r, map[simian.Engine]simian.ChaosDriver{
		simian.EngineChaosMesh:     cm,
		simian.EngineNetworkPolicy: np,
		simian.EngineKubeState:     &fakeDriver{}, // cannot list: skipped
	}, []string{"bank", "boutique"}, aud, now)

	if n != 1 {
		t.Fatalf("adopted %d, want 1", n)
	}
	if !reflect.DeepEqual(cm.namespaces, []string{"bank", "boutique"}) {
		t.Errorf("listed in %v", cm.namespaces)
	}
	got := r.List("boutique")
	if len(got) != 1 || got[0].FaultUID != "f-lost" || got[0].Holder != "new-holder" {
		t.Errorf("leases in boutique = %+v, want f-lost held here", got)
	}
	if len(got) == 1 && got[0].Manifest.Duration != 3*time.Minute {
		t.Errorf("adopted duration = %s, want the 3m from applied to deadline", got[0].Manifest.Duration)
	}
	var adopted, failed int
	for _, e := range aud.events {
		switch {
		case e.Event == "lease.adopted":
			adopted++
			if e.FaultUID != "f-lost" || e.Payload["found_in"] != "cluster" || e.Reason != "untracked-after-restart" {
				t.Errorf("adoption event = %+v", e)
			}
		case e.Reason == "adopt-live-failed":
			failed++
		}
	}
	if adopted != 1 || failed != 1 {
		t.Errorf("events = %+v, want one adoption and one listing failure", aud.events)
	}
}

func TestAdoptLiveDoesNothingWithoutArenas(t *testing.T) {
	cm := &listingDriver{}
	if n := AdoptLive(context.Background(), NewRegistry("h"), map[simian.Engine]simian.ChaosDriver{simian.EngineChaosMesh: cm}, nil, &fakeAuditor{}, time.Now()); n != 0 || cm.namespaces != nil {
		t.Errorf("adopted %d and listed %v with no arenas", n, cm.namespaces)
	}
}
