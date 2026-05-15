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
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/simian-agent/pkg/llm/stub"
	"github.com/go-steer/simian-agent/pkg/simian"
	"github.com/go-steer/simian-agent/pkg/topology"
)

func sampleInput() GenerateInput {
	return GenerateInput{
		Namespace: "boutique",
		Topology: &topology.TargetTopology{
			Namespace: "boutique",
			Workloads: []topology.Workload{
				{Kind: "Deployment", Name: "frontend", DesiredReplicas: 2},
				{Kind: "Deployment", Name: "cartservice", DesiredReplicas: 1},
			},
		},
		Catalog: []simian.CatalogEntry{{
			Engine: simian.EngineChaosMesh, ResourceKind: "PodChaos",
			APIVersion: "chaos-mesh.org/v1alpha1", BlastRadiusTier: simian.TierNamespace,
		}},
		Budget: Budget{
			MaxFaultsPerCycle:   3,
			MaxConcurrentFaults: 1,
			MinCooldown:         30 * time.Second,
			MaxSeverityPerCycle: simian.TierNamespace,
		},
	}
}

func wellFormedPlanJSON() string {
	return `{
  "hypothesis": "killing one cartservice pod will not break the frontend",
  "steps": [{
    "order": 1,
    "rationale": "exercise pod-restart resilience",
    "manifest": {
      "engine": "chaos-mesh",
      "api_version": "chaos-mesh.org/v1alpha1",
      "resource_kind": "PodChaos",
      "spec": {"action": "pod-kill", "mode": "one"},
      "targets": [{"namespace": "boutique", "name": "cartservice"}],
      "duration": "30s",
      "blast_radius_tier": "namespace",
      "rationale": "kill one cartservice pod"
    }
  }]
}`
}

func TestGenerate_HappyPath(t *testing.T) {
	llm := stub.New("stub")
	llm.AddRule(stub.ResponseRule{
		Match:    func(simian.CompletionRequest) bool { return true },
		Response: simian.CompletionResponse{Text: wellFormedPlanJSON()},
	})
	g := NewGenerator(llm)
	plan, err := g.Generate(context.Background(), sampleInput())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if plan.PlanID == "" {
		t.Error("PlanID should be stamped")
	}
	if plan.Hypothesis == "" || len(plan.Steps) != 1 {
		t.Fatalf("plan structure unexpected: %+v", plan)
	}
	if plan.Steps[0].Manifest.Source != simian.SourceAutonomous {
		t.Errorf("step source = %q, want autonomous", plan.Steps[0].Manifest.Source)
	}
	if plan.Steps[0].Manifest.PlanID != plan.PlanID {
		t.Errorf("step manifest plan_id = %q, want %q", plan.Steps[0].Manifest.PlanID, plan.PlanID)
	}
}

func TestGenerate_RetriesOnSchemaInvalid(t *testing.T) {
	llm := stub.New("stub")
	calls := 0
	llm.AddRule(stub.ResponseRule{
		Match: func(simian.CompletionRequest) bool {
			calls++
			return true
		},
		// First call returns missing-hypothesis; rule replaced below for second call.
	})
	// Override the AddRule with a custom handler via a chain.
	g := NewGenerator(&toggleProvider{
		first:  simian.CompletionResponse{Text: `{"steps":[]}`}, // invalid: empty steps
		second: simian.CompletionResponse{Text: wellFormedPlanJSON()},
	})
	plan, err := g.Generate(context.Background(), sampleInput())
	if err != nil {
		t.Fatalf("Generate after retry: %v", err)
	}
	if len(plan.Steps) != 1 {
		t.Errorf("expected one step after successful retry, got %d", len(plan.Steps))
	}
}

func TestGenerate_FailsAfterRetriesExhausted(t *testing.T) {
	llm := stub.New("stub")
	llm.AddRule(stub.ResponseRule{
		Match:    func(simian.CompletionRequest) bool { return true },
		Response: simian.CompletionResponse{Text: `{"steps":[]}`}, // always invalid
	})
	g := NewGenerator(llm)
	_, err := g.Generate(context.Background(), sampleInput())
	if err == nil || !strings.Contains(err.Error(), "exhausted retries") {
		t.Fatalf("expected exhausted-retries error, got %v", err)
	}
}

func TestGenerate_RejectsCycle(t *testing.T) {
	cyclic := `{
  "hypothesis": "x",
  "steps": [
    {"order":1,"depends_on":[2],"manifest":{"engine":"chaos-mesh","api_version":"v","resource_kind":"PodChaos","spec":{"x":1},"targets":[{"namespace":"boutique"}],"duration":"30s"}},
    {"order":2,"depends_on":[1],"manifest":{"engine":"chaos-mesh","api_version":"v","resource_kind":"PodChaos","spec":{"x":1},"targets":[{"namespace":"boutique"}],"duration":"30s"}}
  ]
}`
	llm := stub.New("stub")
	llm.AddRule(stub.ResponseRule{
		Match:    func(simian.CompletionRequest) bool { return true },
		Response: simian.CompletionResponse{Text: cyclic},
	})
	g := NewGenerator(llm)
	g.MaxRetries = 0 // fail fast for the test
	_, err := g.Generate(context.Background(), sampleInput())
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected cycle error, got %v", err)
	}
}

