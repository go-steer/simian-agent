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
	"slices"
	"testing"

	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/probe"
	"github.com/go-steer/simian-agent/pkg/simian"
)

// sotProbe is an SOT-mode ProbeSpec with the given name.
func sotProbe(name string) simian.ProbeSpec {
	return simian.ProbeSpec{Name: name, Type: simian.ProbeTypeHTTP, Mode: simian.ProbeModeSOT}
}

func TestApplyRunsSOTProbesBeforeTouchingTheCluster(t *testing.T) {
	prober := &fakeProber{}
	exec, driver, auditor, _ := newProbedExecutor(t, prober)

	m := goodManifest()
	m.Probes = []simian.ProbeSpec{sotProbe("reachable-before")}
	if _, err := exec.Apply(context.Background(), m); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if prober.callCount() != 1 {
		t.Fatalf("prober calls=%d, want 1 — the precheck did not run", prober.callCount())
	}
	if got := len(driver.AppliedCopy()); got != 1 {
		t.Fatalf("driver.Applied=%d, want 1", got)
	}

	// Ordering is the whole point: a precheck emitted after driver.applied
	// would be describing a cluster the fault had already changed.
	names := auditor.EventNames()
	pre := slices.Index(names, audit.EventFaultPrecheck)
	applied := slices.Index(names, audit.EventDriverApplied)
	if pre == -1 {
		t.Fatalf("no %s event; got %v", audit.EventFaultPrecheck, names)
	}
	if pre > applied {
		t.Errorf("events = %v, want %s before %s", names, audit.EventFaultPrecheck, audit.EventDriverApplied)
	}
}

func TestAFailedPrecheckRejectsTheFaultWithoutApplyingIt(t *testing.T) {
	prober := &fakeProber{results: []probe.Result{
		{Passed: false, Observed: "web-1: unreachable", Expected: "connection to succeed", Attempts: 15},
	}}
	exec, driver, auditor, registry := newProbedExecutor(t, prober)

	m := goodManifest()
	m.Probes = []simian.ProbeSpec{sotProbe("reachable-before")}
	uid, err := exec.Apply(context.Background(), m)
	if err == nil {
		t.Fatal("Apply succeeded against a cluster that failed its own precheck")
	}
	if uid != "" {
		t.Errorf("uid=%q, want empty on rejection", uid)
	}

	ee := asExecutorError(t, err)
	if ee.Stage != simian.StagePrecheck {
		t.Errorf("stage=%q, want %q", ee.Stage, simian.StagePrecheck)
	}
	if ee.Reason != simian.ReasonPrecheckFailed {
		t.Errorf("reason=%q, want %q", ee.Reason, simian.ReasonPrecheckFailed)
	}

	// Nothing was applied, so there is nothing to roll back and no lease.
	if got := len(driver.AppliedCopy()); got != 0 {
		t.Errorf("driver.Applied=%d, want 0 — the fault reached the cluster anyway", got)
	}
	if got := len(driver.Cleared); got != 0 {
		t.Errorf("driver.Cleared=%d, want 0 — nothing should need clearing", got)
	}
	if got := len(registry.List("")); got != 0 {
		t.Errorf("registry holds %d leases, want 0", got)
	}
	if got := len(exec.Recent("", 10)); got != 0 {
		t.Errorf("history entries=%d, want 0 — a rejected fault is not a recent fault", got)
	}

	ev, ok := auditor.FindEvent(audit.EventFaultPrecheck)
	if !ok {
		t.Fatal("no fault.precheck event emitted")
	}
	if ev.Payload["passed"] != false {
		t.Errorf("payload passed=%v, want false", ev.Payload["passed"])
	}
	if ev.Payload["mode"] != simian.ProbeModeSOT {
		t.Errorf("payload mode=%v, want SOT", ev.Payload["mode"])
	}
	if ev.Reason != string(simian.ReasonPrecheckFailed) {
		t.Errorf("event reason=%q, want %q", ev.Reason, simian.ReasonPrecheckFailed)
	}
	if _, ok := auditor.FindEvent(audit.EventExecutorRejected); !ok {
		t.Error("no executor.rejected event for a precheck failure")
	}
}

func TestPrecheckAndSettleAreReportedSeparately(t *testing.T) {
	// Both gates fail the same way from the prober's point of view. They must
	// not read the same way afterwards: one means the experiment never started,
	// the other means it started and did nothing.
	prober := &fakeProber{results: []probe.Result{
		{Passed: true},
		{Passed: false, Observed: "still answering"},
	}}
	exec, driver, auditor, _ := newProbedExecutor(t, prober)

	m := goodManifest()
	m.Probes = []simian.ProbeSpec{sotProbe("reachable-before"), settleProbe("partitioned")}
	_, err := exec.Apply(context.Background(), m)
	if err == nil {
		t.Fatal("Apply succeeded with a failing Settle probe")
	}
	ee := asExecutorError(t, err)
	if ee.Stage != simian.StageProbe {
		t.Errorf("stage=%q, want %q — the precheck passed, the settle did not", ee.Stage, simian.StageProbe)
	}
	if got := len(driver.AppliedCopy()); got != 1 {
		t.Fatalf("driver.Applied=%d, want 1 — the precheck passed so the fault should have been applied", got)
	}
	if got := len(driver.Cleared); got != 1 {
		t.Errorf("driver.Cleared=%d, want 1 — an unverified fault must be backed out", got)
	}

	pre, ok := auditor.FindEvent(audit.EventFaultPrecheck)
	if !ok {
		t.Fatal("no fault.precheck event")
	}
	if pre.Payload["passed"] != true {
		t.Errorf("precheck passed=%v, want true", pre.Payload["passed"])
	}
	eff, ok := auditor.FindEvent(audit.EventFaultEfficacy)
	if !ok {
		t.Fatal("no fault.efficacy event")
	}
	if eff.Payload["passed"] != false {
		t.Errorf("efficacy passed=%v, want false", eff.Payload["passed"])
	}
	if eff.Payload["mode"] != simian.ProbeModeSettle {
		t.Errorf("efficacy mode=%v, want Settle", eff.Payload["mode"])
	}
}

