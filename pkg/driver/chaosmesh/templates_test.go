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

package chaosmesh

import (
	"strings"
	"testing"
)

// TestSpecTemplatesCoverCommonKinds asserts that the canonical Chaos Mesh
// CRDs every M3 install relies on have a non-empty SpecTemplate so the
// planner prompts can render them.
func TestSpecTemplatesCoverCommonKinds(t *testing.T) {
	required := []string{
		"PodChaos",
		"NetworkChaos",
		"StressChaos",
		"IOChaos",
		"TimeChaos",
		"HTTPChaos",
		"DNSChaos",
	}
	for _, k := range required {
		tmpl := SpecTemplateFor(k)
		if tmpl == "" {
			t.Errorf("kind %q: missing SpecTemplate", k)
			continue
		}
		// Each template should at minimum mention the selector shape so the
		// LLM knows where to put namespace + label selector.
		if !strings.Contains(tmpl, "selector") {
			t.Errorf("kind %q: template should reference \"selector\" shape, got:\n%s", k, tmpl)
		}
	}
}

// TestNetworkChaosTemplateForbidsLatencyAction guards against a regression
// where the most common LLM mis-emission ("action: latency" for NetworkChaos,
// which Chaos Mesh rejects) creeps back in. The template must explicitly
// call out that "latency" is not a valid NetworkChaos action.
func TestNetworkChaosTemplateForbidsLatencyAction(t *testing.T) {
	tmpl := SpecTemplateFor("NetworkChaos")
	if !strings.Contains(tmpl, "NEVER \"latency\"") {
		t.Errorf("NetworkChaos template must warn against action=latency, got:\n%s", tmpl)
	}
	if !strings.Contains(tmpl, "\"delay\"") {
		t.Errorf("NetworkChaos template must enumerate \"delay\" as the latency action, got:\n%s", tmpl)
	}
}
