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

package catalog

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"k8s.io/client-go/util/jsonpath"

	"github.com/go-steer/simian-agent/pkg/simian"
)

func byName(probes []simian.ProbeSpec) map[string]simian.ProbeSpec {
	out := map[string]simian.ProbeSpec{}
	for _, p := range probes {
		out[p.Name] = p
	}
	return out
}

func names(probes []simian.ProbeSpec) []string {
	out := make([]string, 0, len(probes))
	for _, p := range probes {
		out = append(out, p.Name)
	}
	return out
}

// --- network-policy ---

func TestNetworkPolicyPartitionGetsATwoSidedReachabilityGate(t *testing.T) {
	m := simian.FaultManifest{
		Engine:       simian.EngineNetworkPolicy,
		ResourceKind: "NetworkPolicy",
		Spec: map[string]any{
			"labelSelectors": map[string]any{"app": "frontend"},
		},
	}
	got := byName(DefaultProbes(m))
	if len(got) != 2 {
		t.Fatalf("probes = %v, want the before/after pair", names(DefaultProbes(m)))
	}

	before, ok := got[ProbeReachableBefore]
	if !ok {
		t.Fatal("no SOT probe: an unreachability check with no baseline passes for the wrong reason")
	}
	if before.Mode != simian.ProbeModeSOT {
		t.Errorf("%s mode = %q, want SOT", before.Name, before.Mode)
	}
	if before.Spec["expect_reachable"] != true {
		t.Errorf("%s spec = %v, want a reachability assertion", before.Name, before.Spec)
	}

	after := got[ProbePartitioned]
	if after.Mode != simian.ProbeModeSettle {
		t.Errorf("%s mode = %q, want Settle", after.Name, after.Mode)
	}
	if after.Spec["expect_unreachable"] != true {
		t.Errorf("%s spec = %v, want an unreachability assertion", after.Name, after.Spec)
	}
	for _, p := range []simian.ProbeSpec{before, after} {
		if p.Type != simian.ProbeTypeHTTP {
			t.Errorf("%s type = %q, want http", p.Name, p.Type)
		}
		if p.Spec["label_selector"] != "app=frontend" {
			t.Errorf("%s selector = %v, want the policy's own", p.Name, p.Spec["label_selector"])
		}
	}
}

func TestAnEgressOnlyPartitionGetsNoGate(t *testing.T) {
	// The controller can prove it cannot reach the target. It has no vantage
	// point on the target's own egress, so a probe here would fail against a
	// fault that worked perfectly.
	m := simian.FaultManifest{
		Engine:       simian.EngineNetworkPolicy,
		ResourceKind: "NetworkPolicy",
		Spec: map[string]any{
			"labelSelectors": map[string]any{"app": "frontend"},
			"directions":     []any{"egress"},
		},
	}
	if got := DefaultProbes(m); len(got) != 0 {
		t.Fatalf("probes = %v, want none for an egress-only partition", names(got))
	}
}

func TestAbsentDirectionsMeansBothAndStillGetsAGate(t *testing.T) {
	m := simian.FaultManifest{
		Engine:       simian.EngineNetworkPolicy,
		ResourceKind: "NetworkPolicy",
		Spec:         map[string]any{"labelSelectors": map[string]any{"app": "frontend"}},
	}
	if got := DefaultProbes(m); len(got) != 2 {
		t.Fatalf("probes = %v, want the pair — the driver defaults directions to both", names(got))
	}
}

// --- chaos-mesh NetworkChaos ---

func TestNetworkChaosPartitionGetsTheSameGate(t *testing.T) {
	m := simian.FaultManifest{
		Engine:       simian.EngineChaosMesh,
		ResourceKind: "NetworkChaos",
		Spec: map[string]any{
			"action":   "partition",
			"selector": map[string]any{"labelSelectors": map[string]any{"app": "cart"}},
		},
	}
	got := byName(DefaultProbes(m))
	if len(got) != 2 {
		t.Fatalf("probes = %v, want the before/after pair", names(DefaultProbes(m)))
	}
	if got[ProbePartitioned].Spec["label_selector"] != "app=cart" {
		t.Errorf("selector = %v, want the nested chaos-mesh selector", got[ProbePartitioned].Spec["label_selector"])
	}
}

