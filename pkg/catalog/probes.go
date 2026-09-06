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
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-steer/simian-agent/pkg/simian"
)

// Reserved names for the probes Simian attaches on its own. Prefixed so a
// manifest that deliberately overrides one has to say so by name, and so an
// audit record makes it obvious which probes the operator did not write.
const (
	ProbeReachableBefore   = "simian-reachable-before"
	ProbePartitioned       = "simian-partitioned"
	ProbeFastBefore        = "simian-fast-before"
	ProbeDelayed           = "simian-delayed"
	ProbeEnvoyRuntime      = "simian-envoy-runtime"
	ProbeImagePullFailed   = "simian-image-pull-failed"
	ProbeCrashLooping      = "simian-crash-looping"
	ProbeOOMKilled         = "simian-oom-killed"
	ProbeUnschedulable     = "simian-unschedulable"
	ProbeJobFailed         = "simian-job-failed"
	ProbeWorkloadReady     = "simian-workload-ready"
	ProbeNoEndpoints       = "simian-no-endpoints"
	ProbeClaimPending      = "simian-claim-pending"
	ProbeEndpointsReady    = "simian-endpoints-ready"
	ProbeEndpointsNotReady = "simian-endpoints-not-ready"
	ProbeDependencyStall   = "simian-dependency-stalled"
	ProbePDBGridlocked     = "simian-pdb-gridlocked"
	ProbeRolloutStuck      = "simian-rollout-stuck"
	ProbeRolloutServing    = "simian-rollout-serving"
	ProbeCertPresent       = "simian-cert-present"
	ProbeRestartsClimbing  = "simian-restarts-climbing"
	ProbeCrashLoopVisible  = "simian-crash-loop-visible"
)

// Envoy runtime keys, mirrored from pkg/sut/envoy's bootstrap. Duplicated
// rather than imported: pkg/sut/envoy pulls in the whole injection machinery,
// and the envoyfault driver already imports this package.
const (
	envoyDelayPercentKey = "fault.http.delay.fixed_delay_percent"
	envoyAbortPercentKey = "fault.http.abort.abort_percent"
	envoyAdminPort       = 15000
)

// gate is the default efficacy gate for one fault kind.
type gate struct {
	// describe is the one-line summary carried on the CatalogEntry.
	describe string
	// build produces the probes for a specific manifest. It returns nil when
	// this particular spec cannot be gated honestly — see the comments on each
	// builder. A nil result is not a failure: it means the fault is applied
	// unverified, which the audit trail records rather than hides.
	build func(simian.FaultManifest) []simian.ProbeSpec
}

type gateKey struct {
	engine simian.Engine
	kind   string
}

