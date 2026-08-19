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
	"sort"

	"github.com/go-steer/simian-agent/pkg/simian"
)

// Spec-level namespace scoping.
//
// FaultManifest.Targets is the manifest's declared blast radius, and
// validateSafety checks every target namespace against arena eligibility.
// That check is not sufficient on its own: the Chaos Mesh driver hands
// FaultManifest.Spec to the API server verbatim, and a Chaos Mesh PodSelector
// carries its own namespace scope. A manifest may therefore name an eligible
// namespace in targets and still select pods in kube-system via
// spec.selector.namespaces — the CR is created in the target namespace but
// the controller reconciles against whatever the selector names.
//
// This file closes that gap. Every PodSelector-shaped node reachable from the
// spec is located and:
//
//   - namespaces it names explicitly must all be arena-eligible, or the
//     manifest is rejected;
//   - if it names none, it is narrowed in place to the manifest's target
//     namespaces, so scope never falls back to the controller's default.
//
// The walk is generic rather than keyed on resource kind, per R-FAULT-01 /
// R-FAULT-07: there are no per-fault-type Go wrappers, so there is nothing
// that knows a NetworkChaos has a second PodSelector under spec.target or
// that an IOChaos has a spec.containerSelector. Over-triggering on a
// selector-shaped node that is not a selector is fail-closed: the worst case
// is a narrowing to the namespace the fault was already aimed at.

// selectorFieldNames are keys whose map value is a selector regardless of what
// it contains — this catches the empty-selector case (`selector: {}`) that
// selectorMarkerKeys alone would miss.
var selectorFieldNames = map[string]bool{
	"selector":          true,
	"podSelector":       true,
	"containerSelector": true,
}

// selectorMarkerKeys are the fields of a Chaos Mesh PodSelector. A map node
// carrying any of them is treated as a selector wherever it appears, which is
// what makes the walk work for selectors nested under keys this file does not
// know about.
var selectorMarkerKeys = map[string]bool{
	"namespaces":          true,
	"labelSelectors":      true,
	"expressionSelectors": true,
	"annotationSelectors": true,
	"fieldSelectors":      true,
	"podPhaseSelectors":   true,
	"nodeSelectors":       true,
	"nodes":               true,
	"pods":                true,
}

// opaqueMapFields hold user-supplied keys (label names, annotation keys,
// namespace names) rather than schema fields. Recursing into them would let a
// label literally named "namespaces" masquerade as a selector, so the walk
// stops at them.
var opaqueMapFields = map[string]bool{
	"labelSelectors":      true,
	"annotationSelectors": true,
	"fieldSelectors":      true,
	"nodeSelectors":       true,
	"pods":                true,
}

// specScopeExempt lists engines whose driver builds its own API objects from
// FaultManifest.Targets and never passes Spec through as a selector — for
// those, Targets is the whole story. Engines are checked unless listed here,
// so a new driver is covered by default.
var specScopeExempt = map[simian.Engine]bool{
	simian.EngineNetworkPolicy: true,
	simian.EngineEnvoyFault:    true,
}

// selectorSite is one selector-shaped node found in a spec.
type selectorSite struct {
	path       string         // dotted path from "spec", for error messages
	node       map[string]any // the live node, mutated when narrowing
	namespaces []string       // namespaces the node names, deduped and sorted
}

// findSelectorSites walks spec and returns every selector-shaped node in it,
// in a deterministic order.
func findSelectorSites(spec map[string]any) []selectorSite {
	var sites []selectorSite
	walkForSelectors(spec, "spec", "", &sites)
	return sites
}

func walkForSelectors(node any, path, key string, out *[]selectorSite) {
	switch n := node.(type) {
	case map[string]any:
		if selectorFieldNames[key] || hasSelectorMarker(n) {
			*out = append(*out, selectorSite{
				path:       path,
				node:       n,
				namespaces: namespacesIn(n),
			})
		}
		for _, k := range sortedKeys(n) {
			if opaqueMapFields[k] {
				continue
			}
			walkForSelectors(n[k], path+"."+k, k, out)
		}
	case []any:
		for i, v := range n {
			walkForSelectors(v, fmt.Sprintf("%s[%d]", path, i), "", out)
		}
	}
}

// hasSelectorMarker reports whether the node carries a PodSelector field with
// a value of the shape that field actually has. The type check keeps a
// same-named label or annotation key from being mistaken for a selector.
func hasSelectorMarker(n map[string]any) bool {
	for k, v := range n {
		if !selectorMarkerKeys[k] {
			continue
		}
		switch v.(type) {
		case []any, map[string]any:
			return true
		}
	}
	return false
}

// namespacesIn collects the namespaces a selector node names. There are two
// sources: the `namespaces` list, and the keys of the `pods` map, which is
// itself keyed by namespace.
func namespacesIn(n map[string]any) []string {
	seen := map[string]bool{}
	if arr, ok := n["namespaces"].([]any); ok {
		for _, v := range arr {
			if s, ok := v.(string); ok && s != "" {
				seen[s] = true
			}
		}
	}
	if pods, ok := n["pods"].(map[string]any); ok {
		for ns := range pods {
			if ns != "" {
				seen[ns] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for ns := range seen {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

// validateSpecNamespaceScope rejects a manifest whose spec selects namespaces
// outside the arena, and narrows any selector that names no namespace to the
// manifest's target namespaces. It returns the dotted paths that were
// narrowed so the caller can record them on the validated audit event.
//
// Callers must have validated target-namespace eligibility first: narrowing
// copies target namespaces into the spec, and an unchecked target would
// launder an ineligible namespace into the selector.
func (e *Executor) validateSpecNamespaceScope(ctx context.Context, m *simian.FaultManifest) ([]string, error) {
	if specScopeExempt[m.Engine] || len(m.Spec) == 0 {
		return nil, nil
	}

	targetNS := targetNamespaces(*m)
	if len(targetNS) == 0 {
		return nil, simian.NewExecutorError(simian.StageSafety, simian.ReasonNamespaceNotEligible,
			"manifest has no target namespace to scope the spec selector to", nil)
	}

	var narrowed []string
	eligible := map[string]bool{}
	for _, site := range findSelectorSites(m.Spec) {
		if len(site.namespaces) == 0 {
			site.node["namespaces"] = toAnySlice(targetNS)
			narrowed = append(narrowed, site.path)
			continue
		}
		for _, ns := range site.namespaces {
			if _, done := eligible[ns]; !done {
				ok, err := e.elig.IsEligible(ctx, ns)
				if err != nil {
					return nil, simian.NewExecutorError(simian.StageSafety, simian.ReasonNamespaceNotEligible,
						fmt.Sprintf("eligibility lookup failed for %s named at %s", ns, site.path), err)
				}
				eligible[ns] = ok
			}
			if !eligible[ns] {
				return nil, simian.NewExecutorError(simian.StageSafety, simian.ReasonNamespaceNotEligible,
					fmt.Sprintf("%s names namespace %q, which is not eligible for chaos", site.path, ns), nil)
			}
		}
	}
	return narrowed, nil
}

// targetNamespaces returns the deduped, sorted namespaces named by the
// manifest's targets.
func targetNamespaces(m simian.FaultManifest) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range m.Targets {
		if t.Namespace == "" || seen[t.Namespace] {
			continue
		}
		seen[t.Namespace] = true
		out = append(out, t.Namespace)
	}
	sort.Strings(out)
	return out
}

func toAnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