func TestATargetedNetworkChaosGetsNoGate(t *testing.T) {
	// A partition between two labelled sets does not include the controller,
	// which would see nothing change and reject a fault that worked.
	for _, field := range []string{"target", "externalTargets"} {
		t.Run(field, func(t *testing.T) {
			m := simian.FaultManifest{
				Engine:       simian.EngineChaosMesh,
				ResourceKind: "NetworkChaos",
				Spec: map[string]any{
					"action":   "partition",
					"selector": map[string]any{"labelSelectors": map[string]any{"app": "cart"}},
					field:      map[string]any{"mode": "all"},
				},
			}
			if got := DefaultProbes(m); len(got) != 0 {
				t.Fatalf("probes = %v, want none when the fault names the other side", names(got))
			}
		})
	}
}

func TestNetworkChaosDelayIsGatedByMeasuredLatency(t *testing.T) {
	m := simian.FaultManifest{
		Engine:       simian.EngineChaosMesh,
		ResourceKind: "NetworkChaos",
		Spec: map[string]any{
			"action":   "delay",
			"selector": map[string]any{"labelSelectors": map[string]any{"app": "cart"}},
			"delay":    map[string]any{"latency": "400ms"},
		},
	}
	got := byName(DefaultProbes(m))
	if len(got) != 2 {
		t.Fatalf("probes = %v, want the before/after pair", names(DefaultProbes(m)))
	}
	if got[ProbeFastBefore].Spec["max_latency"] != "100ms" {
		t.Errorf("SOT max_latency = %v, want a quarter of the injected delay", got[ProbeFastBefore].Spec["max_latency"])
	}
	if got[ProbeDelayed].Spec["min_latency"] != "200ms" {
		t.Errorf("Settle min_latency = %v, want half the injected delay", got[ProbeDelayed].Spec["min_latency"])
	}
	// The per-request deadline has to clear the delay, or every poll times out
	// and the gate rejects the fault it was meant to confirm.
	if got[ProbeDelayed].Spec["request_timeout"] != "1.6s" {
		t.Errorf("Settle request_timeout = %v, want room for the injected delay", got[ProbeDelayed].Spec["request_timeout"])
	}
}

func TestATinyDelayIsNotGated(t *testing.T) {
	// 10ms is inside the noise of an in-cluster round trip. A gate there would
	// reject working faults more often than it caught broken ones.
	m := simian.FaultManifest{
		Engine:       simian.EngineChaosMesh,
		ResourceKind: "NetworkChaos",
		Spec: map[string]any{
			"action":   "delay",
			"selector": map[string]any{"labelSelectors": map[string]any{"app": "cart"}},
			"delay":    map[string]any{"latency": "10ms"},
		},
	}
	if got := DefaultProbes(m); len(got) != 0 {
		t.Fatalf("probes = %v, want none below the measurable threshold", names(got))
	}
}

func TestStatisticalNetworkChaosActionsAreNotGated(t *testing.T) {
	for _, action := range []string{"loss", "duplicate", "corrupt", "bandwidth", ""} {
		t.Run(action, func(t *testing.T) {
			m := simian.FaultManifest{
				Engine:       simian.EngineChaosMesh,
				ResourceKind: "NetworkChaos",
				Spec: map[string]any{
					"action":   action,
					"selector": map[string]any{"labelSelectors": map[string]any{"app": "cart"}},
				},
			}
			if got := DefaultProbes(m); len(got) != 0 {
				t.Fatalf("probes = %v, want none: one request cannot separate %q landing from not", names(got), action)
			}
		})
	}
}

// --- envoy-fault ---

