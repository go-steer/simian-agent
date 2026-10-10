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
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/go-steer/simian-agent/internal/testutil"
	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/catalog"
	"github.com/go-steer/simian-agent/pkg/lease"
	"github.com/go-steer/simian-agent/pkg/simian"
)

// outageDriver is a fake driver that, like the Chaos Mesh one, picks the
// pods for an outage itself.
type outageDriver struct {
	*testutil.FakeDriver
	check func(ctx context.Context, m simian.FaultManifest) error

	checkedExclusions []string
	appliedExclusions []string
}

func (d *outageDriver) CheckTargets(ctx context.Context, m simian.FaultManifest) error {
	d.checkedExclusions = simian.ExcludedWorkloadsFrom(ctx, m.Targets[0].Namespace)
	if d.check == nil {
		return nil
	}
	return d.check(ctx, m)
}

const outageArena = "shop"

func newOutageExecutor(t *testing.T, exclusions []string) (*Executor, *outageDriver, *testutil.FakeAuditor) {
	t.Helper()
	d := &outageDriver{FakeDriver: &testutil.FakeDriver{EngineName: simian.EngineChaosMesh}}
	d.ApplyFn = func(ctx context.Context, m simian.FaultManifest) (string, error) {
		d.appliedExclusions = simian.ExcludedWorkloadsFrom(ctx, m.Targets[0].Namespace)
		simian.RecordApplyDetail(ctx, "outage", map[string]any{"zone": m.Spec["zone"], "pods_affected": 2})
		// A driver detail never replaces what the executor records.
		simian.RecordApplyDetail(ctx, "engine_uid", "forged")
		return "bundle:shop/b1@chaos-mesh.org/v1alpha1/podchaos", nil
	}
	auditor := &testutil.FakeAuditor{}
	elig := &StaticEligibility{
		Eligible:   map[string]bool{outageArena: true},
		Exclusions: map[string][]string{outageArena: exclusions},
	}
	exec := New(DefaultConfig(), map[simian.Engine]simian.ChaosDriver{simian.EngineChaosMesh: d},
		lease.NewRegistry("test"), auditor, elig,
		// With a workload resolver wired, a namespace-wide selector on a
		// named target is refused — which must not catch an outage.
		WithWorkloadSelectors(KubernetesWorkloadSelectors{Client: fake.NewClientset()}))
	return exec, d, auditor
}

func zoneOutage(spec map[string]any, targets ...simian.TargetRef) simian.FaultManifest {
	if len(targets) == 0 {
		targets = []simian.TargetRef{{Namespace: outageArena}}
	}
	return simian.FaultManifest{
		Source: simian.SourceDirected, Engine: simian.EngineChaosMesh, APIVersion: "chaos-mesh.org/v1alpha1",
		ResourceKind: catalog.ChaosMeshZoneOutage, Spec: spec, Targets: targets, Duration: time.Minute,
	}
}

// An outage carries no selector. In an arena that excludes workloads the
// namespace-wide-selector refusal (#197) would refuse every one, and pod
// narrowing would write a selector into a spec that takes none; neither may
// happen. The exclusions go to the driver instead, which skips them.
func TestAZoneOutageInAnArenaWithExclusionsReachesTheDriverUnchanged(t *testing.T) {
	exec, d, auditor := newOutageExecutor(t, []string{"loadgen"})

	if _, err := exec.Apply(context.Background(), zoneOutage(map[string]any{"zone": "zone-a"})); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	applied := d.AppliedCopy()
	if len(applied) != 1 {
		t.Fatalf("driver applied %d, want 1", len(applied))
	}
	if !reflect.DeepEqual(applied[0].Spec, map[string]any{"zone": "zone-a"}) {
		t.Errorf("spec reached the driver as %v; nothing may be added to an outage spec", applied[0].Spec)
	}
	if !slices.Equal(d.checkedExclusions, []string{"loadgen"}) || !slices.Equal(d.appliedExclusions, []string{"loadgen"}) {
		t.Errorf("exclusions on ctx: check %v, apply %v; want [loadgen] for both", d.checkedExclusions, d.appliedExclusions)
	}

	ev, ok := auditor.FindEvent(audit.EventDriverApplied)
	if !ok {
		t.Fatal("no driver.applied")
	}
	if got, _ := ev.Payload["outage"].(map[string]any); got["zone"] != "zone-a" || got["pods_affected"] != 2 {
		t.Errorf("driver.applied outage detail = %v", ev.Payload["outage"])
	}
	if ev.Payload["engine_uid"] != "bundle:shop/b1@chaos-mesh.org/v1alpha1/podchaos" {
		t.Errorf("engine_uid = %v; a driver detail overwrote the executor's", ev.Payload["engine_uid"])
	}
	if ev.Payload["blast_radius_tier"] != string(simian.TierNamespace) {
		t.Errorf("tier = %v, want namespace", ev.Payload["blast_radius_tier"])
	}
}

