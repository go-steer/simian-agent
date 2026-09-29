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

package simian

// AuditRecord returns what the manifest does and where, shaped for an audit
// payload: engine, kind, targets, spec and duration.
//
// The audit trail is what a triaging team is graded against, so it has to
// answer "which fault, on which objects, with what spec" on its own. Before
// this, a trial's log said a PodChaos ran and nothing else; learning it had
// killed bank/accounts-db-0 meant joining CR names against Kubernetes Events,
// which are gone within the hour.
//
// The spec is deep-copied. The executor narrows unscoped selectors in place
// after the manifest is received, and a record taken before that has to keep
// saying what was submitted, whatever an auditor does with the map later.
func (m FaultManifest) AuditRecord() map[string]any {
	targets := make([]any, 0, len(m.Targets))
	for _, t := range m.Targets {
		target := map[string]any{"namespace": t.Namespace}
		if t.Kind != "" {
			target["kind"] = t.Kind
		}
		if t.Name != "" {
			target["name"] = t.Name
		}
		if len(t.Labels) > 0 {
			labels := make(map[string]any, len(t.Labels))
			for k, v := range t.Labels {
				labels[k] = v
			}
			target["labels"] = labels
		}
		targets = append(targets, target)
	}
	rec := map[string]any{
		"engine":   string(m.Engine),
		"kind":     m.ResourceKind,
		"targets":  targets,
		"spec":     copyJSONValue(m.Spec),
		"duration": m.Duration.String(),
	}
	if m.BlastRadiusTier != "" {
		rec["blast_radius_tier"] = string(m.BlastRadiusTier)
	}
	return rec
}

// copyJSONValue deep-copies the maps and slices of a decoded JSON value.
// Anything else is returned as is: scalars are values already, and a spec
// holding some other type is the caller's to explain, not this copy's to
// reject.
func copyJSONValue(v any) any {
	switch v := v.(type) {
	case map[string]any:
		if v == nil {
			return map[string]any{}
		}
		out := make(map[string]any, len(v))
		for k, e := range v {
			out[k] = copyJSONValue(e)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = copyJSONValue(e)
		}
		return out
	default:
		return v
	}
}