func TestGenerate_PropagatesLLMError(t *testing.T) {
	g := NewGenerator(&errorProvider{})
	_, err := g.Generate(context.Background(), sampleInput())
	if err == nil || !strings.Contains(err.Error(), "LLM call failed") {
		t.Fatalf("expected LLM call failed, got %v", err)
	}
}

func TestGenerate_RequiresNamespace(t *testing.T) {
	in := sampleInput()
	in.Namespace = ""
	g := NewGenerator(stub.New("stub"))
	if _, err := g.Generate(context.Background(), in); err == nil {
		t.Fatal("expected error when namespace empty")
	}
}

func TestGenerate_RequiresCatalog(t *testing.T) {
	in := sampleInput()
	in.Catalog = nil
	g := NewGenerator(stub.New("stub"))
	if _, err := g.Generate(context.Background(), in); err == nil {
		t.Fatal("expected error when catalog empty")
	}
}

func TestGenerate_SetsDefaultNamespaceOnTargets(t *testing.T) {
	planJSON := `{
  "hypothesis": "x",
  "steps": [{
    "order":1,
    "manifest": {
      "engine":"chaos-mesh","api_version":"v","resource_kind":"PodChaos",
      "spec": {"action":"pod-kill"}, "targets":[{"namespace":""}],
      "duration":"30s"
    }
  }]
}`
	llm := stub.New("stub")
	llm.AddRule(stub.ResponseRule{
		Match:    func(simian.CompletionRequest) bool { return true },
		Response: simian.CompletionResponse{Text: planJSON},
	})
	g := NewGenerator(llm)
	plan, err := g.Generate(context.Background(), sampleInput())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if plan.Steps[0].Manifest.Targets[0].Namespace != "boutique" {
		t.Errorf("namespace not defaulted, got %q", plan.Steps[0].Manifest.Targets[0].Namespace)
	}
}

// toggleProvider is a minimal LLM provider that returns first then second.
type toggleProvider struct {
	first, second simian.CompletionResponse
	count         int
}

func (p *toggleProvider) Name() string { return "toggle" }
func (p *toggleProvider) Complete(_ context.Context, _ simian.CompletionRequest) (simian.CompletionResponse, error) {
	p.count++
	if p.count == 1 {
		return p.first, nil
	}
	return p.second, nil
}

type errorProvider struct{}

func (errorProvider) Name() string { return "err" }
func (errorProvider) Complete(_ context.Context, _ simian.CompletionRequest) (simian.CompletionResponse, error) {
	return simian.CompletionResponse{}, simianErr("provider unreachable")
}

type simianErr string

func (e simianErr) Error() string { return string(e) }

// TestSummarizeTopologyMarksEnvoyInjected verifies the autonomous-mode
// prompt's topology section flags envoy-injected workloads with
// "envoy=true", which is the planner's eligibility hint for the
// envoy-fault chaos kinds.
func TestSummarizeTopologyMarksEnvoyInjected(t *testing.T) {
	t1 := &topology.TargetTopology{
		Workloads: []topology.Workload{
			{Kind: "Deployment", Name: "frontend", DesiredReplicas: 1, EnvoyInjected: true},
			{Kind: "Deployment", Name: "loadgenerator", DesiredReplicas: 1, EnvoyInjected: false},
		},
	}
	out := summarizeTopology(t1)
	if !strings.Contains(out, "frontend") || !strings.Contains(out, "envoy=true") {
		t.Errorf("expected frontend with envoy=true; got:\n%s", out)
	}
	// The loadgenerator line should NOT carry envoy=true.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "loadgenerator") && strings.Contains(line, "envoy=true") {
			t.Errorf("loadgenerator should not have envoy=true; got line:\n%s", line)
		}
	}
}

func TestPlanSystemPromptIncludesEnvoyEligibilityRule(t *testing.T) {
	cat := []simian.CatalogEntry{
		{Engine: simian.EngineEnvoyFault, ResourceKind: "EnvoyHttpDelay", APIVersion: "simian.io/v1", BlastRadiusTier: simian.TierNamespace},
	}
	system := buildPlanSystemPrompt(cat)
	if !strings.Contains(system, "envoy=true") {
		t.Errorf("system prompt should reference envoy=true precondition; got:\n%s", system)
	}
}

// Sanity check that the JSON we use in tests round-trips through the
// AttackPlan type without information loss; protects against schema drift.
func TestSampleJSONRoundTrips(t *testing.T) {
	var plan simian.AttackPlan
	if err := json.Unmarshal([]byte(wellFormedPlanJSON()), &plan); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if plan.Hypothesis == "" || len(plan.Steps) != 1 {
		t.Fatalf("unexpected: %+v", plan)
	}
}
