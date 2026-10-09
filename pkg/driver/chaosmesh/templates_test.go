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
	"encoding/json"
	"strings"
	"testing"
)

// exampleSpec finds a template's example the way the web UI does: the first
// balanced {…} block that parses as a JSON object, skipping prose fragments
// such as StressChaos's `"stressors": {"cpu": {...}}`.
func exampleSpec(tpl string) (map[string]any, bool) {
	for start := strings.Index(tpl, "{"); start >= 0; {
		depth := 0
		for i := start; i < len(tpl); i++ {
			switch tpl[i] {
			case '{':
				depth++
			case '}':
				depth--
			}
			if depth == 0 {
				var spec map[string]any
				if json.Unmarshal([]byte(tpl[start:i+1]), &spec) == nil {
					return spec, true
				}
				break
			}
		}
		next := strings.Index(tpl[start+1:], "{")
		if next < 0 {
			break
		}
		start += next + 1
	}
	return nil, false
}

// Every template the planner and the web UI start from must carry an
// example spec with a mode: on simian-2 a StressChaos went out as {} because
// the UI could not find one, and Chaos Mesh refused it for having no mode.
func TestEveryTemplateHasAnExampleSpecWithAMode(t *testing.T) {
	for kind, tpl := range specTemplates {
		spec, ok := exampleSpec(tpl)
		if !ok {
			t.Errorf("%s: no example spec the UI can start from", kind)
			continue
		}
		if _, ok := spec["mode"]; !ok {
			t.Errorf("%s: example spec has no mode: %v", kind, spec)
		}
	}
	if spec, _ := exampleSpec(specTemplates["StressChaos"]); spec["stressors"] == nil {
		t.Errorf("StressChaos: the example was not the one with stressors: %v", spec)
	}
}