// gates is keyed by kind, not by cluster. The Chaos Mesh catalog is derived
// from live CRD discovery, so anything hand-listed per installation would go
// stale the moment a cluster ships a different set of CRDs.
var gates = map[gateKey]gate{
	{simian.EngineNetworkPolicy, "NetworkPolicy"}: {
		describe: "reachability: the target must answer on its container port before the fault, and stop answering after it",
		build:    networkPolicyProbes,
	},
	{simian.EngineChaosMesh, "NetworkChaos"}: {
		describe: "reachability or latency, depending on spec.action: partition must stop the target answering; delay must slow it measurably",
		build:    networkChaosProbes,
	},
	{simian.EngineEnvoyFault, "EnvoyHttpDelay"}: {
		describe: "the sidecar's admin API must report the delay runtime key at the requested percentage",
		build:    envoyDelayProbes,
	},
	{simian.EngineEnvoyFault, "EnvoyHttpAbort"}: {
		describe: "the sidecar's admin API must report the abort runtime key at the requested percentage",
		build:    envoyAbortProbes,
	},
	{simian.EngineKubeState, KubeStateImageUnresolvable}: {
		describe: "the synthesized workload's pods must report ImagePullBackOff",
		build:    kubeStateProbes,
	},
	{simian.EngineKubeState, KubeStateContainerExitLoop}: {
		describe: "the synthesized workload's containers must report a last termination of Error, have restarted enough times to be looping rather than merely broken, and be sitting in CrashLoopBackOff when the gate lets go",
		build:    kubeStateProbes,
	},
	{simian.EngineKubeState, KubeStateMemoryLimitSqueeze}: {
		describe: "the synthesized workload's containers must report a last termination of OOMKilled",
		build:    kubeStateProbes,
	},
	{simian.EngineKubeState, KubeStateUnschedulable}: {
		describe: "the synthesized workload's pods must report a PodScheduled condition of Unschedulable",
		build:    kubeStateProbes,
	},
	{simian.EngineKubeState, KubeStateJobFailure}: {
		describe: "the synthesized Job must report a Failed condition of reason BackoffLimitExceeded",
		build:    kubeStateProbes,
	},
	{simian.EngineKubeState, KubeStateSelectorDrift}: {
		describe: "the synthesized workload's pods must be Ready and the Service in front of them must have no endpoint addresses",
		build:    kubeStateProbes,
	},
	{simian.EngineKubeState, KubeStateBackendCrashLoop}: {
		describe: "the synthesized workload's containers must report a last termination of Error, have restarted enough times to be looping, and be sitting in CrashLoopBackOff, and the Service that selects them must report every endpoint not ready",
		build:    kubeStateProbes,
	},
	{simian.EngineKubeState, KubeStateUnboundClaim}: {
		describe: "the synthesized claim must be Pending and the pod mounting it must report Unschedulable",
		build:    kubeStateProbes,
	},
	{simian.EngineKubeState, KubeStateDependencyStall}: {
		describe: "the synthesized workload's pods must be Ready and its Service endpoints ready, and its log must carry the upstream-failure line — healthy everywhere but in what the application says",
		build:    kubeStateProbes,
	},
	{simian.EngineKubeState, KubeStatePDBGridlock}: {
		describe: "the synthesized workload's pods must be Ready and its PodDisruptionBudget must report disruptionsAllowed of exactly 0",
		build:    kubeStateProbes,
	},
	{simian.EngineKubeState, KubeStateRolloutStuck}: {
		describe: "the synthesized Deployment must report Progressing reason ProgressDeadlineExceeded while still reporting every replica of the previous revision available",
		build:    kubeStateProbes,
	},
	{simian.EngineKubeState, KubeStateCertExpiry}: {
		describe: "the synthesized workload's pods must be Ready — which is only possible once the TLS Secret mounted — and the Secret must hold a PEM certificate",
		build:    kubeStateProbes,
	},
	{simian.EngineKubeState, KubeStateNoOp}: {
		describe: "the control workload's pods must be Ready — the gate a control passes is the one every other kind fails",
		build:    kubeStateProbes,
	},
}

// EfficacyGate returns the one-line description of the default gate for a
// fault kind, or "" if it has none. Drivers put this on their CatalogEntry so
// the planner can tell a verified fault kind from an unverified one.
func EfficacyGate(engine simian.Engine, kind string) string {
	return gates[gateKey{engine, kind}].describe
}

// DefaultProbes returns the probes Simian attaches to m on its own.
//
// Probes the manifest already declares under the same name win: a caller who
// needs different timing or a different port says so by name, deliberately and
// visibly. Everything else is additive — a manifest cannot dissolve its own
// gate by declaring one unrelated probe.
func DefaultProbes(m simian.FaultManifest) []simian.ProbeSpec {
	g, ok := gates[gateKey{m.Engine, m.ResourceKind}]
	if !ok || g.build == nil {
		return nil
	}
	declared := map[string]bool{}
	for _, p := range m.Probes {
		declared[p.Name] = true
	}
	var out []simian.ProbeSpec
	for _, p := range g.build(m) {
		if !declared[p.Name] {
			out = append(out, p)
		}
	}
	return out
}

// networkPolicyProbes gates a NetworkPolicy partition.
//
// Only ingress partitions get a gate. The controller proves an ingress
// partition by failing to reach the target itself; it has no vantage point
// from which to observe the target's own egress being cut, and a probe that
// cannot see the fault would fail every time and reject a fault that worked.
func networkPolicyProbes(m simian.FaultManifest) []simian.ProbeSpec {
	if !directionsInclude(m.Spec, "ingress") {
		return nil
	}
	return reachabilityPair(labelSelectorOf(m.Spec, "labelSelectors"))
}

// networkChaosProbes gates a Chaos Mesh NetworkChaos.
//
// A NetworkChaos that names a `target` (or `externalTargets`) applies only
// between two labelled sets, and the controller is in neither of them: it
// would see nothing change and reject a fault that worked. Those get no
// default gate.
func networkChaosProbes(m simian.FaultManifest) []simian.ProbeSpec {
	if _, ok := m.Spec["target"]; ok {
		return nil
	}
	if _, ok := m.Spec["externalTargets"]; ok {
		return nil
	}
	selector := labelSelectorOf(nestedMap(m.Spec, "selector"), "labelSelectors")
	switch stringField(m.Spec, "action") {
	case "partition":
		return reachabilityPair(selector)
	case "delay":
		return delayPair(selector, nestedMap(m.Spec, "delay"))
	default:
		// loss, duplicate, corrupt and bandwidth are statistical: one request
		// from one prober cannot separate "5% loss landed" from "5% loss did
		// not", and inventing a probe that guesses would reject working faults.
		return nil
	}
}

