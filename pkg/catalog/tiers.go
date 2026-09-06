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

// Package catalog holds the static blast-radius tier map for known fault
// types and the per-spec re-classification logic that catches faults whose
// effective scope depends on their spec (notably NetworkChaos and DNSChaos
// targeting external destinations).
//
// Driver implementations call Classify when emitting catalog entries.
// The executor's safety stage calls ReclassifyForSpec on the effective
// manifest before checking it against installation tier policy.
package catalog

import (
	"net"
	"strings"

	"github.com/go-steer/simian-agent/pkg/simian"
)

// chaosMeshBaseTiers maps Chaos Mesh CRD Kinds to their baseline blast-radius
// tier. Per-spec re-classification may escalate (e.g. NetworkChaos with an
// external CIDR target jumps from namespace to external) but never deescalate.
var chaosMeshBaseTiers = map[string]simian.BlastRadiusTier{
	"NetworkChaos":         simian.TierNamespace,
	"PodChaos":             simian.TierNamespace,
	"IOChaos":              simian.TierNamespace,
	"StressChaos":          simian.TierNamespace,
	"TimeChaos":            simian.TierNamespace,
	"HTTPChaos":            simian.TierNamespace,
	"JVMChaos":             simian.TierNamespace,
	"DNSChaos":             simian.TierNamespace, // re-classified per-spec
	"BlockChaos":           simian.TierNamespace,
	"KernelChaos":          simian.TierNode,
	"PhysicalMachineChaos": simian.TierNode,
	"AWSChaos":             simian.TierExternal,
	"GCPChaos":             simian.TierExternal,
	"AzureChaos":           simian.TierExternal,
}

// chaosMeshNonFaultKinds enumerates chaos-mesh.org/v1alpha1 CRDs that are NOT
// directly user-applied faults: orchestration primitives (Workflow, Schedule,
// StatusCheck), cluster registration objects (RemoteCluster, PhysicalMachine),
// and the controller-managed Pod*Chaos manifestations of higher-level faults.
// IsUserFault returns false for these so they don't pollute the LLM's catalog.
var chaosMeshNonFaultKinds = map[string]bool{
	"Workflow":        true,
	"WorkflowNode":    true,
	"Schedule":        true,
	"StatusCheck":     true,
	"RemoteCluster":   true,
	"PhysicalMachine": true,
	"PodNetworkChaos": true,
	"PodIOChaos":      true,
	"PodHttpChaos":    true,
}

// kubeStateNamespaceKinds are the kube-state fault kinds whose effect is
// confined to the arena namespace. Each synthesizes a bundle of objects there
// and touches nothing that existed before it.
//
// Listed here rather than derived from the driver's own kind table so that
// classification is a decision this package records, not a side effect of a
// driver adding a map entry. A new kind is TierExternal — refused under the
// default policy — until someone states otherwise here.
var kubeStateNamespaceKinds = map[string]bool{
	"ImageUnresolvable":  true,
	"ContainerExitLoop":  true,
	"MemoryLimitSqueeze": true,
	"Unschedulable":      true,
	"JobFailure":         true,
	"SelectorDrift":      true,
	"UnboundClaim":       true,
	"DependencyStall":    true,
	"RolloutStuck":       true,
	"CertExpiry":         true,

	// Namespace tier with an asterisk, and the asterisk is worth stating.
	//
	// The objects are namespaced and the pods they protect are the fault's own,
	// so nothing outside the arena is touched — which is what this tier means
	// here. But a PodDisruptionBudget with no headroom is *designed* to be felt
	// at node level: it is what makes a drain of whichever node happens to host
	// the arena pod hang, and what makes a cluster autoscaler refuse to remove
	// that node. That is the fault, not a side effect of it.
	//
	// Left at namespace rather than escalated to node because the effect is
	// bounded by the fault's own lease — Clear and the reaper both delete the
	// PDB — and because escalating it would put a namespace-local object behind
	// the same gate as cordoning someone's node. An operator running this
	// against a cluster mid-upgrade should know what it does, which is what the
	// spec template and the docs say.
	"PDBGridlock": true,

	// The control. It breaks nothing at all, which makes it the narrowest
	// blast radius there is — but it is still listed rather than special-cased,
	// because a kind absent from this table is TierExternal and would be
	// refused, and a control that cannot be applied is a scoring path that
	// cannot be exercised.
	"NoOp": true,
}

// IsUserFault reports whether the given engine+kind is a user-facing fault
// type the LLM should consider proposing. Drivers should skip non-fault CRDs
// when building the catalog.
func IsUserFault(engine simian.Engine, kind string) bool {
	if engine == simian.EngineChaosMesh {
		return !chaosMeshNonFaultKinds[kind]
	}
	return true
}

