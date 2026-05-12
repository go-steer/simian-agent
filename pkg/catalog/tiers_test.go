package catalog

import (
	"net"
	"testing"

	"github.com/go-steer/simian-agent/pkg/simian"
)

func TestClassifyKnownKinds(t *testing.T) {
	cases := []struct {
		kind string
		want simian.BlastRadiusTier
	}{
		{"NetworkChaos", simian.TierNamespace},
		{"PodChaos", simian.TierNamespace},
		{"KernelChaos", simian.TierNode},
		{"PhysicalMachineChaos", simian.TierNode},
		{"AWSChaos", simian.TierExternal},
		{"GCPChaos", simian.TierExternal},
		{"UnknownKindFromFuture", simian.TierExternal},
	}
	for _, c := range cases {
		if got := Classify(simian.EngineChaosMesh, c.kind); got != c.want {
			t.Errorf("Classify(%s)=%s, want %s", c.kind, got, c.want)
		}
	}
}

func TestReclassifyNetworkChaosExternalEscape(t *testing.T) {
	clusterCIDR := mustCIDR(t, "10.0.0.0/8")
	manifest := simian.FaultManifest{
		Engine:       simian.EngineChaosMesh,
		ResourceKind: "NetworkChaos",
		Spec: map[string]any{
			"externalTargets": []any{"api.stripe.com"},
		},
	}
	got := ReclassifyForSpec(manifest, []*net.IPNet{clusterCIDR}, DefaultClusterDomains)
	if got != simian.TierExternal {
		t.Errorf("expected external escalation, got %s", got)
	}
}

func TestReclassifyNetworkChaosExternalIP(t *testing.T) {
	clusterCIDR := mustCIDR(t, "10.0.0.0/8")
	manifest := simian.FaultManifest{
		Engine:       simian.EngineChaosMesh,
		ResourceKind: "NetworkChaos",
		Spec: map[string]any{
			"target": map[string]any{"value": "8.8.8.8"},
		},
	}
	got := ReclassifyForSpec(manifest, []*net.IPNet{clusterCIDR}, DefaultClusterDomains)
	if got != simian.TierExternal {
		t.Errorf("expected external escalation for 8.8.8.8, got %s", got)
	}
}

func TestReclassifyNetworkChaosInClusterIPStaysNamespace(t *testing.T) {
	clusterCIDR := mustCIDR(t, "10.0.0.0/8")
	manifest := simian.FaultManifest{
		Engine:          simian.EngineChaosMesh,
		ResourceKind:    "NetworkChaos",
		BlastRadiusTier: simian.TierNamespace,
		Spec: map[string]any{
			"target": map[string]any{"value": "10.4.5.6"},
		},
	}
	got := ReclassifyForSpec(manifest, []*net.IPNet{clusterCIDR}, DefaultClusterDomains)
	if got != simian.TierNamespace {
		t.Errorf("in-cluster IP should stay namespace, got %s", got)
	}
}

func TestReclassifyDNSChaosExternalDomain(t *testing.T) {
	manifest := simian.FaultManifest{
		Engine:       simian.EngineChaosMesh,
		ResourceKind: "DNSChaos",
		Spec: map[string]any{
			"patterns": []any{"google.com"},
		},
	}
	got := ReclassifyForSpec(manifest, nil, DefaultClusterDomains)
	if got != simian.TierExternal {
		t.Errorf("expected external for google.com, got %s", got)
	}
}

func TestReclassifyDNSChaosInClusterDomainStays(t *testing.T) {
	manifest := simian.FaultManifest{
		Engine:          simian.EngineChaosMesh,
		ResourceKind:    "DNSChaos",
		BlastRadiusTier: simian.TierNamespace,
		Spec: map[string]any{
			"patterns": []any{"*.svc.cluster.local"},
		},
	}
	got := ReclassifyForSpec(manifest, nil, DefaultClusterDomains)
	if got != simian.TierNamespace {
		t.Errorf("in-cluster pattern should stay namespace, got %s", got)
	}
}

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("bad cidr %q: %v", s, err)
	}
	return n
}