// reachabilityPair is the two-sided partition gate: reachable before,
// unreachable after. The SOT half is what makes the Settle half mean anything
// — "nothing answered" is not evidence of a partition against a workload that
// was never answering.
func reachabilityPair(selector map[string]string) []simian.ProbeSpec {
	return []simian.ProbeSpec{
		{
			Name: ProbeReachableBefore,
			Type: simian.ProbeTypeHTTP,
			Mode: simian.ProbeModeSOT,
			Spec: withSelector(selector, map[string]any{
				"expect_reachable": true,
				"timeout":          "30s",
				"interval":         "2s",
			}),
		},
		{
			Name: ProbePartitioned,
			Type: simian.ProbeTypeHTTP,
			Mode: simian.ProbeModeSettle,
			Spec: withSelector(selector, map[string]any{
				"expect_unreachable": true,
				"timeout":            "60s",
				"interval":           "2s",
			}),
		},
	}
}

// minGatedDelay is the smallest injected latency worth gating. Below it the
// signal is inside the noise of an in-cluster HTTP round trip, and the gate
// would reject working faults more often than it caught broken ones.
const minGatedDelay = 100 * time.Millisecond

// delayPair gates a netem delay by measuring it.
//
// The Settle threshold is half the injected latency, not all of it. netem
// applies the delay to one direction of the round trip, and jitter and
// correlation can pull the observed figure below the nominal one; half still
// separates "200ms landed" from "nothing landed" by two orders of magnitude
// while leaving room for the fault to be applied honestly and still measure
// low.
func delayPair(selector map[string]string, delay map[string]any) []simian.ProbeSpec {
	latency, err := time.ParseDuration(stringField(delay, "latency"))
	if err != nil || latency < minGatedDelay {
		return nil
	}
	return []simian.ProbeSpec{
		{
			Name: ProbeFastBefore,
			Type: simian.ProbeTypeHTTP,
			Mode: simian.ProbeModeSOT,
			Spec: withSelector(selector, map[string]any{
				"expect_reachable": true,
				"max_latency":      (latency / 4).String(),
				"timeout":          "30s",
				"interval":         "2s",
			}),
		},
		{
			Name: ProbeDelayed,
			Type: simian.ProbeTypeHTTP,
			Mode: simian.ProbeModeSettle,
			Spec: withSelector(selector, map[string]any{
				"min_latency":     (latency / 2).String(),
				"request_timeout": (latency * 4).String(),
				"timeout":         "60s",
				"interval":        "2s",
			}),
		},
	}
}

// kubeStateGate is one assertion a synthesized fault kind proves itself on.
type kubeStateGate struct {
	probeName string

	// probeType is the prober that runs this gate. Empty means k8s, which is
	// what every gate that reads a field off an object uses. The exception is
	// DependencyStall, whose defining property is that no field is wrong.
	probeType string

	// resource the probe reads. Empty means pods, which is what most of these
	// kinds show their failure on. Ignored by a logs gate, which reads pods
	// either way.
	resource string

	// selectorKey is the label key the probe selects the workload name on.
	// Empty means "app", which is what the driver puts on every object it
	// synthesizes. EndpointSlices are the exception: they are written by the
	// endpointslice controller, which labels them with the Service they belong
	// to and nothing else.
	selectorKey string

	// jsonPath is what a k8s gate renders. A logs gate has no use for it: a log
	// is text, not an object.
	jsonPath string

	expect string

	// expectFrom computes the expected value from the manifest, for a gate
	// whose evidence is something the manifest chose rather than something the
	// control plane writes. Wins over expect when set.
	expectFrom func(simian.FaultManifest) string

	// expectEmpty asserts the jsonpath renders nothing, for a gate whose
	// evidence is an absence. Mutually exclusive with expect, and never the
	// only gate on a kind — an empty read is also what a resource that does
	// not exist yet produces, so an expectEmpty probe has to be preceded by
	// one that proves the objects are there.
	expectEmpty bool

	// expectAtLeast asserts the jsonpath renders counters, all of them at
	// least this large. For a gate whose evidence is a repetition rather than
	// a state: the difference between a container that failed and one that is
	// failing over and over cannot be written as a string match.
	expectAtLeast int

	timeout string
}

