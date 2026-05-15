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

package chaosmesh

// specTemplates returns prompt-ready canonical spec shapes per Chaos Mesh
// resource kind. Used by Driver.Catalog() to populate CatalogEntry.SpecTemplate
// so the planner prompts can render templates directly from the catalog
// without keeping an inline copy.
//
// Each template includes its action enum (where applicable) so the LLM does
// not have to consult a separate verb table — picking outside the listed
// values causes Chaos Mesh's CRD validation to reject the manifest at apply
// time, which surfaces as driver.failed in the audit stream.
//
// Kinds present in this map but not installed in the cluster are simply
// never emitted (Catalog() only returns entries for installed CRDs).
var specTemplates = map[string]string{
	"PodChaos": `action MUST be one of: "pod-kill" | "pod-failure" | "container-kill"
{"action": "pod-kill", "mode": "one",
 "selector": {"namespaces": ["<ns>"], "labelSelectors": {"app": "<workload>"}}}`,

	"NetworkChaos": `action MUST be one of: "netem" | "delay" | "loss" | "duplicate" | "corrupt" | "partition" | "bandwidth"
NEVER "latency" — that belongs to IOChaos. NetworkChaos uses "delay" for latency injection.
{"action": "delay", "mode": "all",
 "selector": {"namespaces": ["<ns>"], "labelSelectors": {"app": "<workload>"}},
 "delay": {"latency": "250ms", "correlation": "0", "jitter": "0ms"}}
For "loss":      "loss":      {"loss": "10", "correlation": "0"}
For "bandwidth": "bandwidth": {"rate": "1mbps", "limit": 20971520, "buffer": 10000}
NOTE: On GKE Dataplane V2 (eBPF/Cilium) this CRD applies cleanly but the
network effect is silently bypassed. For real network impact on DPv2,
use the network-policy engine (partition only) or envoy-fault engine
(HTTP delay/abort).`,

	"StressChaos": `no "action" field — use "stressors": {"cpu": {...}} or {"memory": {...}}
{"mode": "one",
 "selector": {"namespaces": ["<ns>"], "labelSelectors": {"app": "<workload>"}},
 "stressors": {"cpu": {"workers": 2, "load": 80}}}
For memory: "stressors": {"memory": {"workers": 2, "size": "256MB"}}`,

	"IOChaos": `action MUST be one of: "latency" | "fault" | "attrOverride" | "mistake"
{"action": "latency", "mode": "one",
 "selector": {"namespaces": ["<ns>"], "labelSelectors": {"app": "<workload>"}},
 "volumePath": "/data", "path": "/data/**", "delay": "100ms", "percent": 100}`,

	"TimeChaos": `no "action" field — use "timeOffset": "<duration>"
{"mode": "one",
 "selector": {"namespaces": ["<ns>"], "labelSelectors": {"app": "<workload>"}},
 "timeOffset": "-10m"}`,

	"HTTPChaos": `action MUST be one of: "abort" | "delay" | "replace" | "patch"
{"action": "abort", "mode": "all", "port": 80, "method": "GET", "path": "/*",
 "selector": {"namespaces": ["<ns>"], "labelSelectors": {"app": "<workload>"}},
 "abort": true}
For "delay": "delay": "500ms"`,

	"DNSChaos": `action MUST be one of: "error" | "random"
{"action": "error", "mode": "all",
 "selector": {"namespaces": ["<ns>"], "labelSelectors": {"app": "<workload>"}},
 "patterns": ["*.<service>.svc.cluster.local"]}`,

	"BlockChaos": `action MUST be one of: "delay"
{"action": "delay", "mode": "one",
 "selector": {"namespaces": ["<ns>"], "labelSelectors": {"app": "<workload>"}},
 "delay": {"latency": "100ms"}, "volumeName": "data"}`,

	"JVMChaos": `action MUST be one of: "latency" | "exception" | "return" | "stress" | "gc" | "ruleData"
{"action": "latency", "mode": "one",
 "selector": {"namespaces": ["<ns>"], "labelSelectors": {"app": "<workload>"}},
 "class": "com.example.Service", "method": "handleRequest", "latencyDuration": 1000,
 "port": 9277}`,

	"KernelChaos": `injects kernel faults via BPF — high blast radius (TierNode); use sparingly.
{"mode": "one",
 "selector": {"namespaces": ["<ns>"], "labelSelectors": {"app": "<workload>"}},
 "failKernRequest": {"failtype": 0, "callchain": [{"funcname": "__x64_sys_mount"}]}}`,

	"PhysicalMachineChaos": `targets bare-metal nodes (TierNode); spec varies by action.
{"action": "stress-cpu", "address": ["<node-ip>:port"],
 "stress-cpu": {"load": 80, "workers": 2}}`,
}

// SpecTemplateFor returns the canonical spec template for a Chaos Mesh kind,
// or empty string if no template is registered. Exported for tests.
func SpecTemplateFor(kind string) string { return specTemplates[kind] }