func TestEnvoyFaultsAreConfirmedThroughTheAdminAPI(t *testing.T) {
	cases := []struct {
		kind string
		key  string
	}{
		{"EnvoyHttpDelay", "fault.http.delay.fixed_delay_percent"},
		{"EnvoyHttpAbort", "fault.http.abort.abort_percent"},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			m := simian.FaultManifest{
				Engine:       simian.EngineEnvoyFault,
				ResourceKind: tc.kind,
				Spec: map[string]any{
					"percentage":     100,
					"http_status":    503,
					"labelSelectors": map[string]any{"app": "cart"},
				},
			}
			got := DefaultProbes(m)
			if len(got) != 1 {
				t.Fatalf("probes = %v, want one admin-API check", names(got))
			}
			p := got[0]
			// 15000 spelled out, not the constant: the point of the check is
			// that the probe dials the port the sidecar bootstrap actually
			// opens its admin listener on.
			if p.Spec["port"] != 15000 || p.Spec["path"] != "/runtime" {
				t.Errorf("probe aims at :%v%v, want the admin API at :15000/runtime", p.Spec["port"], p.Spec["path"])
			}
			// A 200 from /runtime_modify says the request was accepted. Reading
			// the value back is what says the filter is live.
			jp, _ := p.Spec["jsonpath"].(string)
			if !strings.Contains(jp, strings.ReplaceAll(tc.key, ".", `\.`)) {
				t.Errorf("jsonpath = %q, want the escaped runtime key %q", jp, tc.key)
			}
			if p.Spec["expect_equals"] != "100" {
				t.Errorf("expect_equals = %v, want the requested percentage", p.Spec["expect_equals"])
			}
		})
	}
}

func TestTheEnvoyGateNeedsAPercentageToCheck(t *testing.T) {
	m := simian.FaultManifest{
		Engine:       simian.EngineEnvoyFault,
		ResourceKind: "EnvoyHttpDelay",
		Spec:         map[string]any{"labelSelectors": map[string]any{"app": "cart"}},
	}
	if got := DefaultProbes(m); len(got) != 0 {
		t.Fatalf("probes = %v, want none when the spec names no percentage to verify", names(got))
	}
}

func TestTheEscapedRuntimeKeyIsOneFieldNotFive(t *testing.T) {
	got := escapeJSONPathKey("fault.http.delay.fixed_delay_percent")
	want := `fault\.http\.delay\.fixed_delay_percent`
	if got != want {
		t.Errorf("escapeJSONPathKey = %q, want %q", got, want)
	}
}

// --- merging and coverage ---

func TestAManifestKeepsItsOwnProbeWhenItOverridesOneByName(t *testing.T) {
	mine := simian.ProbeSpec{
		Name: ProbePartitioned,
		Type: simian.ProbeTypeHTTP,
		Mode: simian.ProbeModeSettle,
		Spec: map[string]any{"expect_unreachable": true, "port": 9090},
	}
	m := simian.FaultManifest{
		Engine:       simian.EngineNetworkPolicy,
		ResourceKind: "NetworkPolicy",
		Spec:         map[string]any{"labelSelectors": map[string]any{"app": "frontend"}},
		Probes:       []simian.ProbeSpec{mine},
	}
	got := DefaultProbes(m)
	if len(got) != 1 || got[0].Name != ProbeReachableBefore {
		t.Fatalf("probes = %v, want only the half the manifest did not override", names(got))
	}
}

func TestAnUnrelatedProbeDoesNotDissolveTheGate(t *testing.T) {
	// Otherwise a planner could disable its own verification by declaring one
	// probe that checks nothing in particular.
	m := simian.FaultManifest{
		Engine:       simian.EngineNetworkPolicy,
		ResourceKind: "NetworkPolicy",
		Spec:         map[string]any{"labelSelectors": map[string]any{"app": "frontend"}},
		Probes: []simian.ProbeSpec{{
			Name: "my-own-check", Type: simian.ProbeTypeK8s, Mode: simian.ProbeModeSettle,
		}},
	}
	if got := DefaultProbes(m); len(got) != 2 {
		t.Fatalf("probes = %v, want the full gate alongside the manifest's own probe", names(got))
	}
}

func TestKindsWithNoGateGetNothing(t *testing.T) {
	m := simian.FaultManifest{
		Engine:       simian.EngineChaosMesh,
		ResourceKind: "StressChaos",
		Spec:         map[string]any{"selector": map[string]any{"labelSelectors": map[string]any{"app": "cart"}}},
	}
	if got := DefaultProbes(m); len(got) != 0 {
		t.Fatalf("probes = %v, want none for a kind with no gate", names(got))
	}
	if EfficacyGate(simian.EngineChaosMesh, "StressChaos") != "" {
		t.Error("StressChaos advertises a gate it does not have")
	}
}