// crashLoopVisibleGate is the last gate on both crash-loop kinds: the pod is in
// `waiting: CrashLoopBackOff` at the moment the harness stops asking.
//
// This is the criterion the other two gates were written to avoid, and it is
// safe here for a reason that is only true here: the gate before it has already
// waited out the short backoff windows. Below five restarts the kubelet's
// backoff is 10s to 80s, and sampling for this state inside a window that short
// caught it one poll in six. From the fifth restart the window is 160s, and the
// pod both enters CrashLoopBackOff and stays there.
//
// It is not instant, and the first draft of this comment claimed it was.
// Measured across three consecutive runs on GKE 1.36, the gate passed in 65.6s,
// 78.4s and 82.4s — 32 to 40 polls, each of them comfortably inside one 160s
// window but nowhere near its start. The timeout is set well above the slowest
// of those rather than close to it.
//
// Without it the harness asks its question about a second after the restart
// counter ticks, which is the one moment a loop looks least like one. Measured:
// three consecutive runs of lookout-crash-loop scored severity 0.67, 1.00, 1.00
// — full recall every time, and a namespace verdict that depended on which side
// of a restart the scan landed. Two runs of one scenario that disagree are a
// harness bug by definition, because there is nothing else left to vary.
//
// So the gate is not here to prove the fault landed; the two before it did
// that. It is here to hand the subject a steady state instead of a transient.
var crashLoopVisibleGate = kubeStateGate{
	probeName: ProbeCrashLoopVisible,
	jsonPath:  "{.items[*].status.containerStatuses[*].state.waiting.reason}",
	expect:    "CrashLoopBackOff",
	// One replica is enough, and expect_contains can only say that anyway. The
	// stronger claim — every replica is looping — is the gate above, and this
	// one is about what is on screen rather than about what landed.
	timeout: "3m",
}

