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
