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

import (
	"encoding/json"
	"testing"
	"time"
)

func TestAuditRecordSaysWhatAndWhere(t *testing.T) {
	m := FaultManifest{
		Engine:          EngineChaosMesh,
		ResourceKind:    "PodChaos",
		Spec:            map[string]any{"action": "pod-kill", "selector": map[string]any{"labelSelectors": map[string]any{"app": "accounts-db"}}},
		Targets:         []TargetRef{{Namespace: "bank", Kind: "StatefulSet", Name: "accounts-db", Labels: map[string]string{"app": "accounts-db"}}},
		Duration:        90 * time.Second,
		BlastRadiusTier: TierNamespace,
	}
	b, err := json.Marshal(m.AuditRecord())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"blast_radius_tier":"namespace","duration":"1m30s","engine":"chaos-mesh","kind":"PodChaos",` +
		`"spec":{"action":"pod-kill","selector":{"labelSelectors":{"app":"accounts-db"}}},` +
		`"targets":[{"kind":"StatefulSet","labels":{"app":"accounts-db"},"name":"accounts-db","namespace":"bank"}]}`
	if string(b) != want {
		t.Errorf("record =\n  %s\nwant\n  %s", b, want)
	}
}

// The executor narrows selectors in place after taking the "as submitted"
// record. The record must not follow.
func TestAuditRecordDoesNotMoveWhenTheSpecDoes(t *testing.T) {
	sel := map[string]any{}
	m := FaultManifest{Spec: map[string]any{"selector": sel, "ports": []any{"80"}}}
	rec := m.AuditRecord()

	sel["namespaces"] = []any{"bank"}
	m.Spec["ports"].([]any)[0] = "443"

	spec := rec["spec"].(map[string]any)
	if _, moved := spec["selector"].(map[string]any)["namespaces"]; moved {
		t.Error("record picked up a selector narrowed after it was taken")
	}
	if got := spec["ports"].([]any)[0]; got != "80" {
		t.Errorf("record ports[0] = %v, want 80", got)
	}
}

func TestAuditRecordOfAnEmptyManifestIsStillAList(t *testing.T) {
	rec := FaultManifest{}.AuditRecord()
	if targets, ok := rec["targets"].([]any); !ok || targets == nil {
		t.Errorf("targets = %#v, want an empty list, so a missing target shows as [] and not null", rec["targets"])
	}
}