// kubeStateGates are run in order and stop at the first failure, which is what
// lets a later gate depend on what an earlier one established.
var kubeStateGates = map[string][]kubeStateGate{
	KubeStateImageUnresolvable: {{
		probeName: ProbeImagePullFailed,
		jsonPath:  "{.items[*].status.containerStatuses[*].state.waiting.reason}",
		// The kubelet reports ErrImagePull on the first failure and
		// ImagePullBackOff once it starts backing off. Matching the backoff
		// state rather than the first error is the stabler assertion: it is
		// where the pod stays, so the probe does not depend on catching a
		// transient on the right poll.
		expect:  "ImagePullBackOff",
		timeout: "90s",
	}},
	KubeStateContainerExitLoop: {
		{
			probeName: ProbeCrashLooping,
			// Not state.waiting.reason == CrashLoopBackOff, which is the obvious
			// choice and is a coin flip. A container that exits immediately spends
			// almost all of its time with the *previous* termination showing in
			// `state.terminated`; the kubelet only flips to `waiting:
			// CrashLoopBackOff` in a narrow window around each restart decision.
			// Measured on GKE 1.36: one poll in six saw CrashLoopBackOff, and a
			// 90s / 44-poll gate missed it entirely on one run and passed in 6.5s
			// on another. A gate that flaky reports a fault that landed as inert,
			// which is the exact failure the gates exist to prevent.
			//
			// lastState.terminated.reason is stable from the first restart on.
			jsonPath: "{.items[*].status.containerStatuses[*].lastState.terminated.reason}",
			// "Error" is the kubelet's reason for a non-zero exit. It reads
			// generic, and is specific enough here: the workload is synthesized,
			// so nothing else could have terminated these pods, and the one other
			// way this engine kills a container reports OOMKilled instead.
			expect:  "Error",
			timeout: "90s",
		},
		{
			// And it is still going. The gate above is stable from the first
			// restart on, which is exactly its problem: it passes 2s after
			// Apply, when the container has died once and nothing is looping
			// yet. Handing the scenario to a subject there asks it to see a
			// crash loop that has not happened.
			probeName:     ProbeRestartsClimbing,
			jsonPath:      "{.items[*].status.containerStatuses[*].restartCount}",
			expectAtLeast: KubeStateCrashLoopRestarts,
			// Double the ~150s the backoff schedule needs, which leaves room
			// for a slow image pull and not much more. It is deliberately not
			// more generous than that: every gate's worst case has to fit
			// inside the fault's lease, and the lease has to fit inside the
			// executor's 15-minute ceiling.
			timeout: "5m",
		},
		crashLoopVisibleGate,
	},
	KubeStateMemoryLimitSqueeze: {{
		probeName: ProbeOOMKilled,
		// lastState, not state, for the reason above, plus one specific to
		// this kind: by the time the kubelet is backing off, the OOM kill is
		// only visible as the previous termination. Asserting on the current
		// state would pass for any crash loop and prove nothing about memory.
		jsonPath: "{.items[*].status.containerStatuses[*].lastState.terminated.reason}",
		expect:   "OOMKilled",
		// Longer than the others: the container has to start, allocate past
		// its limit, be killed, and be restarted before lastState is
		// populated at all.
		timeout: "120s",
	}},
	KubeStateUnschedulable: {{
		probeName: ProbeUnschedulable,
		// Unschedulable is the reason on the PodScheduled condition, and it is
		// the only condition reason with that value, so matching across all
		// conditions is specific without needing a jsonpath filter
		// expression. Asserting on phase == Pending instead would also pass
		// while an image is still being pulled.
		jsonPath: "{.items[*].status.conditions[*].reason}",
		expect:   "Unschedulable",
		timeout:  "60s",
	}},
	KubeStateJobFailure: {{
		probeName: ProbeJobFailed,
		resource:  "jobs",
		// The Job's own Failed condition, not the state of its pods. Dead pods
		// are what a crash loop looks like too; BackoffLimitExceeded is the
		// moment the Job gives up, which is the thing this kind is about and
		// the thing whatever was waiting on the Job will never recover from.
		jsonPath: "{.items[*].status.conditions[*].reason}",
		expect:   "BackoffLimitExceeded",
		// Generous, because the wait is structural: the Job controller delays
		// 10s before the first retry and doubles from there, so even the
		// default backoff_limit of 2 is three pod starts and 30s of deliberate
		// idling before the condition appears.
		timeout: "180s",
	}},
	KubeStateSelectorDrift: {
		{
			// First, and not optional. This kind's evidence is an absence, and
			// an absence proves nothing until something is there to be absent
			// from: pods that are Running and Ready are what make "no
			// endpoints" mean a broken Service rather than a namespace that
			// has not finished starting.
			probeName: ProbeWorkloadReady,
			jsonPath:  podReadyJSONPath,
			expect:    "True",
			timeout:   "120s",
		},
		{
			probeName: ProbeNoEndpoints,
			resource:  "endpointslices",
			// Written by the endpointslice controller, which labels each slice
			// with the Service it belongs to. The Service shares the bundle's
			// name, so this finds the drifted Service's slices and no others.
			selectorKey: "kubernetes.io/service-name",
			// The addresses, not the slices. A Service selecting nothing still
			// gets a placeholder EndpointSlice, so asserting no slice exists
			// would fail against a fault that landed perfectly.
			jsonPath:    "{.items[*].endpoints[*].addresses[*]}",
			expectEmpty: true,
			timeout:     "30s",
		},
	},
	KubeStateBackendCrashLoop: {
		{
			// The cause first, and here the order carries the meaning rather
			// than only the failure message. This kind is the one scoring uses
			// to ask whether a subject found the root or stopped at the
			// symptom, so the gate proves them in that order too: the pods are
			// crash-looping, and *therefore* the Service has nothing ready.
			//
			// Same expression and same reasoning as ContainerExitLoop —
			// lastState.terminated.reason rather than the CrashLoopBackOff
			// waiting reason, which is a coin flip on any given poll.
			probeName: ProbeCrashLooping,
			jsonPath:  "{.items[*].status.containerStatuses[*].lastState.terminated.reason}",
			expect:    "Error",
			timeout:   "90s",
		},
		{
			// Both replicas, and the "every value" reading of expect_at_least
			// is doing real work here: a Service whose backends are half down
			// is a different diagnosis from one whose backends are all down,
			// and this kind is the second.
			probeName:     ProbeRestartsClimbing,
			jsonPath:      "{.items[*].status.containerStatuses[*].restartCount}",
			expectAtLeast: KubeStateCrashLoopRestarts,
			timeout:       "5m",
		},
		crashLoopVisibleGate,
		{
			probeName:   ProbeEndpointsNotReady,
			resource:    "endpointslices",
			selectorKey: "kubernetes.io/service-name",
			// Not expectEmpty, and the difference from SelectorDrift is the
			// whole point of having both kinds. A Service that selects nothing
			// gets a slice with no addresses; a Service that selects pods which
			// are not serving gets a slice that lists them with ready false.
			// Asserting emptiness here would fail against a fault that landed,
			// and asserting this against SelectorDrift would render nothing at
			// all — the two shapes are distinguishable, which is what a subject
			// is being asked to do.
			//
			// A crash-looping container is Ready whenever it is Running, so this
			// condition flickers unless the container carries a readiness probe
			// that can never pass. It carries one — not for this gate's sake,
			// which polls and would simply wait out the flicker, but for the
			// subject's; see backendCrashLoopBundle.
			jsonPath: "{.items[*].endpoints[*].conditions.ready}",
			expect:   "false",
			timeout:  "90s",
		},
	},
	KubeStateUnboundClaim: {
		{
			// The cause before the symptom. A Pending claim is unambiguous;
			// the pod below it can be Pending for reasons that have nothing to
			// do with storage.
			probeName: ProbeClaimPending,
			resource:  "persistentvolumeclaims",
			jsonPath:  "{.items[*].status.phase}",
			expect:    "Pending",
			timeout:   "60s",
		},
		{
			probeName: ProbeUnschedulable,
			jsonPath:  "{.items[*].status.conditions[*].reason}",
			expect:    "Unschedulable",
			timeout:   "120s",
		},
	},
	KubeStateDependencyStall: {
		{
			// Not preamble. This kind's whole claim is that everything an agent
			// normally checks is clean, and a gate that only read the log would
			// pass just as happily against a workload that was crash-looping —
			// which is a different fault, and one the subject would find without
			// reading anything. The two healthy assertions are what make the
			// third one mean "and *only* the log is wrong".
			probeName: ProbeWorkloadReady,
			jsonPath:  podReadyJSONPath,
			expect:    "True",
			timeout:   "120s",
		},
		{
			probeName:   ProbeEndpointsReady,
			resource:    "endpointslices",
			selectorKey: "kubernetes.io/service-name",
			// The endpoint's own ready condition, not merely that an address
			// exists. A Service in front of a pod that is not serving still gets
			// a slice; the condition is what says traffic would be sent there.
			jsonPath: "{.items[*].endpoints[*].conditions.ready}",
			expect:   "true",
			timeout:  "60s",
		},
		{
			probeName: ProbeDependencyStall,
			probeType: simian.ProbeTypeLogs,
			// The only evidence this fault leaves, and the reason the logs probe
			// type exists. Computed from the manifest because the line is the
			// manifest's to choose — see catalog.KubeStateStallMessage, which
			// the driver resolves the same way before it writes the pod.
			expectFrom: func(m simian.FaultManifest) string {
				return KubeStateStallMessage(stringField(m.Spec, "message"))
			},
			// Short. The workload writes its first line as soon as httpd is up,
			// and the two gates before this one have already waited for that.
			timeout: "60s",
		},
	},
	KubeStatePDBGridlock: {
		{
			// The pods first, and for this kind that is not just preamble:
			// disruptionsAllowed is computed from how many of the covered pods
			// are healthy, so a budget over pods that never started reports 0
			// too. Proving the pods are Ready is what makes the 0 below mean
			// "there is no headroom" rather than "there is nothing here".
			probeName: ProbeWorkloadReady,
			jsonPath:  podReadyJSONPath,
			expect:    "True",
			timeout:   "120s",
		},
		{
			probeName: ProbePDBGridlocked,
			resource:  "poddisruptionbudgets",
			// The filter does the comparison, and the render is the name. The
			// obvious spelling — render disruptionsAllowed and expect "0" — is
			// a substring match against a decimal number, so it would pass just
			// as happily against 10 or 20. Here the name only appears at all
			// when the value is exactly zero.
			//
			// The comparison works because the dynamic client decodes JSON
			// numbers to int64; the same filter against a float64 would fail
			// with "incompatible types for comparison" and reject a fault that
			// landed.
			jsonPath: "{.items[?(@.status.disruptionsAllowed==0)].metadata.name}",
			expectFrom: func(m simian.FaultManifest) string {
				return KubeStateWorkloadName(m.ResourceKind, stringField(m.Spec, "name"), m.UID)
			},
			// The disruption controller has to observe the pods before it writes
			// the status at all, and until it does the field is absent rather
			// than zero.
			timeout: "90s",
		},
	},
	KubeStateRolloutStuck: {
		{
			// Stuck first, serving second, and the order is a choice rather than
			// a constraint: neither of these is an absence gate, so either could
			// run first. This one is the fault. If the rollout never wedged there
			// is nothing to prove about what users saw, and failing on the
			// stronger assertion first makes the failure report say the useful
			// thing.
			probeName: ProbeRolloutStuck,
			resource:  "deployments",
			jsonPath:  `{.items[*].status.conditions[?(@.type=="Progressing")].reason}`,
			expect:    "ProgressDeadlineExceeded",
			// The deployment controller does not write this reason until the
			// progress deadline has actually elapsed, which is the kind's own
			// spec.progress_deadline_seconds — 60 by default, up to 600. Long
			// enough for the default with room to spare, and a manifest that
			// raises the deadline past this is choosing a gate that will time
			// out, which the spec template says.
			timeout: "240s",
		},
		{
			probeName: ProbeRolloutServing,
			resource:  "deployments",
			// The other half of the diagnosis, and the half that makes this kind
			// hard: nothing is down. The previous revision is still serving every
			// replica it was, so no availability signal fires and the only
			// evidence is the rollout itself.
			jsonPath: "{.items[*].status.availableReplicas}",
			expectFrom: func(m simian.FaultManifest) string {
				n, ok := intField(m.Spec, "replicas")
				if !ok {
					n = KubeStateDefaultReplicas(m.ResourceKind)
				}
				return strconv.Itoa(n)
			},
			// Short: by the time the deadline above has expired the old replicas
			// have been available for minutes. This is a read, not a wait.
			timeout: "60s",
		},
	},
	KubeStateCertExpiry: {
		{
			// Ready pods are load-bearing evidence here, not a warm-up. The
			// Deployment mounts the Secret, so the kubelet will not start a
			// container until the Secret exists and its keys are present: a
			// Ready pod proves the certificate landed and is mountable, which no
			// read of the Secret alone would.
			probeName: ProbeWorkloadReady,
			jsonPath:  podReadyJSONPath,
			expect:    "True",
			timeout:   "120s",
		},
		{
			probeName: ProbeCertPresent,
			resource:  "secrets",
			// The dot in the key is escaped so jsonpath reads `tls.crt` as one
			// field name rather than two.
			jsonPath: `{.items[*].data.tls\.crt}`,
			// Secret data comes back base64, so the assertion is on the base64
			// rendering of a PEM header — the certificate is well-formed enough
			// to begin like one.
			//
			// This is the honest limit of the gate, and it is worth stating: no
			// probe type in Simian can parse a certificate, so nothing here
			// proves the *expiry*. The arithmetic is proved by a driver unit test
			// that parses the DER it generated. What the live gate proves is that
			// a certificate reached the cluster and a pod mounted it.
			expect:  KubeStateCertPEMPrefix,
			timeout: "30s",
		},
	},
	KubeStateNoOp: {{
		// The control gets a gate like everything else, and it asserts the
		// opposite of one: the workload came up and is serving. Without it a
		// control would "inject" successfully against a cluster that was too
		// broken to run anything, and the subject's correct finding of nothing
		// would be scored as a correct finding rather than as the vacuous pass
		// it is.
		probeName: ProbeWorkloadReady,
		jsonPath:  podReadyJSONPath,
		expect:    "True",
		timeout:   "120s",
	}},
}

