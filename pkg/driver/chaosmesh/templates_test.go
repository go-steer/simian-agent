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
	"os"
	"path/filepath"
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

// crdSpecFields is testdata/crd-spec-fields-v2.8.4.json: for each kind with a
// template, the top-level spec properties and required fields from Chaos
// Mesh's own CRD schema (manifests/crd.yaml at v2.8.4), inlined structs
// already folded in. It is a fixture for checking the templates, not a copy
// the driver validates against — that is the API server's job.
type crdSpecFields struct {
	Fields   []string `json:"fields"`
	Required []string `json:"required"`
}

func loadCRDSpecFields(t *testing.T) map[string]crdSpecFields {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "crd-spec-fields-v2.8.4.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]crdSpecFields
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// templateExample decodes the example spec in a template: the JSON object
// starting on the first line that opens with "{". The lines around it are
// prose for the planner.
func templateExample(t *testing.T, kind string) map[string]any {
	t.Helper()
	tmpl := SpecTemplateFor(kind)
	i := strings.Index(tmpl, "\n{")
	if i < 0 {
		t.Fatalf("kind %q: template has no line starting with \"{\":\n%s", kind, tmpl)
	}
	var obj map[string]any
	if err := json.NewDecoder(strings.NewReader(tmpl[i+1:])).Decode(&obj); err != nil {
		t.Fatalf("kind %q: example spec does not parse: %v\n%s", kind, err, tmpl)
	}
	return obj
}

// The planner copies these examples nearly verbatim, so a template that does
// not match the CRD is a fault kind that fails every time it is chosen. On
// the 2026-09-30 GKE trial HTTPChaos failed 9 of 9 at apply: its template
// taught an "action" field HTTPChaos has never had (#159). The same check
// found JVMChaos teaching "latencyDuration" for "latency" and
// PhysicalMachineChaos leaving out the required "mode".
//
// When a new Chaos Mesh minor changes a kind's spec, regenerate the fixture
// from that release's manifests/crd.yaml and let this say which templates
// broke.
func TestEveryTemplateExampleIsASpecTheCRDAccepts(t *testing.T) {
	crd := loadCRDSpecFields(t)
	for kind := range specTemplates {
		want, ok := crd[kind]
		if !ok {
			t.Errorf("kind %q has a template but no entry in the CRD fixture", kind)
			continue
		}
		obj := templateExample(t, kind)
		known := map[string]bool{}
		for _, f := range want.Fields {
			known[f] = true
		}
		for f := range obj {
			if !known[f] {
				t.Errorf("kind %q: example sets %q, which the v2.8.4 CRD does not have; strict apply rejects it", kind, f)
			}
		}
		for _, f := range want.Required {
			if _, ok := obj[f]; !ok {
				t.Errorf("kind %q: example leaves out required field %q", kind, f)
			}
		}
	}
}

// The DNSChaos template taught "*.<service>.svc.cluster.local", a pattern
// chaos-dns-server rejects, and the planner followed it (#160). The example
// has to pass the same check Apply makes.
func TestTheDNSChaosTemplateTeachesAPatternChaosMeshAccepts(t *testing.T) {
	obj := templateExample(t, "DNSChaos")
	if _, ok := obj["patterns"]; !ok {
		t.Fatal("DNSChaos example has no patterns")
	}
	if err := checkDNSPatterns(obj); err != nil {
		t.Errorf("DNSChaos template example: %v", err)
	}
}
