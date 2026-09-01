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
	ProbeReachableBefore = "simian-reachable-before"
	ProbePartitioned     = "simian-partitioned"
	ProbeFastBefore      = "simian-fast-before"
	ProbeDelayed         = "simian-delayed"
	ProbeEnvoyRuntime    = "simian-envoy-runtime"
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