func TestEveryGatedKindAdvertisesItsGate(t *testing.T) {
	// The catalog's efficacy_gate is what tells the planner a verified fault
	// kind from an unverified one; a gate with no description is invisible.
	for key, g := range gates {
		if strings.TrimSpace(g.describe) == "" {
			t.Errorf("%s/%s has a gate but no description", key.engine, key.kind)
		}
		if g.build == nil {
			t.Errorf("%s/%s advertises a gate with no builder", key.engine, key.kind)
		}
		if got := EfficacyGate(key.engine, key.kind); got != g.describe {
			t.Errorf("EfficacyGate(%s, %s) = %q, want %q", key.engine, key.kind, got, g.describe)
		}
	}
}

func TestDefaultProbeNamesAreReserved(t *testing.T) {
	// Every probe Simian attaches must be identifiable as Simian's in an audit
	// record, and overridable by name without guessing.
	m := simian.FaultManifest{
		Engine:       simian.EngineNetworkPolicy,
		ResourceKind: "NetworkPolicy",
		Spec:         map[string]any{"labelSelectors": map[string]any{"app": "frontend"}},
	}
	for _, p := range DefaultProbes(m) {
		if !strings.HasPrefix(p.Name, "simian-") {
			t.Errorf("default probe %q is not under the reserved prefix", p.Name)
		}
	}
}

func TestASpecWithNoSelectorFallsBackToTheFaultsTarget(t *testing.T) {
	// The probe inherits Target.Labels in that case; pinning an empty selector
	// would aim the probe at every pod in the namespace.
	m := simian.FaultManifest{
		Engine:       simian.EngineNetworkPolicy,
		ResourceKind: "NetworkPolicy",
		Spec:         map[string]any{},
	}
	for _, p := range DefaultProbes(m) {
		if _, ok := p.Spec["label_selector"]; ok {
			t.Errorf("%s pinned a selector the spec did not have: %v", p.Name, p.Spec["label_selector"])
		}
	}
}

func TestAMultiLabelSelectorRendersInAStableOrder(t *testing.T) {
	// Go map iteration is randomized. An unsorted render would make the probe
	// spec — and so the audit record of what was checked — differ between two
	// runs of the same manifest.
	m := simian.FaultManifest{
		Engine:       simian.EngineNetworkPolicy,
		ResourceKind: "NetworkPolicy",
		Spec: map[string]any{"labelSelectors": map[string]any{
			"tier": "backend", "app": "cart", "version": "v2",
		}},
	}
	const want = "app=cart,tier=backend,version=v2"
	for i := range 20 {
		for _, p := range DefaultProbes(m) {
			if got := p.Spec["label_selector"]; got != want {
				t.Fatalf("run %d: %s label_selector = %v, want %q", i, p.Name, got, want)
			}
		}
	}
}

// --- kube-state ---

func kubeStateManifest(kind string, spec map[string]any) simian.FaultManifest {
	return simian.FaultManifest{
		UID:          "01K4ZQ8XABCDEF",
		Engine:       simian.EngineKubeState,
		ResourceKind: kind,
		Spec:         spec,
		Targets:      []simian.TargetRef{{Namespace: "arena-1"}},
	}
}

