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

// On the 2026-09-30 trial (--duration-ceiling=5m) the planner was never told
// the ceiling and copied the example's "30s" into 93 of 128 plans; one asked
// for 10m and was refused at apply (#162).
func TestThePlannerIsToldTheDurationCeilingAndNotHandedADuration(t *testing.T) {
	in := sampleInput()
	in.Budget.MaxFaultDuration = 5 * time.Minute

	if user := buildPlanUserPrompt(in); !strings.Contains(user, "max_fault_duration: 5m0s") {
		t.Errorf("user prompt does not state the ceiling:\n%s", user)
	}
	if system := buildPlanSystemPrompt(in.Catalog); strings.Contains(system, `"duration": "30s"`) {
		t.Error(`system prompt's example plan still pins "duration": "30s"`)
	}

	in.Budget.MaxFaultDuration = 0
	if user := buildPlanUserPrompt(in); strings.Contains(user, "max_fault_duration") {
		t.Errorf("user prompt states a ceiling when there is none:\n%s", user)
	}
}

func TestAStepOverTheCeilingIsSentBackForCorrection(t *testing.T) {
	over := strings.Replace(wellFormedPlanJSON(), `"duration": "30s"`, `"duration": "10m"`, 1)
	p := &toggleProvider{
		first:  simian.CompletionResponse{Text: over},
		second: simian.CompletionResponse{Text: wellFormedPlanJSON()},
	}
	in := sampleInput()
	in.Budget.MaxFaultDuration = 5 * time.Minute

	plan, err := NewGenerator(p).Generate(context.Background(), in)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if p.count != 2 {
		t.Errorf("LLM called %d times, want 2: the 10m plan should have been sent back", p.count)
	}
	if got := plan.Steps[0].Manifest.Duration; got != 30*time.Second {
		t.Errorf("duration = %s, want the corrected 30s", got)
	}

	_, err = parseAttackPlan([]byte(over), in)
	if err == nil || !strings.Contains(err.Error(), "exceeds max_fault_duration 5m0s") {
		t.Errorf("err = %v, want it to name the ceiling so the retry can fix it", err)
	}
}

func TestAStepWithNoDurationDefaultsWithinTheCeiling(t *testing.T) {
	none := strings.Replace(wellFormedPlanJSON(), `"duration": "30s",`, ``, 1)
	in := sampleInput()
	in.Budget.MaxFaultDuration = time.Minute
	plan, err := parseAttackPlan([]byte(none), in)
	if err != nil {
		t.Fatalf("parseAttackPlan: %v", err)
	}
	if got := plan.Steps[0].Manifest.Duration; got != time.Minute {
		t.Errorf("duration = %s, want the 1m ceiling rather than the 2m default", got)
	}
}

func TestThePlannerSeesWhichContainersAreReadOnlyAndWhatTheyMount(t *testing.T) {
	out := summarizeTopology(&topology.TargetTopology{Workloads: []topology.Workload{
		{Kind: "Deployment", Name: "redis-cart", Containers: []topology.ContainerSummary{
			{Name: "redis", ReadOnlyRootFS: true, MountPaths: []string{"/data"}},
		}},
		{Kind: "Deployment", Name: "frontend", Containers: []topology.ContainerSummary{{Name: "server"}}},
	}})
	if !strings.Contains(out, "Deployment/redis-cart replicas=0 readonly_rootfs=redis mounts=redis:/data") {
		t.Errorf("redis-cart line missing its read-only root or mount:\n%s", out)
	}
	if strings.Contains(out, "frontend replicas=0 readonly_rootfs") || strings.Contains(out, "frontend replicas=0 mounts") {
		t.Errorf("frontend flagged with nothing to flag:\n%s", out)
	}
	if system := buildPlanSystemPrompt(nil); !strings.Contains(system, "readonly_rootfs") {
		t.Error("system prompt does not explain readonly_rootfs")
	}
}

// #173: the planner kept choosing IOChaos on a read-only redis-cart despite
// rule 11. A step the topology already rules out fails validation, so the
// corrective retry fixes it instead of the executor refusing it.
func TestAStepTheTopologyRulesOutFailsValidation(t *testing.T) {
	in := sampleInput()
	in.Topology.Workloads = append(in.Topology.Workloads,
		topology.Workload{Kind: "Deployment", Name: "redis-cart", Labels: map[string]string{"app": "redis-cart"},
			Containers: []topology.ContainerSummary{{Name: "redis", ReadOnlyRootFS: true, MountPaths: []string{"/data"}}}},
		topology.Workload{Kind: "Deployment", Name: "ledger-db", Labels: map[string]string{"app": "ledger-db"},
			Containers: []topology.ContainerSummary{{Name: "postgres", MountPaths: []string{"/var/lib/postgresql/data/"}}}},
	)
	step := func(kind, target, spec string) string {
		return strings.NewReplacer(
			`"resource_kind": "PodChaos"`, `"resource_kind": "`+kind+`"`,
			`"name": "cartservice"`, `"name": "`+target+`"`,
			`"spec": {"action": "pod-kill", "mode": "one"}`, `"spec": `+spec,
		).Replace(wellFormedPlanJSON())
	}
	for name, tc := range map[string]struct {
		plan string
		want string // "" = accepted
	}{
		"IOChaos on a read-only container, by name": {
			step("IOChaos", "redis-cart", `{"action": "latency", "volumePath": "/data"}`), `container "redis" is readonly_rootfs`},
		"DNSChaos on a read-only container, by label selector": {
			step("DNSChaos", "frontend-x", `{"action": "error", "selector": {"labelSelectors": {"app": "redis-cart"}}}`), `container "redis" is readonly_rootfs`},
		"IOChaos at a path that is not a mount": {
			step("IOChaos", "ledger-db", `{"action": "latency", "volumePath": "/var/lib"}`), `is not a mount of ledger-db`},
		"IOChaos at a mount, written without the trailing slash": {
			step("IOChaos", "ledger-db", `{"action": "latency", "volumePath": "/var/lib/postgresql/data"}`), ""},
		"PodChaos on a read-only container": {
			step("PodChaos", "redis-cart", `{"action": "pod-kill"}`), ""},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseAttackPlan([]byte(tc.plan), in)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("rejected: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
