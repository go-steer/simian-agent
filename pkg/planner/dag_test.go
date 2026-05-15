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

package planner

import (
	"reflect"
	"strings"
	"testing"

	"github.com/go-steer/simian-agent/pkg/simian"
)

func steps(orders ...int) []simian.PlanStep {
	out := make([]simian.PlanStep, 0, len(orders))
	for _, o := range orders {
		out = append(out, simian.PlanStep{Order: o})
	}
	return out
}

func step(order int, deps ...int) simian.PlanStep {
	return simian.PlanStep{Order: order, DependsOn: deps}
}

func TestValidateStepDAG_Empty(t *testing.T) {
	if err := validateStepDAG(nil); err == nil {
		t.Fatal("expected error for empty plan")
	}
}

func TestValidateStepDAG_DuplicateOrder(t *testing.T) {
	err := validateStepDAG(steps(1, 1))
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate-order error, got %v", err)
	}
}

func TestValidateStepDAG_SelfLoop(t *testing.T) {
	err := validateStepDAG([]simian.PlanStep{step(1, 1)})
	if err == nil || !strings.Contains(err.Error(), "depends on itself") {
		t.Fatalf("expected self-loop error, got %v", err)
	}
}

func TestValidateStepDAG_UnknownDependency(t *testing.T) {
	err := validateStepDAG([]simian.PlanStep{step(1), step(2, 99)})
	if err == nil || !strings.Contains(err.Error(), "unknown step") {
		t.Fatalf("expected unknown-step error, got %v", err)
	}
}

func TestValidateStepDAG_Cycle(t *testing.T) {
	// 1 → 2 → 3 → 1
	err := validateStepDAG([]simian.PlanStep{step(1, 3), step(2, 1), step(3, 2)})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected cycle error, got %v", err)
	}
}

func TestPlanLayers_Linear(t *testing.T) {
	layers, err := PlanLayers([]simian.PlanStep{step(1), step(2, 1), step(3, 2)})
	if err != nil {
		t.Fatalf("PlanLayers: %v", err)
	}
	want := [][]int{{1}, {2}, {3}}
	if !reflect.DeepEqual(layers, want) {
		t.Errorf("layers=%v, want %v", layers, want)
	}
}

func TestPlanLayers_FullyParallel(t *testing.T) {
	layers, err := PlanLayers(steps(1, 2, 3))
	if err != nil {
		t.Fatalf("PlanLayers: %v", err)
	}
	want := [][]int{{1, 2, 3}}
	if !reflect.DeepEqual(layers, want) {
		t.Errorf("layers=%v, want %v", layers, want)
	}
}

func TestPlanLayers_Diamond(t *testing.T) {
	// 1 → {2,3} → 4
	layers, err := PlanLayers([]simian.PlanStep{step(1), step(2, 1), step(3, 1), step(4, 2, 3)})
	if err != nil {
		t.Fatalf("PlanLayers: %v", err)
	}
	want := [][]int{{1}, {2, 3}, {4}}
	if !reflect.DeepEqual(layers, want) {
		t.Errorf("layers=%v, want %v", layers, want)
	}
}
