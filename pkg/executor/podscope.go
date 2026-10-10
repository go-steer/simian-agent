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
	"context"
	"fmt"
	"maps"
	"strings"

	"github.com/go-steer/simian-agent/pkg/catalog"
	"github.com/go-steer/simian-agent/pkg/simian"
)

// podNarrowingKeys are the PodSelector fields that pick particular pods out
// of the namespaces a selector names. A selector with none of them matches
// every pod there.
var podNarrowingKeys = []string{
	"labelSelectors", "expressionSelectors", "annotationSelectors",
	"fieldSelectors", "pods", "nodes", "nodeSelectors",
}

// narrowPodSelector makes a Chaos Mesh spec's pod selector select the pods
// its targets name, not the whole namespace.
//
// Targets are what the safety checks judge — eligibility, exclusions, the
// default probes — but the selector is what Chaos Mesh acts on. They came
// apart for a fault built by `simian chaos --workload productcatalogservice
// --spec '{"action":"pod-kill","mode":"one"}'`: the target named the
// workload and its labels were resolved, the selector named only the
// namespace, and Chaos Mesh killed a frontend pod (getting-started
// validation, 2026-10-06). It could as well have been an excluded load
// generator.
//
// So a selector that picks no particular pods is given the targets' labels
// when they are known and agree. When they are not, a target that names a
// workload is refused rather than widened to the namespace; a target that
// names none is taken as meaning the namespace, and refused only where the
// namespace has excluded workloads the fault would reach. It returns the
// labels it narrowed to, for the audit record.
func (e *Executor) narrowPodSelector(ctx context.Context, m *simian.FaultManifest) (map[string]string, error) {
	if m.Engine != simian.EngineChaosMesh || m.Spec == nil {
		return nil, nil
	}
	// An outage kind has no selector to narrow: the driver builds one per
	// workload it finds in the zone or on the node, skipping the excluded
	// ones, and the executor asks it beforehand whether any is left — see
	// checkDriverTargets. Narrowing here would write a selector into a spec
	// that takes none, and the namespace-wide refusal below would refuse
	// every outage in an arena that excludes anything.
	if catalog.IsOutageKind(m.Engine, m.ResourceKind) {
		return nil, nil
	}
	if err := e.checkSecondarySelectors(ctx, m); err != nil {
		return nil, err
	}
	sel, _ := m.Spec["selector"].(map[string]any)
	if sel == nil {
		sel = map[string]any{}
	}
	if selectsPods(sel) {
		return nil, nil // the spec already says which pods
	}

	var labels map[string]string
	named, agree := false, true
	for _, t := range m.Targets {
		if t.Name != "" {
			named = true
		}
		switch {
		case len(t.Labels) == 0:
			agree = false
		case labels == nil:
			labels = t.Labels
		case !maps.Equal(labels, t.Labels):
			agree = false
		}
	}
	if agree && labels != nil {
		ls := make(map[string]any, len(labels))
		for k, v := range labels {
			ls[k] = v
		}
		sel["labelSelectors"] = ls
		m.Spec["selector"] = sel
		return labels, nil
	}

	if named {
		if e.workloads == nil {
			return nil, nil // no resolver configured: judged on what was given
		}
		return nil, simian.NewExecutorError(simian.StageSafety, simian.ReasonSchemaInvalid,
			"spec.selector selects no particular pods and the targets' pod labels could not be determined, so the fault would reach every pod in the namespace; add spec.selector.labelSelectors", nil)
	}
	for _, ns := range m.TargetNamespaces() {
		excluded, err := e.elig.ExcludedWorkloads(ctx, ns)
		if err != nil {
			return nil, simian.NewExecutorError(simian.StageSafety, simian.ReasonWorkloadExcluded, "exclusion lookup failed", err)
		}
		if len(excluded) > 0 {
			return nil, simian.NewExecutorError(simian.StageSafety, simian.ReasonWorkloadExcluded,
				fmt.Sprintf("spec.selector matches every pod in namespace %q, including excluded workloads %s; name a target workload or add spec.selector.labelSelectors", ns, strings.Join(excluded, ", ")), nil)
		}
	}
	return nil, nil
}

func emptyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case map[string]any:
		return len(x) == 0
	case []any:
		return len(x) == 0
	case string:
		return x == ""
	}
	return false
}

// checkSecondarySelectors refuses a fault whose other pod selectors — a
// NetworkChaos spec.target.selector picking the far side of the link, say —
// match every pod in a namespace with excluded workloads (#198).
//
// Targets describe only the primary side, so there are no labels to narrow
// these to; a namespace-wide far side is a legitimate partition ("cut edge
// off from everything"), and is allowed where nothing is excluded. Where
// something is, the exclusion would be bypassed silently, which is the one
// thing it exists to prevent.
func (e *Executor) checkSecondarySelectors(ctx context.Context, m *simian.FaultManifest) error {
	for _, site := range findSelectorSites(m.Spec) {
		if site.path == "spec.selector" || selectsPods(site.node) {
			continue
		}
		namespaces := site.namespaces
		if len(namespaces) == 0 {
			namespaces = m.TargetNamespaces()
		}
		for _, ns := range namespaces {
			excluded, err := e.elig.ExcludedWorkloads(ctx, ns)
			if err != nil {
				return simian.NewExecutorError(simian.StageSafety, simian.ReasonWorkloadExcluded, "exclusion lookup failed", err)
			}
			if len(excluded) > 0 {
				return simian.NewExecutorError(simian.StageSafety, simian.ReasonWorkloadExcluded,
					fmt.Sprintf("%s matches every pod in namespace %q, including excluded workloads %s; add labelSelectors to it", site.path, ns, strings.Join(excluded, ", ")), nil)
			}
		}
	}
	return nil
}

// selectsPods reports whether a selector picks particular pods out of its
// namespaces.
func selectsPods(sel map[string]any) bool {
	for _, k := range podNarrowingKeys {
		if v, ok := sel[k]; ok && !emptyValue(v) {
			return true
		}
	}
	return false
}