// Classify returns the baseline tier for an engine + resource kind. Unknown
// kinds default to TierExternal so a misclassified new fault type fails
// closed against the v1 default policy.
func Classify(engine simian.Engine, kind string) simian.BlastRadiusTier {
	switch engine {
	case simian.EngineChaosMesh:
		if t, ok := chaosMeshBaseTiers[kind]; ok {
			return t
		}
	case simian.EngineLitmus:
		// M2 will populate Litmus experiment tiers from hub metadata.
		// Default to namespace for now since most ChaosHub experiments are
		// pod/workload scoped; refined when the Litmus driver lands.
		return simian.TierNamespace
	case simian.EngineNetworkPolicy:
		// NetworkPolicy partitions live entirely within one namespace —
		// they cannot affect resources outside it.
		return simian.TierNamespace
	case simian.EngineEnvoyFault:
		// Envoy fault filters operate inside the per-pod sidecar — scoped
		// to the workload's namespace.
		return simian.TierNamespace
	case simian.EngineKubeState:
		// Only kinds the driver actually synthesizes are namespace-scoped.
		// Falling through for anything else keeps the fail-closed default
		// meaningful as kinds are added. NodeUnready is the standing example:
		// every mechanism that could produce it acts on a real node, so it is
		// emphatically not namespace tier, and it must not inherit a namespace
		// classification just by arriving under this engine name.
		if kubeStateNamespaceKinds[kind] {
			return simian.TierNamespace
		}
	}
	return simian.TierExternal
}

// ReclassifyForSpec inspects a manifest's spec and may escalate its tier when
// the spec targets resources outside the cluster. The caller-supplied
// clusterCIDRs are the in-cluster pod and service CIDR ranges; targets falling
// outside those ranges are treated as external.
//
// In M1 we recognize:
//   - NetworkChaos with a `target.selector.externalTargets` or any IP/CIDR not
//     in clusterCIDRs.
//   - DNSChaos with `patterns` that do not match the in-cluster service domain
//     (`*.svc.cluster.local` and configured cluster domain suffixes).
//
// Any other spec leaves the tier unchanged.
func ReclassifyForSpec(m simian.FaultManifest, clusterCIDRs []*net.IPNet, clusterDomains []string) simian.BlastRadiusTier {
	current := m.BlastRadiusTier
	if current == "" {
		current = Classify(m.Engine, m.ResourceKind)
	}
	if m.Engine != simian.EngineChaosMesh {
		return current
	}
	switch m.ResourceKind {
	case "NetworkChaos":
		if hasExternalIPTarget(m.Spec, clusterCIDRs) {
			return simian.TierExternal
		}
	case "DNSChaos":
		if hasExternalDNSPattern(m.Spec, clusterDomains) {
			return simian.TierExternal
		}
	}
	return current
}

func hasExternalIPTarget(spec map[string]any, clusterCIDRs []*net.IPNet) bool {
	// NetworkChaos spec has a top-level `externalTargets` array of hostnames
	// or IPs; presence alone is enough to mark external.
	if v, ok := spec["externalTargets"]; ok {
		if arr, ok := v.([]any); ok && len(arr) > 0 {
			return true
		}
	}
	// Also check `target.value` if it's set as a CIDR or IP outside cluster ranges.
	if t, ok := spec["target"].(map[string]any); ok {
		if v, ok := t["value"].(string); ok && v != "" {
			if ip := parseIP(v); ip != nil && !inAnyCIDR(ip, clusterCIDRs) {
				return true
			}
		}
	}
	return false
}

func hasExternalDNSPattern(spec map[string]any, clusterDomains []string) bool {
	patterns, _ := spec["patterns"].([]any)
	if len(patterns) == 0 {
		return false
	}
	for _, p := range patterns {
		s, ok := p.(string)
		if !ok {
			continue
		}
		if !matchesAnyDomain(s, clusterDomains) {
			return true
		}
	}
	return false
}

func matchesAnyDomain(pattern string, clusterDomains []string) bool {
	pattern = strings.TrimSuffix(pattern, ".")
	for _, d := range clusterDomains {
		d = strings.TrimSuffix(d, ".")
		if strings.HasSuffix(pattern, "."+d) || pattern == d {
			return true
		}
	}
	return false
}

func parseIP(s string) net.IP {
	// Accept either a bare IP or a CIDR; for CIDR, return the network IP.
	if ip := net.ParseIP(s); ip != nil {
		return ip
	}
	if _, n, err := net.ParseCIDR(s); err == nil {
		return n.IP
	}
	return nil
}

func inAnyCIDR(ip net.IP, cidrs []*net.IPNet) bool {
	for _, c := range cidrs {
		if c.Contains(ip) {
			return true
		}
	}
	return false
}

// DefaultClusterDomains is the conventional in-cluster service DNS suffix.
// Installations using a different cluster domain should override via config.
var DefaultClusterDomains = []string{"svc.cluster.local", "cluster.local"}