func TestEveryKubeStateKindIsGatedOnItsOwnFailureState(t *testing.T) {
	// The reason each kind proves itself on. A gate that fired on something
	// broader — Pending, or any crash loop — would pass for a fault other than
	// the one injected, which is the same lie as not gating at all.
	want := map[string]struct{ probe, reason string }{
		KubeStateImageUnresolvable:  {ProbeImagePullFailed, "ImagePullBackOff"},
		KubeStateContainerExitLoop:  {ProbeCrashLooping, "Error"},
		KubeStateMemoryLimitSqueeze: {ProbeOOMKilled, "OOMKilled"},
		KubeStateUnschedulable:      {ProbeUnschedulable, "Unschedulable"},
	}
	for kind, w := range want {
		probes := DefaultProbes(kubeStateManifest(kind, nil))
		if len(probes) != 1 {
			t.Fatalf("%s: probes = %v, want exactly one", kind, names(probes))
		}
		p := probes[0]
		if p.Name != w.probe {
			t.Errorf("%s: probe name = %q, want %q", kind, p.Name, w.probe)
		}
		if p.Type != simian.ProbeTypeK8s {
			t.Errorf("%s: type = %q, want k8s", kind, p.Type)
		}
		// Settle only. A synthesized workload did not exist before Apply, so
		// there is no pre-existing condition for an SOT half to rule out.
		if p.Mode != simian.ProbeModeSettle {
			t.Errorf("%s: mode = %q, want Settle", kind, p.Mode)
		}
		if got := p.Spec["expect_contains"]; got != w.reason {
			t.Errorf("%s: expect_contains = %v, want %q", kind, got, w.reason)
		}
		if got := p.Spec["resource"]; got != "pods" {
			t.Errorf("%s: resource = %v, want pods", kind, got)
		}
		if got, _ := p.Spec["jsonpath"].(string); !strings.HasPrefix(got, "{.items[*].status.") {
			t.Errorf("%s: jsonpath %q does not read pod status", kind, got)
		}
		// state.waiting.reason is a coin flip for anything that restarts: the
		// kubelet only shows CrashLoopBackOff in a narrow window around each
		// restart decision, and sits in state.terminated the rest of the time.
		// Measured on GKE 1.36 at one poll in six. Only the kind that never
		// starts a container may read the current waiting state.
		if got, _ := p.Spec["jsonpath"].(string); strings.Contains(got, "state.waiting") && kind != KubeStateImageUnresolvable {
			t.Errorf("%s: gate reads %q, which is only intermittently populated for a container that restarts", kind, got)
		}
		if got := p.Spec["timeout"]; got == "" || got == nil {
			t.Errorf("%s: gate has no timeout", kind)
		}
	}
}

// The gate runs before Apply and has to select pods that do not exist yet. It
// can only do that because both sides derive the workload name from the fault
// UID. If they ever disagree the probe selects nothing, the Settle never
// passes, and a fault that landed is rolled back and reported as inert.
func TestTheKubeStateGateSelectsTheWorkloadApplyWillCreate(t *testing.T) {
	m := kubeStateManifest(KubeStateImageUnresolvable, nil)
	want := "app=" + KubeStateWorkloadName(KubeStateImageUnresolvable, "", m.UID)
	if got := DefaultProbes(m)[0].Spec["label_selector"]; got != want {
		t.Errorf("label_selector = %v, want %q", got, want)
	}

	named := kubeStateManifest(KubeStateImageUnresolvable, map[string]any{"name": "checkout"})
	got, _ := DefaultProbes(named)[0].Spec["label_selector"].(string)
	if !strings.HasPrefix(got, "app=checkout-") {
		t.Errorf("label_selector = %q, want the requested name plus the UID suffix", got)
	}
}

// The same manifest must produce the same selector every time, or the audit
// record of what was checked differs between two replays of one scenario.
func TestTheKubeStateWorkloadNameIsAPureFunctionOfTheManifest(t *testing.T) {
	m := kubeStateManifest(KubeStateUnschedulable, nil)
	first, _ := DefaultProbes(m)[0].Spec["label_selector"].(string)
	for i := range 20 {
		if got, _ := DefaultProbes(m)[0].Spec["label_selector"].(string); got != first {
			t.Fatalf("run %d: selector = %q, want %q", i, got, first)
		}
	}
	// Two faults of the same kind must not share a workload — the second
	// Create would collide, and one gate would read the other's pods.
	other := kubeStateManifest(KubeStateUnschedulable, nil)
	other.UID = "01K4ZQ8XZZZZZZ"
	if got, _ := DefaultProbes(other)[0].Spec["label_selector"].(string); got == first {
		t.Errorf("two fault UIDs produced the same selector %q", got)
	}
}

