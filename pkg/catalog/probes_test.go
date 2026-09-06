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
	"encoding/base64"
	"slices"
	"strconv"
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
			if !g.expectEmpty && g.expect == "" && g.expectFrom == nil {
				t.Errorf("%s: %q asserts nothing", kind, g.probeName)
			}
			if g.expect != "" && g.expectFrom != nil {
				t.Errorf("%s: %q sets both expect and expectFrom", kind, g.probeName)
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
	k8s, logs := simian.ProbeTypeK8s, simian.ProbeTypeLogs
	for _, tc := range []struct {
		kind   string
		probes []string
		types  []string
	}{
		{KubeStateJobFailure, []string{ProbeJobFailed}, []string{k8s}},
		{KubeStateSelectorDrift, []string{ProbeWorkloadReady, ProbeNoEndpoints}, []string{k8s, k8s}},
		{KubeStateUnboundClaim, []string{ProbeClaimPending, ProbeUnschedulable}, []string{k8s, k8s}},
		// The only kind with a probe that is not a k8s read, because it is the
		// only kind with no field to read: the two healthy assertions come off
		// objects, the fault itself only off the log.
		{KubeStateDependencyStall,
			[]string{ProbeWorkloadReady, ProbeEndpointsReady, ProbeDependencyStall},
			[]string{k8s, k8s, logs}},
		{KubeStatePDBGridlock, []string{ProbeWorkloadReady, ProbePDBGridlocked}, []string{k8s, k8s}},
		// The one pair whose order is chosen rather than forced. Neither half is
		// an absence gate, so either could run first; the stall is the fault, and
		// failing on it first makes the failure report say the useful thing.
		{KubeStateRolloutStuck, []string{ProbeRolloutStuck, ProbeRolloutServing}, []string{k8s, k8s}},
		{KubeStateCertExpiry, []string{ProbeWorkloadReady, ProbeCertPresent}, []string{k8s, k8s}},
		{KubeStateNoOp, []string{ProbeWorkloadReady}, []string{k8s}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			got := DefaultProbes(kubeStateManifest(tc.kind, nil))
			if !slices.Equal(names(got), tc.probes) {
				t.Fatalf("probes = %v, want %v in that order", names(got), tc.probes)
			}
			for i, p := range got {
				if p.Mode != simian.ProbeModeSettle {
					t.Errorf("%s: mode = %q, want Settle", p.Name, p.Mode)
				}
				if p.Type != tc.types[i] {
					t.Errorf("%s: type = %q, want %q", p.Name, p.Type, tc.types[i])
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

// The negative test DependencyStall exists for: while the fault is live, every
// object-level check an agent would run comes back healthy.
//
// Written against the same jsonpath evaluator the probes use, over the state
// the synthesized bundle really reaches, so it is a claim about the fault and
// not about the gate table. If a future change to this kind makes the
// workload's failure visible on any object, one of these assertions inverts and
// the kind has quietly become an easier one.
func TestDependencyStallLeavesNothingWrongOnAnyObject(t *testing.T) {
	render := func(t *testing.T, expr string, items ...any) string {
		t.Helper()
		jp := jsonpath.New("gate")
		jp.AllowMissingKeys(true)
		if err := jp.Parse(expr); err != nil {
			t.Fatalf("parse %q: %v", expr, err)
		}
		var buf bytes.Buffer
		if err := jp.Execute(&buf, map[string]any{"items": items}); err != nil {
			t.Fatalf("execute %q: %v", expr, err)
		}
		return buf.String()
	}

	// One pod of a stalling workload: Running, both conditions true, no
	// restarts, no termination, no waiting reason. There is nothing here to
	// find, which is the point.
	stalledPod := map[string]any{
		"status": map[string]any{
			"phase": "Running",
			"conditions": []any{
				map[string]any{"type": "PodScheduled", "status": "True"},
				map[string]any{"type": "Ready", "status": "True"},
			},
			"containerStatuses": []any{map[string]any{
				"ready": true, "restartCount": int64(0),
				"state": map[string]any{"running": map[string]any{}},
			}},
		},
	}
	readySlice := map[string]any{
		"endpoints": []any{map[string]any{
			"addresses":  []any{"10.4.1.7"},
			"conditions": map[string]any{"ready": true},
		}},
	}

	gs := kubeStateGates[KubeStateDependencyStall]
	if len(gs) != 3 {
		t.Fatalf("gates = %d, want the two healthy assertions plus the log", len(gs))
	}
	if got := render(t, gs[0].jsonPath, stalledPod); !strings.Contains(got, gs[0].expect) {
		t.Errorf("readiness gate renders %q, want %q: the workload must be Ready while stalling", got, gs[0].expect)
	}
	if got := render(t, gs[1].jsonPath, readySlice); !strings.Contains(got, gs[1].expect) {
		t.Errorf("endpoint gate renders %q, want %q: the Service must be serving while stalling", got, gs[1].expect)
	}

	// The checks a subject would reach for first, and what they say. Every one
	// of them is the same answer it would give for the NoOp control.
	for _, tc := range []struct{ what, expr string }{
		{"crash loop", "{.items[*].status.containerStatuses[*].lastState.terminated.reason}"},
		{"waiting reason", "{.items[*].status.containerStatuses[*].state.waiting.reason}"},
		{"restart count", "{.items[*].status.containerStatuses[?(@.restartCount>0)].restartCount}"},
		{"not-ready condition", `{.items[*].status.conditions[?(@.status=="False")].type}`},
	} {
		if got := render(t, tc.expr, stalledPod); strings.TrimSpace(got) != "" {
			t.Errorf("a %s check on a stalling workload renders %q, want nothing", tc.what, got)
		}
	}

	// And the third gate is the one that can see it. It is a logs probe, it
	// looks for the line the driver writes, and it is last because the two
	// above it are what make "only the log is wrong" a claim rather than an
	// assumption.
	log := gs[2]
	if log.probeType != simian.ProbeTypeLogs {
		t.Errorf("the evidence gate is a %q probe, want logs", log.probeType)
	}
	if log.expectFrom == nil {
		t.Fatal("the log gate has no expectation derived from the manifest")
	}
	if got := log.expectFrom(kubeStateManifest(KubeStateDependencyStall, nil)); got != KubeStateDefaultStallMessage {
		t.Errorf("the log gate looks for %q, want the line the driver writes", got)
	}
	custom := "level=error upstream=ledger"
	if got := log.expectFrom(kubeStateManifest(KubeStateDependencyStall, map[string]any{"message": custom})); got != custom {
		t.Errorf("with a custom message the log gate looks for %q, want %q", got, custom)
	}
}

// renderPath evaluates a gate's jsonpath the way the k8s prober does, against
// the shape the dynamic client actually produces.
//
// The distinction matters for the numeric filters below. A dynamic-client list
// decodes JSON numbers to int64; a plain json.Unmarshal produces float64, and
// client-go's jsonpath refuses to compare a float64 against an integer literal
// with "incompatible types for comparison". A test written over float64 would
// fail against a filter that works in a cluster, and — worse — a filter written
// to satisfy such a test would fail against one that does not.
func renderPath(t *testing.T, expr string, items ...any) string {
	t.Helper()
	jp := jsonpath.New("gate")
	jp.AllowMissingKeys(true)
	if err := jp.Parse(expr); err != nil {
		t.Fatalf("parse %q: %v", expr, err)
	}
	var buf bytes.Buffer
	if err := jp.Execute(&buf, map[string]any{"items": items}); err != nil {
		t.Fatalf("execute %q: %v", expr, err)
	}
	return buf.String()
}

// The obvious spelling of this gate — render disruptionsAllowed, expect "0" —
// is a substring match against a decimal number, so it passes against 10 and
// 20 as well. The filter moves the comparison into the jsonpath and renders the
// name instead, so the value is either exactly zero or the gate sees nothing.
func TestThePDBGateOnlyMatchesExactlyZeroDisruptions(t *testing.T) {
	gs := kubeStateGates[KubeStatePDBGridlock]
	if len(gs) != 2 {
		t.Fatalf("gates = %d, want the readiness assertion plus the budget", len(gs))
	}
	g := gs[1]
	budget := func(name string, allowed int64) any {
		return map[string]any{
			"metadata": map[string]any{"name": name},
			"status":   map[string]any{"disruptionsAllowed": allowed},
		}
	}
	if got := renderPath(t, g.jsonPath, budget("ledger-api", 0)); !strings.Contains(got, "ledger-api") {
		t.Errorf("a gridlocked budget renders %q, want its name", got)
	}
	for _, allowed := range []int64{1, 10, 20} {
		if got := renderPath(t, g.jsonPath, budget("ledger-api", allowed)); strings.TrimSpace(got) != "" {
			t.Errorf("a budget allowing %d disruptions renders %q, which satisfies the gate", allowed, got)
		}
	}
	// The disruption controller writes no status at all until it has observed
	// the pods, and AllowMissingKeys has to carry that or the gate errors on its
	// first poll instead of waiting.
	if got := renderPath(t, g.jsonPath, map[string]any{"metadata": map[string]any{"name": "ledger-api"}}); strings.TrimSpace(got) != "" {
		t.Errorf("a budget with no status yet renders %q", got)
	}

	// And the name the gate looks for is the one Apply will create.
	m := kubeStateManifest(KubeStatePDBGridlock, nil)
	if got, want := g.expectFrom(m), KubeStateWorkloadName(KubeStatePDBGridlock, "", m.UID); got != want {
		t.Errorf("the gate looks for %q, Apply creates %q", got, want)
	}
}

// The second half of RolloutStuck is that nothing is down, and the number of
// replicas that must still be available is one the gate has to know before Apply
// creates any of them.
func TestTheRolloutGateExpectsThePreviousRevisionIntact(t *testing.T) {
	gs := kubeStateGates[KubeStateRolloutStuck]
	if len(gs) != 2 {
		t.Fatalf("gates = %d, want the stall plus the serving half", len(gs))
	}
	stuck, serving := gs[0], gs[1]

	// Progressing is a condition every healthy rollout also has, so the gate
	// reads its reason and not merely its presence.
	dep := func(reason string, available int64) any {
		return map[string]any{"status": map[string]any{
			"conditions": []any{
				map[string]any{"type": "Available", "status": "True", "reason": "MinimumReplicasAvailable"},
				map[string]any{"type": "Progressing", "status": "False", "reason": reason},
			},
			"availableReplicas": available,
		}}
	}
	if got := renderPath(t, stuck.jsonPath, dep("ProgressDeadlineExceeded", 2)); !strings.Contains(got, stuck.expect) {
		t.Errorf("a wedged rollout renders %q, want %q", got, stuck.expect)
	}
	// A healthy rollout's Progressing reason. If the gate matched on the
	// condition type it would pass here too, and prove nothing.
	if got := renderPath(t, stuck.jsonPath, dep("NewReplicaSetAvailable", 2)); strings.Contains(got, stuck.expect) {
		t.Errorf("a completed rollout renders %q, which satisfies the gate", got)
	}

	if got := renderPath(t, serving.jsonPath, dep("ProgressDeadlineExceeded", 2)); strings.TrimSpace(got) != "2" {
		t.Errorf("serving gate renders %q, want the available replica count", got)
	}
	m := kubeStateManifest(KubeStateRolloutStuck, nil)
	if got, want := serving.expectFrom(m), strconv.Itoa(KubeStateDefaultReplicas(KubeStateRolloutStuck)); got != want {
		t.Errorf("the gate expects %q available, the driver creates %q", got, want)
	}
	explicit := kubeStateManifest(KubeStateRolloutStuck, map[string]any{"replicas": float64(4)})
	if got := serving.expectFrom(explicit); got != "4" {
		t.Errorf("with spec.replicas 4 the gate expects %q, want 4", got)
	}
}

// The CertExpiry gate reads the Secret's own data, which comes back base64. It
// can prove the certificate landed and mounted; it cannot prove the expiry,
// because no probe type here can decode a certificate. That limit is stated in
// the gate's comment and the arithmetic is tested in the driver.
func TestTheCertGateMatchesAPEMHeaderThroughBase64(t *testing.T) {
	gs := kubeStateGates[KubeStateCertExpiry]
	if len(gs) != 2 {
		t.Fatalf("gates = %d, want the readiness assertion plus the Secret", len(gs))
	}
	g := gs[1]
	if g.expect != KubeStateCertPEMPrefix {
		t.Errorf("gate expects %q, want the computed PEM prefix %q", g.expect, KubeStateCertPEMPrefix)
	}
	// A whole number of base64 groups, or the encoding of the prefix is not a
	// prefix of the encoding and the gate silently never matches.
	if len(KubeStateCertPEMPrefix)%4 != 0 {
		t.Errorf("the prefix %q is not a whole number of base64 groups", KubeStateCertPEMPrefix)
	}

	pemCert := "-----BEGIN CERTIFICATE-----\nMIIB…\n-----END CERTIFICATE-----\n"
	secret := map[string]any{"data": map[string]any{
		"tls.crt": base64.StdEncoding.EncodeToString([]byte(pemCert)),
		"tls.key": base64.StdEncoding.EncodeToString([]byte("-----BEGIN PRIVATE KEY-----\n")),
	}}
	// The escaped dot is what makes jsonpath read `tls.crt` as one field name
	// rather than two; unescaped it renders nothing and the gate never passes.
	got := renderPath(t, g.jsonPath, secret)
	if !strings.HasPrefix(strings.TrimSpace(got), g.expect) {
		t.Errorf("the Secret renders %q, which does not begin with %q", got, g.expect)
	}
	if strings.Contains(got, base64.StdEncoding.EncodeToString([]byte("-----BEGIN PRIVATE KEY"))) {
		t.Error("the gate renders the private key as well as the certificate")
	}
}
