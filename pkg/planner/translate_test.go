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
	"context"
	"testing"
	"time"

	"github.com/go-steer/simian-agent/pkg/llm/stub"
	"github.com/go-steer/simian-agent/pkg/simian"
)

func sampleCatalog() []simian.CatalogEntry {
	return []simian.CatalogEntry{
		{Engine: simian.EngineChaosMesh, APIVersion: "chaos-mesh.org/v1alpha1", ResourceKind: "NetworkChaos", BlastRadiusTier: simian.TierNamespace},
		{Engine: simian.EngineChaosMesh, APIVersion: "chaos-mesh.org/v1alpha1", ResourceKind: "PodChaos", BlastRadiusTier: simian.TierNamespace},
	}
}

func TestTranslateHappyPath(t *testing.T) {
	provider := stub.New("test")
	if err := provider.AlwaysReturnStructured(map[string]any{
		"engine":        "chaos-mesh",
		"api_version":   "chaos-mesh.org/v1alpha1",
		"resource_kind": "NetworkChaos",
		"spec": map[string]any{
			"action":   "delay",
			"delay":    map[string]any{"latency": "250ms"},
			"selector": map[string]any{"labelSelectors": map[string]any{"app": "paymentservice"}},
			"mode":     "all",
		},
		"targets":   []any{map[string]any{"namespace": "online-boutique", "name": "paymentservice"}},
		"duration":  "2m",
		"rationale": "delay paymentservice for 2 minutes",
	}); err != nil {
		t.Fatalf("seed stub: %v", err)
	}
	tr := New(provider)
	m, err := tr.Translate(context.Background(), IntentInput{
		Intent:          "add 250ms latency to paymentservice for 2 minutes",
		Catalog:         sampleCatalog(),
		DefaultDuration: 2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if m.Engine != simian.EngineChaosMesh {
		t.Errorf("engine=%s, want chaos-mesh", m.Engine)
	}
	if m.ResourceKind != "NetworkChaos" {
		t.Errorf("kind=%s, want NetworkChaos", m.ResourceKind)
	}
	if m.Duration != 2*time.Minute {
		t.Errorf("duration=%s, want 2m", m.Duration)
	}
	if len(m.Targets) != 1 || m.Targets[0].Name != "paymentservice" {
		t.Errorf("targets=%+v, want one paymentservice target", m.Targets)
	}
}

func TestTranslateRetriesOnSchemaInvalid(t *testing.T) {
	provider := stub.New("test")
	// Two responses queued: first invalid (missing engine), second valid.
	provider.AddRule(stub.ResponseRule{
		Match: func(req simian.CompletionRequest) bool {
			// First call has the original prompt; second call has the corrective suffix.
			for _, m := range req.Messages {
				if containsCorrection(m.Content) {
					return false
				}
			}
			return true
		},
		Response: simian.CompletionResponse{
			Structured: []byte(`{"resource_kind":"NetworkChaos"}`), // missing engine + api_version
		},
	})
	provider.AddRule(stub.ResponseRule{
		Match: func(req simian.CompletionRequest) bool { return true },
		Response: simian.CompletionResponse{
			Structured: []byte(`{
				"engine":"chaos-mesh",
				"api_version":"chaos-mesh.org/v1alpha1",
				"resource_kind":"NetworkChaos",
				"spec":{"action":"delay"},
				"targets":[{"namespace":"online-boutique","name":"paymentservice"}],
				"duration":"30s",
				"rationale":"retry good response"
			}`),
		},
	})
	tr := New(provider)
	m, err := tr.Translate(context.Background(), IntentInput{
		Intent:          "delay paymentservice",
		Catalog:         sampleCatalog(),
		DefaultDuration: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if got := len(provider.Calls()); got != 2 {
		t.Errorf("LLM call count=%d, want 2 (one retry)", got)
	}
	if m.Engine != simian.EngineChaosMesh {
		t.Errorf("engine=%s, want chaos-mesh", m.Engine)
	}
}

func containsCorrection(s string) bool {
	return len(s) > 0 && (containsString(s, "previous response failed validation"))
}

func containsString(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (indexOf(haystack, needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestTranslatePromptIncludesDefaultNamespace(t *testing.T) {
	provider := stub.New("test")
	if err := provider.AlwaysReturnStructured(map[string]any{
		"engine":        "chaos-mesh",
		"api_version":   "chaos-mesh.org/v1alpha1",
		"resource_kind": "PodChaos",
		"spec":          map[string]any{"action": "pod-kill", "mode": "one", "selector": map[string]any{"labelSelectors": map[string]any{"app": "paymentservice"}}},
		"targets":       []any{map[string]any{"namespace": "boutique-m3", "name": "paymentservice"}},
		"duration":      "30s",
		"rationale":     "kill paymentservice",
	}); err != nil {
		t.Fatalf("seed stub: %v", err)
	}
	tr := New(provider)
	if _, err := tr.Translate(context.Background(), IntentInput{
		Intent:           "kill one paymentservice pod for 30 seconds",
		Catalog:          sampleCatalog(),
		DefaultDuration:  30 * time.Second,
		DefaultNamespace: "boutique-m3",
	}); err != nil {
		t.Fatalf("Translate: %v", err)
	}
	calls := provider.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 LLM call, got %d", len(calls))
	}
	user := calls[0].Messages[0].Content
	if !containsString(user, `default to "boutique-m3"`) {
		t.Errorf("user prompt should instruct LLM to default to boutique-m3.\nGot:\n%s", user)
	}
	if !containsString(user, `NOT "default"`) {
		t.Errorf("user prompt should warn against using the literal \"default\" namespace.\nGot:\n%s", user)
	}
	// And the system prompt should carry the same NEVER-default rule.
	system := calls[0].System
	if !containsString(system, `NEVER use the literal string "default"`) {
		t.Errorf("system prompt should forbid literal \"default\" namespace.\nGot:\n%s", system)
	}
}

func TestTranslatePromptOmitsDefaultNamespaceClauseWhenUnset(t *testing.T) {
	provider := stub.New("test")
	if err := provider.AlwaysReturnStructured(map[string]any{
		"engine":        "chaos-mesh",
		"api_version":   "chaos-mesh.org/v1alpha1",
		"resource_kind": "PodChaos",
		"spec":          map[string]any{"action": "pod-kill", "mode": "one", "selector": map[string]any{"labelSelectors": map[string]any{"app": "x"}}},
		"targets":       []any{map[string]any{"namespace": "ns", "name": "x"}},
		"duration":      "30s",
		"rationale":     "x",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tr := New(provider)
	if _, err := tr.Translate(context.Background(), IntentInput{
		Intent:          "kill x in namespace ns",
		Catalog:         sampleCatalog(),
		DefaultDuration: 30 * time.Second,
		// DefaultNamespace deliberately empty — caller didn't pass one.
	}); err != nil {
		t.Fatalf("Translate: %v", err)
	}
	user := provider.Calls()[0].Messages[0].Content
	if containsString(user, "If the user did not name a namespace") {
		t.Errorf("user prompt should not mention default namespace when none provided.\nGot:\n%s", user)
	}
}
