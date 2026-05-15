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
	"net"
	"time"

	"github.com/go-steer/simian-agent/pkg/catalog"
	"github.com/go-steer/simian-agent/pkg/simian"
)

// Config governs the executor's safety policy and budget caps. Production
// deployments populate this from Helm values + ConfigMap; tests can construct
// it directly.
type Config struct {
	// DurationCeiling is the absolute maximum any single fault may declare.
	// Manifests with Duration above this are rejected. Default 15 min per
	// requirements R-FAULT-04.
	DurationCeiling time.Duration

	// PermittedTiers is the set of blast-radius tiers allowed by this
	// installation. Default v1 policy permits "namespace" and "node";
	// "external" requires explicit opt-in.
	PermittedTiers map[simian.BlastRadiusTier]bool

	// MaxConcurrentFaults caps the total number of leased faults across all
	// namespaces. 0 disables the cap.
	MaxConcurrentFaults int

	// MinCooldown is the minimum gap between consecutive faults applied to
	// the same namespace. 0 disables.
	MinCooldown time.Duration

	// ClusterPodCIDRs / ClusterServiceCIDRs are used by per-spec re-classification
	// to detect external IP targets in NetworkChaos.
	ClusterPodCIDRs     []*net.IPNet
	ClusterServiceCIDRs []*net.IPNet

	// ClusterDomains is the in-cluster DNS suffix list (e.g. cluster.local).
	ClusterDomains []string
}

// DefaultConfig returns the v1 default policy.
func DefaultConfig() Config {
	return Config{
		DurationCeiling: 15 * time.Minute,
		PermittedTiers: map[simian.BlastRadiusTier]bool{
			simian.TierNamespace: true,
			simian.TierNode:      true,
		},
		MaxConcurrentFaults: 0,
		MinCooldown:         0,
		ClusterDomains:      catalog.DefaultClusterDomains,
	}
}

// AllCIDRs returns pod + service CIDRs combined for in-cluster IP checks.
func (c Config) AllCIDRs() []*net.IPNet {
	out := make([]*net.IPNet, 0, len(c.ClusterPodCIDRs)+len(c.ClusterServiceCIDRs))
	out = append(out, c.ClusterPodCIDRs...)
	out = append(out, c.ClusterServiceCIDRs...)
	return out
}