// Only synthesize creates pods. Attaching a gate to a mode that creates none
// turns a clear driver error into a 90-second probe timeout.
func TestOnlySynthesizeModeGetsAKubeStateGate(t *testing.T) {
	if got := DefaultProbes(kubeStateManifest(KubeStateImageUnresolvable, map[string]any{"mode": "synthesize"})); len(got) != 1 {
		t.Errorf("explicit synthesize: probes = %v, want one", names(got))
	}
	for _, mode := range []string{"mutate", "nonsense"} {
		if got := DefaultProbes(kubeStateManifest(KubeStateImageUnresolvable, map[string]any{"mode": mode})); len(got) != 0 {
			t.Errorf("mode %q: probes = %v, want none", mode, names(got))
		}
	}
}

func TestAnUnknownKubeStateKindGetsNoGate(t *testing.T) {
	if got := DefaultProbes(kubeStateManifest("NodeUnready", nil)); len(got) != 0 {
		t.Errorf("probes = %v, want none for a kind this engine does not synthesize", names(got))
	}
}

// The Ready gate is the one gate in this package that uses a jsonpath filter,
// and a filter that does not parse fails at probe time in a cluster rather
// than here. It is also the only thing standing between "the workload is
// serving" and "the workload has been scheduled": without the filter the
// expression renders every condition's status, and PodScheduled=True would
// satisfy expect_contains "True" for a pod that has not pulled its image.
func TestThePodReadyGateRendersOnlyTheReadyCondition(t *testing.T) {
	pod := func(conds ...[2]string) any {
		out := make([]any, 0, len(conds))
		for _, c := range conds {
			out = append(out, map[string]any{"type": c[0], "status": c[1]})
		}
		return map[string]any{"status": map[string]any{"conditions": out}}
	}
	render := func(t *testing.T, items ...any) string {
		t.Helper()
		jp := jsonpath.New("ready")
		jp.AllowMissingKeys(true)
		if err := jp.Parse(podReadyJSONPath); err != nil {
			t.Fatalf("parse %q: %v", podReadyJSONPath, err)
		}
		var buf bytes.Buffer
		if err := jp.Execute(&buf, map[string]any{"items": items}); err != nil {
			t.Fatalf("execute: %v", err)
		}
		return buf.String()
	}

	scheduledNotReady := pod([2]string{"PodScheduled", "True"}, [2]string{"Ready", "False"})
	if got := render(t, scheduledNotReady); strings.Contains(got, "True") {
		t.Errorf("a scheduled-but-not-ready pod renders %q, which satisfies the gate", got)
	}
	ready := pod([2]string{"PodScheduled", "True"}, [2]string{"Ready", "True"})
	if got := render(t, ready); !strings.Contains(got, "True") {
		t.Errorf("a Ready pod renders %q, which does not satisfy the gate", got)
	}
	// A pod the kubelet has not reported on yet has no conditions at all.
	// AllowMissingKeys has to carry that, or the gate errors on its first poll
	// instead of waiting for the workload to come up.
	if got := render(t, map[string]any{"status": map[string]any{}}); strings.Contains(got, "True") {
		t.Errorf("a pod with no conditions renders %q", got)
	}
}

// Some kinds need more than one probe, and the order is load-bearing: an
// expect_empty gate passes against a namespace where nothing has been created
// yet, so it must be preceded by one that proves the objects are there.
func TestAGateWhoseEvidenceIsAnAbsenceIsNeverFirst(t *testing.T) {
	for kind, gs := range kubeStateGates {
		for i, g := range gs {
			if g.expectEmpty && i == 0 {
				t.Errorf("%s: %q asserts an absence and runs first, so it passes before anything exists", kind, g.probeName)
			}
			if g.expectEmpty && g.expect != "" {
				t.Errorf("%s: %q sets both expect and expectEmpty", kind, g.probeName)
			}
			if !g.expectEmpty && g.expect == "" {
				t.Errorf("%s: %q asserts nothing", kind, g.probeName)
			}
			if g.timeout == "" {
				t.Errorf("%s: %q has no timeout", kind, g.probeName)
			}
		}
	}
}

