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

package loop

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-steer/simian-agent/pkg/simian"
	"github.com/go-steer/simian-agent/pkg/sut"
	"github.com/go-steer/simian-agent/pkg/topology"
)

type fakeBaselines struct {
	bl sut.Baseline
	ok bool
}

func (f *fakeBaselines) Baseline(_ string) (sut.Baseline, bool) { return f.bl, f.ok }

type fakeTopology struct {
	snap *topology.TargetTopology
	err  error
}

func (f *fakeTopology) Snapshot(_ context.Context, _ string) (*topology.TargetTopology, error) {
	return f.snap, f.err
}

type fakeActive struct {
	out []simian.ActiveFault
	err error
}

func (f *fakeActive) ListActive(_ context.Context, _ string) ([]simian.ActiveFault, error) {
	return f.out, f.err
}

func goodSnapshot() *topology.TargetTopology {
	return &topology.TargetTopology{
		Namespace: "boutique",
		PodStatus: map[string][]topology.PodSummary{
			"frontend":    {{Ready: true}, {Ready: true}},
			"cartservice": {{Ready: true}},
		},
	}
}

func goodBaseline() sut.Baseline {
	return sut.Baseline{
		Namespace: "boutique",
		SUT:       "online-boutique",
		Workloads: []sut.WorkloadStatus{
			{Kind: "Deployment", Name: "frontend", DesiredReplicas: 2, ReadyReplicas: 2},
			{Kind: "Deployment", Name: "cartservice", DesiredReplicas: 1, ReadyReplicas: 1},
		},
	}
}

func TestBaselineHealthGate_Healthy(t *testing.T) {
	g := &BaselineHealthGate{
		Baselines:    &fakeBaselines{bl: goodBaseline(), ok: true},
		Topology:     &fakeTopology{snap: goodSnapshot()},
		ActiveFaults: &fakeActive{},
	}
	if err := g.Check(context.Background(), "boutique"); err != nil {
		t.Fatalf("expected healthy, got %v", err)
	}
}

func TestBaselineHealthGate_NoBaseline(t *testing.T) {
	g := &BaselineHealthGate{
		Baselines:    &fakeBaselines{ok: false},
		Topology:     &fakeTopology{snap: goodSnapshot()},
		ActiveFaults: &fakeActive{},
	}
	err := g.Check(context.Background(), "boutique")
	if err == nil || !strings.Contains(err.Error(), "no baseline") {
		t.Fatalf("expected no-baseline error, got %v", err)
	}
}

func TestBaselineHealthGate_ActiveFaultPresent(t *testing.T) {
	g := &BaselineHealthGate{
		Baselines:    &fakeBaselines{bl: goodBaseline(), ok: true},
		Topology:     &fakeTopology{snap: goodSnapshot()},
		ActiveFaults: &fakeActive{out: []simian.ActiveFault{{FaultUID: "f-1"}}},
	}
	err := g.Check(context.Background(), "boutique")
	if err == nil || !strings.Contains(err.Error(), "still active") {
		t.Fatalf("expected active-fault error, got %v", err)
	}
}

func TestBaselineHealthGate_WorkloadDegraded(t *testing.T) {
	snap := goodSnapshot()
	snap.PodStatus["frontend"] = []topology.PodSummary{{Ready: true}, {Ready: false}}
	g := &BaselineHealthGate{
		Baselines:    &fakeBaselines{bl: goodBaseline(), ok: true},
		Topology:     &fakeTopology{snap: snap},
		ActiveFaults: &fakeActive{},
	}
	err := g.Check(context.Background(), "boutique")
	if err == nil || !strings.Contains(err.Error(), "frontend") {
		t.Fatalf("expected frontend-degraded error, got %v", err)
	}
}

func TestBaselineHealthGate_TopologyError(t *testing.T) {
	g := &BaselineHealthGate{
		Baselines:    &fakeBaselines{bl: goodBaseline(), ok: true},
		Topology:     &fakeTopology{err: errors.New("apiserver down")},
		ActiveFaults: &fakeActive{},
	}
	err := g.Check(context.Background(), "boutique")
	if err == nil || !strings.Contains(err.Error(), "topology") {
		t.Fatalf("expected topology error, got %v", err)
	}
}

func TestBaselineHealthGate_DependenciesNil(t *testing.T) {
	g := &BaselineHealthGate{}
	if err := g.Check(context.Background(), "x"); err == nil {
		t.Fatal("expected error when deps nil")
	}
}
