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
	"slices"
	"sort"
	"strings"

	"github.com/go-steer/simian-agent/pkg/simian"
)

// Outage kinds: composite Chaos Mesh faults that are not CRDs of their own.
// The driver builds one PodChaos per arena workload with pods in the zone or
// on the node, so their spec names a place, not pods (#258).
const (
	// ChaosMeshZoneOutage takes down the arena's pods in one zone.
	ChaosMeshZoneOutage = "ZoneOutage"
	// ChaosMeshNodeOutage takes down the arena's pods on one node.
	ChaosMeshNodeOutage = "NodeOutage"

	// ZoneLabel is the well-known node label a ZoneOutage's zone is read from.
	ZoneLabel = "topology.kubernetes.io/zone"

	// OutageActionPodFailure is the only action an outage takes today: the
	// pods stay scheduled and stop running, as in the first minutes of a
	// real zone outage, before anything is evicted.
	OutageActionPodFailure = "pod-failure"
)

// IsOutageKind reports whether engine+kind is a composite outage, which picks
// its own pods when applied and carries no selector.
func IsOutageKind(engine simian.Engine, kind string) bool {
	return engine == simian.EngineChaosMesh && (kind == ChaosMeshZoneOutage || kind == ChaosMeshNodeOutage)
}

// outagePlaceField is the spec field naming where an outage kind acts.
func outagePlaceField(kind string) string {
	if kind == ChaosMeshNodeOutage {
		return "node"
	}
	return "zone"
}

// OutagePlace returns the place an outage manifest names — the zone or the
// node — and the field it is in.
func OutagePlace(m simian.FaultManifest) (field, value string) {
	field = outagePlaceField(m.ResourceKind)
	value, _ = m.Spec[field].(string)
	return field, strings.TrimSpace(value)
}

// OutageAction returns the outage's action, defaulted.
func OutageAction(spec map[string]any) string {
	if a, _ := spec["action"].(string); a != "" {
		return a
	}
	return OutageActionPodFailure
}

// CheckOutageManifest refuses an outage manifest whose shape the driver
// cannot build from: a missing zone or node, an action other than
// pod-failure, a field the kind does not have, or targets in more than one
// namespace. It is strict for the reason the Chaos Mesh driver creates with
// strict field validation: a misspelled field silently dropped would run a
// fault nobody wrote. Nil for any other kind.
func CheckOutageManifest(m simian.FaultManifest) error {
	if !IsOutageKind(m.Engine, m.ResourceKind) {
		return nil
	}
	field, place := OutagePlace(m)
	allowed := []string{field, "action"}
	var unknown []string
	for k := range m.Spec {
		if !slices.Contains(allowed, k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("%s spec has unknown field(s) %s; it takes only %q and \"action\" — it picks the pods itself",
			m.ResourceKind, strings.Join(unknown, ", "), field)
	}
	if place == "" {
		return fmt.Errorf("%s spec needs %q: the %s whose arena pods go down", m.ResourceKind, field, field)
	}
	if a := OutageAction(m.Spec); a != OutageActionPodFailure {
		return fmt.Errorf("%s action %q is not supported; the only action is %q", m.ResourceKind, a, OutageActionPodFailure)
	}
	if ns := m.TargetNamespaces(); len(ns) != 1 {
		return fmt.Errorf("%s acts on one arena; targets name %d namespaces (%s)", m.ResourceKind, len(ns), strings.Join(ns, ", "))
	}
	return nil
}