// The multi-probe kinds, spelled out. Each pair is the thing the kind is about
// plus the thing that keeps it from being vacuous.
func TestTheBundleKindsAreGatedOnEveryHalfOfTheirFault(t *testing.T) {
	for _, tc := range []struct {
		kind   string
		probes []string
	}{
		{KubeStateJobFailure, []string{ProbeJobFailed}},
		{KubeStateSelectorDrift, []string{ProbeWorkloadReady, ProbeNoEndpoints}},
		{KubeStateUnboundClaim, []string{ProbeClaimPending, ProbeUnschedulable}},
		{KubeStateNoOp, []string{ProbeWorkloadReady}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			got := DefaultProbes(kubeStateManifest(tc.kind, nil))
			if !slices.Equal(names(got), tc.probes) {
				t.Fatalf("probes = %v, want %v in that order", names(got), tc.probes)
			}
			for _, p := range got {
				if p.Mode != simian.ProbeModeSettle {
					t.Errorf("%s: mode = %q, want Settle", p.Name, p.Mode)
				}
				if p.Type != simian.ProbeTypeK8s {
					t.Errorf("%s: type = %q, want k8s", p.Name, p.Type)
				}
			}
		})
	}
}

// Each probe has to read the resource its evidence lives on, and select it the
// way that resource is actually labelled. EndpointSlices are the trap: they are
// written by the endpointslice controller, which knows nothing about
// simian.chaos labels and stamps only the Service name.
func TestEachBundleGateReadsTheRightResource(t *testing.T) {
	m := kubeStateManifest(KubeStateSelectorDrift, nil)
	name := KubeStateWorkloadName(KubeStateSelectorDrift, "", m.UID)
	probes := DefaultProbes(m)
	if got := probes[0].Spec["resource"]; got != "pods" {
		t.Errorf("readiness probe reads %v, want pods", got)
	}
	if got := probes[0].Spec["label_selector"]; got != "app="+name {
		t.Errorf("readiness selector = %v, want app=%s", got, name)
	}
	if got := probes[1].Spec["resource"]; got != "endpointslices" {
		t.Errorf("endpoint probe reads %v, want endpointslices", got)
	}
	if got := probes[1].Spec["label_selector"]; got != "kubernetes.io/service-name="+name {
		t.Errorf("endpoint selector = %v, want the Service name label", got)
	}
	// The addresses, not the slices. A Service that selects nothing still gets
	// a placeholder EndpointSlice, so asserting no slice exists would fail
	// against a fault that landed perfectly.
	if got, _ := probes[1].Spec["jsonpath"].(string); !strings.Contains(got, "addresses") {
		t.Errorf("endpoint probe jsonpath %q does not read the addresses", got)
	}
	if probes[1].Spec["expect_empty"] != true || probes[1].Spec["expect_contains"] != nil {
		t.Errorf("endpoint probe conditions = %v", probes[1].Spec)
	}

	claim := DefaultProbes(kubeStateManifest(KubeStateUnboundClaim, nil))
	if got := claim[0].Spec["resource"]; got != "persistentvolumeclaims" {
		t.Errorf("claim probe reads %v, want persistentvolumeclaims", got)
	}
	if got := DefaultProbes(kubeStateManifest(KubeStateJobFailure, nil))[0].Spec["resource"]; got != "jobs" {
		t.Errorf("job probe reads %v, want jobs", got)
	}
}

// The control is gated too, and on the opposite of a fault. Without it a
// control would "inject" successfully against a cluster too broken to run
// anything, and the subject's correct report of nothing would be scored as a
// correct answer instead of the vacuous pass it is.
func TestTheControlIsGatedOnItsWorkloadBeingHealthy(t *testing.T) {
	got := DefaultProbes(kubeStateManifest(KubeStateNoOp, nil))
	if len(got) != 1 {
		t.Fatalf("probes = %v, want one", names(got))
	}
	if got[0].Spec["expect_contains"] != "True" {
		t.Errorf("control gate expects %v, want True", got[0].Spec["expect_contains"])
	}
	if EfficacyGate(simian.EngineKubeState, KubeStateNoOp) == "" {
		t.Error("the control advertises no efficacy gate")
	}
}