func TestAnOutageWithNothingToTakeDownIsRefusedBeforeTheDriverRuns(t *testing.T) {
	exec, d, auditor := newOutageExecutor(t, nil)
	d.check = func(context.Context, simian.FaultManifest) error {
		return fmt.Errorf("%w: no workload of arena shop has pods in zone zone-x", simian.ErrTargetIncompatible)
	}
	_, err := exec.Apply(context.Background(), zoneOutage(map[string]any{"zone": "zone-x"}))
	var ee *simian.ExecutorError
	if !errors.As(err, &ee) || ee.Reason != simian.ReasonTargetIncompatible || ee.Stage != simian.StagePrecheck {
		t.Fatalf("Apply = %v, want precheck target-incompatible", err)
	}
	if ee.Message != "no workload of arena shop has pods in zone zone-x" {
		t.Errorf("message = %q", ee.Message)
	}
	if len(d.AppliedCopy()) != 0 {
		t.Error("the driver applied a refused outage")
	}
	if _, ok := auditor.FindEvent(audit.EventExecutorRejected); !ok {
		t.Error("no executor.rejected")
	}
}

// A driver that cannot tell — an API error — does not refuse: Apply fails or
// not on its own.
func TestATargetCheckThatCannotTellLeavesItToApply(t *testing.T) {
	exec, d, _ := newOutageExecutor(t, nil)
	d.check = func(context.Context, simian.FaultManifest) error { return errors.New("list pods: connection refused") }
	if _, err := exec.Apply(context.Background(), zoneOutage(map[string]any{"zone": "zone-a"})); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(d.AppliedCopy()) != 1 {
		t.Error("not applied")
	}
}

func TestOutageManifestsStillPassTheSafetyGates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		m      simian.FaultManifest
		reason simian.RejectionReason
	}{
		{"an ineligible namespace", zoneOutage(map[string]any{"zone": "zone-a"}, simian.TargetRef{Namespace: "kube-system"}), simian.ReasonNamespaceNotEligible},
		{"an excluded workload named", zoneOutage(map[string]any{"zone": "zone-a"}, simian.TargetRef{Namespace: outageArena, Name: "loadgen"}), simian.ReasonWorkloadExcluded},
		{"a selector smuggled in", zoneOutage(map[string]any{"zone": "zone-a", "selector": map[string]any{"namespaces": []any{"kube-system"}}}), simian.ReasonSchemaInvalid},
		{"no zone", zoneOutage(map[string]any{"action": "pod-failure"}), simian.ReasonSchemaInvalid},
		{"another action", zoneOutage(map[string]any{"zone": "zone-a", "action": "pod-kill"}), simian.ReasonSchemaInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec, d, _ := newOutageExecutor(t, []string{"loadgen"})
			_, err := exec.Apply(context.Background(), tc.m)
			var ee *simian.ExecutorError
			if !errors.As(err, &ee) || ee.Reason != tc.reason {
				t.Fatalf("Apply = %v, want %s", err, tc.reason)
			}
			if len(d.AppliedCopy()) != 0 {
				t.Error("applied")
			}
		})
	}
}