// podReadyJSONPath renders the status of each pod's Ready condition. The
// filter is what keeps it honest: without it the expression would render every
// condition's status and match "True" from PodScheduled alone, which is true
// of a pod that has not pulled its image yet.
const podReadyJSONPath = `{.items[*].status.conditions[?(@.type=="Ready")].status}`

// kubeStateProbes gates a synthesized declarative-state fault by reading the
// state back off the pods it created.
//
// There is no SOT half here, and its absence is not an oversight. The
// dataplane gates need one because their Settle assertion is differential:
// "the target does not answer" proves nothing about a workload that was not
// answering beforehand. A synthesized workload did not exist before Apply, so
// "these pods are in ImagePullBackOff" cannot be a pre-existing condition —
// there is nothing for a precheck to rule out. When `mutate` mode lands it
// will need the SOT half back, because there the workload was already running
// and might already have been broken.
//
// The selector names pods Apply has not created yet, which is possible because
// KubeStateWorkloadName derives the workload name from the fault UID.
func kubeStateProbes(m simian.FaultManifest) []simian.ProbeSpec {
	gs, ok := kubeStateGates[m.ResourceKind]
	if !ok {
		return nil
	}
	// Only synthesize mode creates the pods this gate reads. Anything else is
	// either unimplemented or a spec the driver will reject, and attaching a
	// probe for pods that will never appear would turn a clear driver error
	// into a probe timeout.
	if mode := stringField(m.Spec, "mode"); mode != "" && mode != "synthesize" {
		return nil
	}
	name := KubeStateWorkloadName(m.ResourceKind, stringField(m.Spec, "name"), m.UID)
	if name == "" {
		return nil
	}
	out := make([]simian.ProbeSpec, 0, len(gs))
	for _, g := range gs {
		key := g.selectorKey
		if key == "" {
			key = "app"
		}
		want := g.expect
		if g.expectFrom != nil {
			want = g.expectFrom(m)
		}
		spec := map[string]any{
			"label_selector": key + "=" + name,
			"timeout":        g.timeout,
			"interval":       "2s",
		}
		typ := g.probeType
		if typ == "" {
			typ = simian.ProbeTypeK8s
		}
		if typ == simian.ProbeTypeLogs {
			// A log is text. There is no resource to name and no jsonpath to
			// render, and no expect_empty either: "the log does not say X" is
			// satisfied by a container that never started, which is the vacuous
			// pass the gates exist to refuse. See pkg/probe's logs prober.
			spec["expect_contains"] = want
		} else {
			resource := g.resource
			if resource == "" {
				resource = "pods"
			}
			spec["resource"] = resource
			spec["jsonpath"] = g.jsonPath
			switch {
			case g.expectEmpty:
				spec["expect_empty"] = true
			case g.expectAtLeast > 0:
				spec["expect_at_least"] = g.expectAtLeast
			default:
				spec["expect_contains"] = want
			}
		}
		out = append(out, simian.ProbeSpec{
			Name: g.probeName,
			Type: typ,
			Mode: simian.ProbeModeSettle,
			Spec: spec,
		})
	}
	return out
}

