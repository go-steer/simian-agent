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

package kubestate

import (
	"fmt"
	"strings"
	"testing"

	"github.com/go-steer/simian-agent/pkg/catalog"
	"github.com/go-steer/simian-agent/pkg/scenario"
	"github.com/go-steer/simian-agent/pkg/simian"
)

// Every kube-state fault in every shipped scenario pack must synthesize.
//
// The packs live in pkg/scenario and are validated there, but that validation
// is about the *scenario*: an ID, a prompt that does not leak, an expectation
// that names a kind. Nothing in it asks the engine whether the spec makes
// sense. A pack could ship `exit_code: 0`, or an `allocate_mb` inside its own
// limit, or a `min_available` the budget would permit a disruption under, and
// every test in that package would pass — the failure would surface as an Apply
// error on a real cluster, in the middle of a scored run, after the namespace
// had been created.
//
// This runs the same validation Apply does, minus the API calls and minus the
// finishers that wait on a cluster: mode, kind, name, replica bounds, and the
// builder's own checks on the spec it was handed.
func TestEveryShippedScenarioFaultSynthesizes(t *testing.T) {
	for _, packName := range scenario.BuiltinPacks {
		pack := scenario.MustBuiltin(packName)
		for _, s := range pack.Scenarios {
			for i, m := range s.Faults {
				if m.Engine != simian.EngineKubeState {
					continue
				}
				if err := synthesizeForTest(m); err != nil {
					t.Errorf("pack %s, scenario %s, fault %d (%s): %v",
						packName, s.ID, i, m.ResourceKind, err)
				}
			}
		}
	}
}

// The gate a pack's fault gets is the default one, and a default gate is built
// from the manifest before Apply has created anything — the workload name in
// its label selector is derived, not observed. A scenario that named its
// workload in a way the gate could not reconstruct would apply cleanly and then
// gate against pods that do not exist.
func TestEveryShippedScenarioFaultIsGatedOnItsOwnWorkload(t *testing.T) {
	for _, packName := range scenario.BuiltinPacks {
		for _, s := range scenario.MustBuiltin(packName).Scenarios {
			for i, m := range s.Faults {
				if m.Engine != simian.EngineKubeState {
					continue
				}
				name := catalog.KubeStateWorkloadName(m.ResourceKind, optString(m.Spec, "name", ""), m.UID)
				probes := catalog.DefaultProbes(m)
				if len(probes) == 0 {
					t.Errorf("pack %s, scenario %s, fault %d (%s): no default probes",
						packName, s.ID, i, m.ResourceKind)
					continue
				}
				for _, p := range probes {
					if p.Mode != simian.ProbeModeSettle {
						continue
					}
					if !probeMentions(p, name) {
						t.Errorf("pack %s, scenario %s, fault %d (%s): probe %q does not select %q: %+v",
							packName, s.ID, i, m.ResourceKind, p.Name, name, p.Spec)
					}
				}
			}
		}
	}
}

// synthesizeForTest mirrors Apply's validation without touching a cluster.
func synthesizeForTest(m simian.FaultManifest) error {
	if err := checkMode(m.Spec); err != nil {
		return err
	}
	build, ok := builders[m.ResourceKind]
	if !ok {
		return fmt.Errorf("unsupported kind %q", m.ResourceKind)
	}
	name, err := workloadName(m.ResourceKind, m.Spec, m.UID)
	if err != nil {
		return err
	}
	replicas, err := optInt(m.Spec, "replicas", catalog.KubeStateDefaultReplicas(m.ResourceKind))
	if err != nil {
		return err
	}
	if replicas < 1 || replicas > maxReplicas {
		return fmt.Errorf("spec.replicas must be between 1 and %d, got %d", maxReplicas, replicas)
	}
	_, err = build(synthesis{
		name:      name,
		namespace: m.Targets[0].Namespace,
		replicas:  int32(replicas),
		spec:      m.Spec,
		now:       testNow,
	})
	return err
}

// probeMentions reports whether a probe's spec names the workload anywhere —
// in a label selector, a resource name, or a log selector. Which field it lands
// in is per-kind; that it is there at all is the invariant.
func probeMentions(p simian.ProbeSpec, name string) bool {
	for _, v := range p.Spec {
		if s, ok := v.(string); ok && strings.Contains(s, name) {
			return true
		}
	}
	return false
}
