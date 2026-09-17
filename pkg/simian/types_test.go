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

func TestPlanBudgetUnmarshalMinCooldown(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want time.Duration
		err  bool
	}{
		{"string-seconds", `{"min_cooldown":"30s"}`, 30 * time.Second, false},
		{"string-minutes", `{"min_cooldown":"2m"}`, 2 * time.Minute, false},
		{"int-nanoseconds", `{"min_cooldown":30000000000}`, 30 * time.Second, false},
		{"omitted", `{"max_concurrent_faults":1}`, 0, false},
		{"null", `{"min_cooldown":null}`, 0, false},
		{"bad-string", `{"min_cooldown":"not-a-duration"}`, 0, true},
		{"bad-type", `{"min_cooldown":true}`, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var b PlanBudget
			err := json.Unmarshal([]byte(tc.in), &b)
			if tc.err {
				if err == nil {
					t.Fatalf("expected error, got %+v", b)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if b.MinCooldown != tc.want {
				t.Fatalf("got %v want %v", b.MinCooldown, tc.want)
			}
		})
	}
}

func TestPlanBudgetUnmarshalPreservesOtherFields(t *testing.T) {
	in := `{"max_concurrent_faults":3,"min_cooldown":"45s","max_severity_tier":"namespace"}`
	var b PlanBudget
	if err := json.Unmarshal([]byte(in), &b); err != nil {
		t.Fatal(err)
	}
	if b.MaxConcurrentFaults != 3 {
		t.Errorf("MaxConcurrentFaults: got %d want 3", b.MaxConcurrentFaults)
	}
	if b.MinCooldown != 45*time.Second {
		t.Errorf("MinCooldown: got %v want 45s", b.MinCooldown)
	}
	if b.MaxSeverityTier != "namespace" {
		t.Errorf("MaxSeverityTier: got %q want %q", b.MaxSeverityTier, "namespace")
	}
}

// A manifest may declare targets in more than one namespace. Everything that
// accounts for blast radius has to see all of them, so the two accessors that
// answer "which namespaces" and "is this one of them" are the contract the
// cooldown, the lease lookup, the history and the arena teardown check all
// depend on.
func TestTargetNamespacesCoversEveryTargetNotJustTheFirst(t *testing.T) {
	cases := []struct {
		name    string
		targets []TargetRef
		want    []string
	}{
		{"none", nil, []string{}},
		{"one", []TargetRef{{Namespace: "ns-a"}}, []string{"ns-a"}},
		{
			"two, sorted",
			[]TargetRef{{Namespace: "ns-b"}, {Namespace: "ns-a"}},
			[]string{"ns-a", "ns-b"},
		},
		{
			"deduped across pods in the same namespace",
			[]TargetRef{{Namespace: "ns-a", Name: "p1"}, {Namespace: "ns-a", Name: "p2"}},
			[]string{"ns-a"},
		},
		{
			"an unnamespaced target is not a namespace",
			[]TargetRef{{Namespace: "", Name: "node-1"}, {Namespace: "ns-a"}},
			[]string{"ns-a"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := FaultManifest{Targets: tc.targets}
			got := m.TargetNamespaces()
			if len(got) != len(tc.want) {
				t.Fatalf("TargetNamespaces() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("TargetNamespaces() = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestTargetsNamespaceMatchesASecondTargetAndTreatsEmptyAsNoFilter(t *testing.T) {
	m := FaultManifest{Targets: []TargetRef{
		{Namespace: "ns-a", Name: "frontend"},
		{Namespace: "ns-b", Name: "payments"},
	}}
	for _, tc := range []struct {
		ns   string
		want bool
	}{
		{"ns-a", true},
		{"ns-b", true}, // the one Targets[0] alone would have missed
		{"ns-c", false},
		{"", true}, // "no filter" — every call site spells it this way
	} {
		if got := m.TargetsNamespace(tc.ns); got != tc.want {
			t.Errorf("TargetsNamespace(%q) = %v, want %v", tc.ns, got, tc.want)
		}
	}
}