func envoyDelayProbes(m simian.FaultManifest) []simian.ProbeSpec {
	return envoyRuntimeProbes(m, envoyDelayPercentKey)
}

func envoyAbortProbes(m simian.FaultManifest) []simian.ProbeSpec {
	return envoyRuntimeProbes(m, envoyAbortPercentKey)
}

// envoyRuntimeProbes reads the fault percentage back out of the sidecar's own
// admin API.
//
// The driver's Apply is a POST to /runtime_modify, and a 200 from that endpoint
// says the request was accepted, not that the filter is live. GET /runtime
// reports the value Envoy is actually running with, which is the difference
// between "we asked" and "it happened".
func envoyRuntimeProbes(m simian.FaultManifest, key string) []simian.ProbeSpec {
	pct, ok := intField(m.Spec, "percentage")
	if !ok {
		return nil
	}
	return []simian.ProbeSpec{
		{
			Name: ProbeEnvoyRuntime,
			Type: simian.ProbeTypeHTTP,
			Mode: simian.ProbeModeSettle,
			Spec: withSelector(labelSelectorOf(m.Spec, "labelSelectors"), map[string]any{
				"port":          envoyAdminPort,
				"path":          "/runtime",
				"jsonpath":      fmt.Sprintf("{.entries['%s'].final_value}", escapeJSONPathKey(key)),
				"expect_equals": strconv.Itoa(pct),
				"timeout":       "30s",
				"interval":      "2s",
			}),
		},
	}
}