func TestApplyRefusesSOTProbesWhenNoProberIsWired(t *testing.T) {
	exec, driver, _, _ := newProbedExecutor(t, nil)

	m := goodManifest()
	m.Probes = []simian.ProbeSpec{sotProbe("reachable-before")}
	_, err := exec.Apply(context.Background(), m)
	if err == nil {
		t.Fatal("Apply succeeded with an SOT probe and no prober — the gate was silently skipped")
	}
	ee := asExecutorError(t, err)
	if ee.Reason != simian.ReasonProbeNotConfigured {
		t.Errorf("reason=%q, want %q", ee.Reason, simian.ReasonProbeNotConfigured)
	}
	if ee.Stage != simian.StagePrecheck {
		t.Errorf("stage=%q, want %q", ee.Stage, simian.StagePrecheck)
	}
	if got := len(driver.AppliedCopy()); got != 0 {
		t.Errorf("driver.Applied=%d, want 0", got)
	}
}

func TestSOTProbesGetTheFaultsNamespaceAndLabels(t *testing.T) {
	prober := &fakeProber{}
	exec, _, _, _ := newProbedExecutor(t, prober)

	m := goodManifest()
	m.Targets = []simian.TargetRef{{
		Namespace: "online-boutique",
		Labels:    map[string]string{"app": "paymentservice"},
	}}
	m.Probes = []simian.ProbeSpec{sotProbe("reachable-before")}
	if _, err := exec.Apply(context.Background(), m); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := prober.seen[0]
	if got.Namespace != "online-boutique" {
		t.Errorf("namespace=%q, want the fault's own", got.Namespace)
	}
	if got.Labels["app"] != "paymentservice" {
		t.Errorf("labels=%v, want the fault's own", got.Labels)
	}
}

// --- default probes ---

func TestDefaultProbesAreAttachedAndRun(t *testing.T) {
	prober := &fakeProber{}
	exec, driver, auditor, _ := newProbedExecutor(t, prober,
		WithDefaultProbes(func(simian.FaultManifest) []simian.ProbeSpec {
			return []simian.ProbeSpec{sotProbe("simian-reachable-before"), settleProbe("simian-partitioned")}
		}))

	m := goodManifest()
	if _, err := exec.Apply(context.Background(), m); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if prober.callCount() != 2 {
		t.Fatalf("prober calls=%d, want 2 — the manifest declared no probes and should have been given the default gate", prober.callCount())
	}
	// The driver must see the manifest the gate describes, probes included.
	applied := driver.AppliedCopy()[0]
	if len(applied.Probes) != 2 {
		t.Errorf("driver saw %d probes, want the attached defaults", len(applied.Probes))
	}

	ev, ok := auditor.FindEvent(audit.EventExecutorValidated)
	if !ok {
		t.Fatal("no executor.validated event")
	}
	attached, _ := ev.Payload["default_probes"].([]string)
	if len(attached) != 2 {
		t.Errorf("payload default_probes=%v, want both names recorded", ev.Payload["default_probes"])
	}
}

func TestAFaultWithNoDefaultSourceIsUnchanged(t *testing.T) {
	prober := &fakeProber{}
	exec, driver, auditor, _ := newProbedExecutor(t, prober)

	if _, err := exec.Apply(context.Background(), goodManifest()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if prober.callCount() != 0 {
		t.Errorf("prober calls=%d, want 0", prober.callCount())
	}
	if got := len(driver.AppliedCopy()[0].Probes); got != 0 {
		t.Errorf("driver saw %d probes, want none", got)
	}
	if ev, ok := auditor.FindEvent(audit.EventExecutorValidated); ok {
		if _, present := ev.Payload["default_probes"]; present {
			t.Error("validated event claims default probes were attached when none were")
		}
	}
}

func TestDefaultProbesSeeTheNarrowedManifest(t *testing.T) {
	// Narrowing rewrites the spec's selector to stay inside the arena, and
	// classification stamps the tier. A gate built before either would aim at
	// a manifest the driver is never going to be handed.
	var sawNamespaces []any
	var sawTier simian.BlastRadiusTier
	prober := &fakeProber{}
	exec, _, _, _ := newProbedExecutor(t, prober,
		WithDefaultProbes(func(m simian.FaultManifest) []simian.ProbeSpec {
			sawTier = m.BlastRadiusTier
			if sel, ok := m.Spec["selector"].(map[string]any); ok {
				sawNamespaces, _ = sel["namespaces"].([]any)
			}
			return nil
		}))

	m := goodManifest()
	m.Spec["selector"] = map[string]any{"labelSelectors": map[string]any{"app": "paymentservice"}}
	if _, err := exec.Apply(context.Background(), m); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(sawNamespaces) != 1 || sawNamespaces[0] != "online-boutique" {
		t.Errorf("default builder saw spec.selector.namespaces=%v, want the narrowed [online-boutique]", sawNamespaces)
	}
	if sawTier == "" {
		t.Error("default builder saw an unclassified manifest — it ran before the blast-radius tier was assigned")
	}
}
