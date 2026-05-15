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