// escapeJSONPathKey backslash-escapes the dots in a runtime key so client-go's
// jsonpath treats "fault.http.delay..." as one field name rather than five.
func escapeJSONPathKey(key string) string {
	out := make([]rune, 0, len(key)*2)
	for _, r := range key {
		if r == '.' {
			out = append(out, '\\')
		}
		out = append(out, r)
	}
	return string(out)
}

// withSelector pins the probe to the engine-native selector when the spec has
// one. Without it the probe inherits the manifest's TargetRef labels, which
// are a denormalized copy and may be absent.
func withSelector(selector map[string]string, spec map[string]any) map[string]any {
	if len(selector) == 0 {
		return spec
	}
	spec["label_selector"] = renderSelector(selector)
	return spec
}

// renderSelector builds a label-selector string, sorted so the probe spec is
// byte-stable across runs and diffs cleanly in an audit record.
func renderSelector(labels map[string]string) string {
	parts := make([]string, 0, len(labels))
	for k, v := range labels {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// directionsInclude reports whether a network-policy spec cuts the given
// direction. Absent means both, matching the driver's own default.
func directionsInclude(spec map[string]any, want string) bool {
	raw, ok := spec["directions"]
	if !ok || raw == nil {
		return true
	}
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return true
	}
	for _, v := range list {
		if s, ok := v.(string); ok && s == want {
			return true
		}
	}
	return false
}

// labelSelectorOf pulls a string-map selector out of a spec field.
func labelSelectorOf(spec map[string]any, field string) map[string]string {
	raw := nestedMap(spec, field)
	if len(raw) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, v := range raw {
		s, ok := v.(string)
		if !ok {
			return nil
		}
		out[k] = s
	}
	return out
}

func nestedMap(spec map[string]any, field string) map[string]any {
	if spec == nil {
		return nil
	}
	m, _ := spec[field].(map[string]any)
	return m
}

func stringField(spec map[string]any, field string) string {
	if spec == nil {
		return ""
	}
	s, _ := spec[field].(string)
	return s
}

// intField accepts the shapes a decoded JSON spec can carry a number in.
func intField(spec map[string]any, field string) (int, bool) {
	if spec == nil {
		return 0, false
	}
	switch n := spec[field].(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if n != float64(int(n)) {
			return 0, false
		}
		return int(n), true
	default:
		return 0, false
	}
}
