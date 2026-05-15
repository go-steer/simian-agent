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
	"fmt"
	"sort"

	"github.com/go-steer/simian-agent/pkg/simian"
)

// validateStepDAG checks that an AttackPlan's steps form a valid DAG:
//   - Each PlanStep.Order must be unique within the plan.
//   - Every DependsOn entry must reference a real step's Order.
//   - There must be no cycles (a step cannot transitively depend on itself).
//   - Self-loops (DependsOn referring to the step's own Order) are rejected.
//
// On success returns nil. On failure returns a descriptive error so the
// LLM can be told what to fix on the retry.
func validateStepDAG(steps []simian.PlanStep) error {
	if len(steps) == 0 {
		return fmt.Errorf("plan has no steps")
	}

	orders := make(map[int]bool, len(steps))
	for _, s := range steps {
		if orders[s.Order] {
			return fmt.Errorf("duplicate step order %d", s.Order)
		}
		orders[s.Order] = true
	}

	for _, s := range steps {
		for _, dep := range s.DependsOn {
			if dep == s.Order {
				return fmt.Errorf("step %d depends on itself", s.Order)
			}
			if !orders[dep] {
				return fmt.Errorf("step %d depends on unknown step %d", s.Order, dep)
			}
		}
	}

	if _, err := PlanLayers(steps); err != nil {
		return err
	}
	return nil
}

// PlanLayers groups step orders into topological layers: layer 0 contains
// steps with no DependsOn, layer N contains steps whose dependencies all
// resolve in layers < N. Within a layer, steps may execute in parallel
// (subject to the loop's MaxConcurrentFaults cap).
//
// Returns an error if the steps contain a cycle.
func PlanLayers(steps []simian.PlanStep) ([][]int, error) {
	if len(steps) == 0 {
		return nil, nil
	}

	// Index by Order so we can look up DependsOn quickly.
	indeg := make(map[int]int, len(steps))
	deps := make(map[int][]int, len(steps))
	for _, s := range steps {
		indeg[s.Order] = len(s.DependsOn)
		deps[s.Order] = append([]int(nil), s.DependsOn...)
	}

	// Reverse adjacency: dep → dependents.
	dependents := map[int][]int{}
	for order, ds := range deps {
		for _, d := range ds {
			dependents[d] = append(dependents[d], order)
		}
	}

	var layers [][]int
	remaining := len(steps)
	for remaining > 0 {
		var layer []int
		for order, n := range indeg {
			if n == 0 {
				layer = append(layer, order)
			}
		}
		if len(layer) == 0 {
			return nil, fmt.Errorf("plan contains a dependency cycle")
		}
		sort.Ints(layer)
		for _, order := range layer {
			delete(indeg, order)
			for _, dep := range dependents[order] {
				if _, ok := indeg[dep]; ok {
					indeg[dep]--
				}
			}
			remaining--
		}
		layers = append(layers, layer)
	}
	return layers, nil
}
