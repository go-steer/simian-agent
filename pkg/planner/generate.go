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
	"errors"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/go-steer/simian-agent/pkg/executor"
	"github.com/go-steer/simian-agent/pkg/simian"
	"github.com/go-steer/simian-agent/pkg/sut"
	"github.com/go-steer/simian-agent/pkg/topology"
)

// ErrNoValidPlan is wrapped by Generate when the LLM answered every attempt
// and none of its plans passed validation — not the same thing as the LLM
// being unavailable, and recorded differently.
var ErrNoValidPlan = errors.New("no valid plan")

// Budget is the install-side per-cycle budget the autonomous loop enforces.
// The LLM also emits a PlanBudget in its AttackPlan; the loop applies
// `min(plan, install)` at every cap. Defined here (not in pkg/loop) so the
// Generator can reference it without an import cycle.
type Budget struct {
	MaxFaultsPerCycle   int
	MaxConcurrentFaults int
	MinCooldown         time.Duration
	MaxSeverityPerCycle simian.BlastRadiusTier
	// MaxFaultDuration is the executor's duration ceiling. A step that asks
	// for longer is refused at apply time, so the planner is told the limit
	// and a plan that exceeds it is sent back for correction. 0 means none.
	MaxFaultDuration time.Duration
}

// GenerateInput is everything the Generator needs to draft an AttackPlan.
type GenerateInput struct {
	Namespace    string
	Topology     *topology.TargetTopology
	Baseline     *sut.Baseline // optional; nil if no SUT deployed
	Catalog      []simian.CatalogEntry
	RecentFaults []executor.RecentFault
	// RecentRefusals are faults the executor would not run, newest first. A
	// planner that only sees what ran proposes what did not, again.
	RecentRefusals []executor.RefusedFault
	Budget         Budget
	Hypothesis     string // optional user-supplied seed
}

// Generator drafts an AttackPlan by asking the LLM for structured output.
// The output is always validated (schema + DAG) before the loop sees it; on
// schema-invalid responses, the Generator retries once with a corrective
// follow-up turn before giving up.
type Generator struct {
	LLM          simian.LLMProvider
	Model        string
	MaxRetries   int
	LogResponses func(attempt int, raw []byte)
}

// NewGenerator returns a Generator with sane defaults.
func NewGenerator(llm simian.LLMProvider) *Generator {
	return &Generator{LLM: llm, MaxRetries: 1}
}

// Generate asks the LLM for an AttackPlan and validates it. Returns a typed
// AttackPlan ready for execution, or an error suitable for the cycle audit
// (LLMUnavailable / SchemaInvalid). The returned plan always carries a
// freshly-minted PlanID and Source=autonomous on every step's manifest.
func (g *Generator) Generate(ctx context.Context, in GenerateInput) (simian.AttackPlan, error) {
	if in.Namespace == "" {
		return simian.AttackPlan{}, fmt.Errorf("generator: namespace is required")
	}
	if len(in.Catalog) == 0 {
		return simian.AttackPlan{}, fmt.Errorf("generator: catalog is empty — no faults are installed or permitted")
	}

	system := buildPlanSystemPrompt(in.Catalog)
	user := buildPlanUserPrompt(in)

	maxRetries := g.MaxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		// As in translate.go, we deliberately do NOT pass ResponseSchema:
		// strict structured-output mode cannot fill the nested
		// `manifest.spec` object with engine-native fields not enumerated
		// in the schema, and we want the LLM to populate spec freely.
		req := simian.CompletionRequest{
			System:      system,
			Messages:    []simian.Message{{Role: "user", Content: user}},
			Temperature: 0.3,
			MaxTokens:   16384,
			Model:       g.Model,
		}
		resp, err := g.LLM.Complete(ctx, req)
		if err != nil {
			return simian.AttackPlan{}, fmt.Errorf("generator: LLM call failed: %w", err)
		}

		var raw []byte
		switch {
		case len(resp.Structured) > 0:
			raw = resp.Structured
		default:
			raw = extractJSON(resp.Text)
		}
		if g.LogResponses != nil {
			g.LogResponses(attempt, raw)
		}

		plan, perr := parseAttackPlan(raw, in)
		if perr == nil {
			return plan, nil
		}
		lastErr = perr
		// Append the validation error to the user turn and retry.
		user = user + "\n\nYour previous response failed validation: " + perr.Error() +
			"\nReturn a JSON object that conforms to the AttackPlan schema described above."
	}
	return simian.AttackPlan{}, fmt.Errorf("generator: exhausted retries: %w: %w", ErrNoValidPlan, lastErr)
}

// parseAttackPlan decodes the LLM's JSON, normalizes per-step durations,
// stamps the autonomous source, mints a PlanID, and validates the DAG.
func parseAttackPlan(raw []byte, in GenerateInput) (simian.AttackPlan, error) {
	if len(raw) == 0 {
		return simian.AttackPlan{}, fmt.Errorf("empty plan response")
	}
	var plan simian.AttackPlan
	if err := json.Unmarshal(raw, &plan); err != nil {
		return simian.AttackPlan{}, fmt.Errorf("decode: %w", err)
	}
	if len(plan.Steps) == 0 {
		return simian.AttackPlan{}, fmt.Errorf("plan must include at least one step")
	}
	if plan.Hypothesis == "" {
		return simian.AttackPlan{}, fmt.Errorf("plan must include a hypothesis")
	}

	// Normalize each step.
	for i := range plan.Steps {
		s := &plan.Steps[i]
		if s.Order == 0 {
			s.Order = i + 1
		}
		s.Manifest.Source = simian.SourceAutonomous
		// Default the namespace if the LLM omitted it.
		for j := range s.Manifest.Targets {
			if s.Manifest.Targets[j].Namespace == "" {
				s.Manifest.Targets[j].Namespace = in.Namespace
			}
		}
		if s.Manifest.Duration <= 0 {
			s.Manifest.Duration = 2 * time.Minute
			if ceiling := in.Budget.MaxFaultDuration; ceiling > 0 && s.Manifest.Duration > ceiling {
				s.Manifest.Duration = ceiling
			}
		}
		if ceiling := in.Budget.MaxFaultDuration; ceiling > 0 && s.Manifest.Duration > ceiling {
			return simian.AttackPlan{}, fmt.Errorf("step %d: duration %s exceeds max_fault_duration %s", s.Order, s.Manifest.Duration, ceiling)
		}
		// Light per-step manifest sanity — full schema validation lives in
		// the executor, but reject obviously broken steps here so the LLM
		// can be told what to fix on the retry.
		if s.Manifest.Engine == "" {
			return simian.AttackPlan{}, fmt.Errorf("step %d: engine is required", s.Order)
		}
		if s.Manifest.ResourceKind == "" {
			return simian.AttackPlan{}, fmt.Errorf("step %d: resource_kind is required", s.Order)
		}
		if s.Manifest.APIVersion == "" {
			return simian.AttackPlan{}, fmt.Errorf("step %d: api_version is required", s.Order)
		}
		if len(s.Manifest.Targets) == 0 {
			return simian.AttackPlan{}, fmt.Errorf("step %d: at least one target is required", s.Order)
		}
		if s.Manifest.Spec == nil {
			return simian.AttackPlan{}, fmt.Errorf("step %d: spec is required", s.Order)
		}
		if strings.TrimSpace(s.DurationRationale) == "" {
			return simian.AttackPlan{}, fmt.Errorf("step %d: duration_rationale is required (rule 10): say what the %s duration lets the hypothesis observe", s.Order, s.Manifest.Duration)
		}
		if err := checkTargetCompat(s.Manifest, in.Topology); err != nil {
			return simian.AttackPlan{}, fmt.Errorf("step %d: %w", s.Order, err)
		}
	}

	if err := validateStepDAG(plan.Steps); err != nil {
		return simian.AttackPlan{}, err
	}

	// Stamp PlanID and propagate it to every step's manifest so audit lines
	// can correlate.
	if plan.PlanID == "" {
		plan.PlanID = "plan-" + ulid.Make().String()
	}
	for i := range plan.Steps {
		plan.Steps[i].Manifest.PlanID = plan.PlanID
	}
	return plan, nil
}

func buildPlanSystemPrompt(cat []simian.CatalogEntry) string {
	var sb strings.Builder
	sb.WriteString(`You are Simian Agent's autonomous-mode plan generator. Your job is to produce a structured AttackPlan: an ordered set of chaos engineering experiments designed to test the resilience of a single arena namespace.

You will be given:
- Live cluster topology (workloads, services, dependency graph).
- The current baseline snapshot (what "healthy" looks like).
- A catalog of fault types installed and permitted by current policy (each entry below includes its canonical spec template).
- Recent faults (so you don't repeat the same attack with no observation gap).
- Per-cycle budget caps (you MUST respect these).
- An optional hypothesis hint from the operator.

Your response MUST be a single JSON object matching this schema (no markdown, no commentary):

{
  "plan_id": "",                   // leave empty; the runtime stamps it
  "hypothesis": "...",             // one or two sentences: what you expect to learn
  "steps": [
    {
      "order": 1,                  // 1-indexed; unique within the plan
      "rationale": "...",          // why this step, why now
      "duration_rationale": "...", // required; see rule 10
      "depends_on": [],            // optional list of prior step Orders this depends on
      "manifest": {
        "engine":        "<engine from catalog>",
        "api_version":   "<api_version from catalog>",
        "resource_kind": "<resource_kind from catalog>",
        "spec": { ... },           // engine-native; copy the spec template under the catalog entry and adapt
        "targets": [{"namespace": "<ns>", "name": "<workload>"}],
        "duration": "<Go duration>", // see rule 10
        "blast_radius_tier": "namespace",
        "rationale": "..."
      }
    }
  ],
  "budget": {                      // declare what you intend; the loop caps further
    "max_concurrent_faults": 1,
    "min_cooldown": "30s",
    "max_severity_tier": "namespace"
  }
}

Rules you MUST follow:
1. Pick fault types only from the catalog provided; do not invent kinds.
2. Targets MUST name workloads that exist in the topology you were given.
3. Each target MUST set "namespace" explicitly to the Target namespace given above; never leave it blank, never use the literal string "default".
4. Honor the cycle budget caps — do not emit more steps than max_faults_per_cycle.
5. Order steps so cause precedes effect; use depends_on to express ordering.
6. Plans should be small (1–3 steps typical) so observation can be scoped.
7. Engine-native spec MUST be populated. Where a catalog entry's spec template lists "action MUST be one of …", picking outside that list causes the cluster to reject the manifest at apply time (driver.failed).
8. NEVER target a workload tagged as "excluded" via topology.
9. envoy-fault kinds (EnvoyHttpDelay, EnvoyHttpAbort) require the target workload to be flagged envoy=true in the topology snapshot. If the chosen target lacks envoy=true, pick a different workload OR a different fault kind.
10. Choose each step's "duration" for its own hypothesis, and say why in "duration_rationale": what must be observed, and how long that takes. Faults differ, so durations should too. A one-shot action (PodChaos pod-kill or container-kill) acts once at injection; its duration is only the window for watching recovery, so size it to how long the workload takes to come back. A sustained fault (latency, loss, partition, IO delay, DNS errors, stress, an HTTP abort) acts for the whole duration; size it to how long the effect must persist to show up in probes and alerts, and to test whether it degrades further over time. Do not copy the durations of recent faults by default. Never exceed max_fault_duration in the cycle budget caps — a longer step is refused before it runs.
11. Chaos Mesh IOChaos and DNSChaos cannot inject into a container listed under readonly_rootfs in the topology; they are refused before they run. IOChaos's "volumePath" must be exactly one of the target container's mounts (listed as container:path); with no mounts listed, do not choose IOChaos for that workload.

Available fault catalog (kinds you may choose). Each entry shows engine + kind + api_version + tier; entries with a spec template include the canonical engine-native spec shape directly under the entry — copy and adapt.

`)
	renderCatalogWithTemplates(&sb, cat)
	return sb.String()
}

func buildPlanUserPrompt(in GenerateInput) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Target namespace: %s\n\n", in.Namespace)

	if in.Hypothesis != "" {
		fmt.Fprintf(&sb, "Operator hypothesis hint (soft preference): %s\n\n", in.Hypothesis)
	}

	fmt.Fprintln(&sb, "## Cycle budget caps")
	fmt.Fprintf(&sb, "- max_faults_per_cycle: %d\n", in.Budget.MaxFaultsPerCycle)
	fmt.Fprintf(&sb, "- max_concurrent_faults: %d\n", in.Budget.MaxConcurrentFaults)
	fmt.Fprintf(&sb, "- min_cooldown: %s\n", in.Budget.MinCooldown)
	fmt.Fprintf(&sb, "- max_severity_tier: %s\n", in.Budget.MaxSeverityPerCycle)
	if in.Budget.MaxFaultDuration > 0 {
		fmt.Fprintf(&sb, "- max_fault_duration: %s (hard limit per step)\n", in.Budget.MaxFaultDuration)
	}
	sb.WriteString("\n")

	if in.Topology != nil {
		sb.WriteString("## Topology snapshot\n")
		sb.WriteString(summarizeTopology(in.Topology))
		sb.WriteString("\n")
	} else {
		sb.WriteString("## Topology snapshot\n(unavailable)\n\n")
	}

	if in.Baseline != nil {
		sb.WriteString("## Baseline (what healthy looks like)\n")
		fmt.Fprintf(&sb, "SUT=%s, established=%s, stability_window=%s\n",
			in.Baseline.SUT, in.Baseline.EstablishedAt.Format(time.RFC3339), in.Baseline.StabilityWindow)
		for _, w := range in.Baseline.Workloads {
			fmt.Fprintf(&sb, "  %s/%s ready=%d/%d\n", w.Kind, w.Name, w.ReadyReplicas, w.DesiredReplicas)
		}
		sb.WriteString("\n")
	}

	if len(in.RecentFaults) > 0 {
		sb.WriteString("## Recent faults (avoid pointless repetition)\n")
		for _, rf := range in.RecentFaults {
			ns, name := "", ""
			if len(rf.Manifest.Targets) > 0 {
				ns = rf.Manifest.Targets[0].Namespace
				name = rf.Manifest.Targets[0].Name
			}
			cleared := "still active"
			if !rf.ClearedAt.IsZero() {
				cleared = "cleared " + rf.ClearedAt.Format(time.RFC3339) + " (" + rf.ClearReason + ")"
			}
			duration := ""
			if rf.Manifest.Duration > 0 {
				duration = " for " + rf.Manifest.Duration.String()
			}
			fmt.Fprintf(&sb, "  %s on %s/%s%s applied %s, %s\n",
				rf.Manifest.ResourceKind, ns, name, duration, rf.AppliedAt.Format(time.RFC3339), cleared)
		}
		sb.WriteString("\n")
	}

	if len(in.RecentRefusals) > 0 {
		sb.WriteString("## Recently refused — do not repeat without changing what caused the refusal\n")
		sb.WriteString(summarizeRefusals(in.RecentRefusals))
		sb.WriteString("\n")
	}

	sb.WriteString("Emit the AttackPlan JSON now.")
	return sb.String()
}

func summarizeTopology(t *topology.TargetTopology) string {
	var sb strings.Builder
	sb.WriteString("Workloads (envoy=true marks workloads with the Envoy fault sidecar; only those are eligible for envoy-fault chaos kinds):\n")
	for _, w := range t.Workloads {
		fmt.Fprintf(&sb, "  %s/%s replicas=%d", w.Kind, w.Name, w.DesiredReplicas)
		if w.EnvoyInjected {
			sb.WriteString(" envoy=true")
		}
		var readOnly, mounts []string
		for _, c := range w.Containers {
			if c.ReadOnlyRootFS {
				readOnly = append(readOnly, c.Name)
			}
			for _, mp := range c.MountPaths {
				mounts = append(mounts, c.Name+":"+mp)
			}
		}
		if len(readOnly) > 0 {
			fmt.Fprintf(&sb, " readonly_rootfs=%s", strings.Join(readOnly, ","))
		}
		if len(mounts) > 0 {
			fmt.Fprintf(&sb, " mounts=%s", strings.Join(mounts, ","))
		}
		if pods := t.PodStatus[w.Name]; len(pods) > 0 {
			ready := 0
			for _, p := range pods {
				if p.Ready {
					ready++
				}
			}
			fmt.Fprintf(&sb, " pods_ready=%d/%d", ready, len(pods))
		}
		sb.WriteString("\n")
	}
	if len(t.Services) > 0 {
		sb.WriteString("Services: ")
		names := make([]string, 0, len(t.Services))
		for _, s := range t.Services {
			names = append(names, s.Name)
		}
		sb.WriteString(strings.Join(names, ", "))
		sb.WriteString("\n")
	}
	if len(t.DependencyGraph) > 0 {
		sb.WriteString("Dependencies (caller → callees, provenance in []):\n")
		for src, dsts := range t.DependencyGraph {
			provs := make([]string, 0, len(dsts))
			for _, d := range dsts {
				provs = append(provs, fmt.Sprintf("%s [%s]", d, strings.Join(t.EdgeProvenance[src+"->"+d], ",")))
			}
			fmt.Fprintf(&sb, "  %s -> %s\n", src, strings.Join(provs, ", "))
		}
	}
	return sb.String()
}

// refusalErrorLimit bounds how much of each refusal's error reaches the
// prompt. The cause is in the first line or two; the rest is probe output.
const refusalErrorLimit = 240

// summarizeRefusals renders refusals one line per distinct (kind, targets,
// reason, error), newest first, with a count. Identical refusals are folded
// because they arrive in runs — the same step refused cycle after cycle — and
// ten copies of one line would crowd out the other refusals without saying
// anything the count does not.
func summarizeRefusals(refusals []executor.RefusedFault) string {
	type group struct {
		line  string
		count int
		last  time.Time
	}
	var order []string
	groups := map[string]*group{}
	for _, rf := range refusals {
		msg := rf.Error
		if len(msg) > refusalErrorLimit {
			msg = msg[:refusalErrorLimit] + "…"
		}
		reason := string(rf.Reason)
		if reason == "" {
			reason = "refused"
		}
		line := fmt.Sprintf("%s on %s: %s: %s", rf.Manifest.ResourceKind, describeTargets(rf.Manifest.Targets), reason, msg)
		g, ok := groups[line]
		if !ok {
			g = &group{line: line, last: rf.RefusedAt}
			groups[line] = g
			order = append(order, line)
		}
		g.count++
		if rf.RefusedAt.After(g.last) {
			g.last = rf.RefusedAt
		}
	}
	var sb strings.Builder
	for _, key := range order {
		g := groups[key]
		fmt.Fprintf(&sb, "  %s (x%d, last %s)\n", g.line, g.count, g.last.Format(time.RFC3339))
	}
	return sb.String()
}

// describeTargets renders every target, not just the first: a refusal can be
// about any of them.
func describeTargets(targets []simian.TargetRef) string {
	if len(targets) == 0 {
		return "(no target)"
	}
	parts := make([]string, 0, len(targets))
	for _, t := range targets {
		switch {
		case t.Name != "":
			parts = append(parts, t.Namespace+"/"+t.Name)
		case len(t.Labels) > 0:
			parts = append(parts, t.Namespace+"/"+renderLabels(t.Labels))
		default:
			parts = append(parts, t.Namespace)
		}
	}
	return strings.Join(parts, ", ")
}

func renderLabels(labels map[string]string) string {
	parts := make([]string, 0, len(labels))
	for k, v := range labels {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// checkTargetCompat rejects a step the topology already shows cannot be
// injected: IOChaos or DNSChaos into a container with a read-only root
// filesystem, or IOChaos at a volumePath that is not one of the container's
// mounts. Rule 11 says as much, and the executor refuses it anyway; caught
// here, it costs a corrective retry instead of the namespace's cycle. On the
// 2026-10-01 trial the planner chose IOChaos on a read-only redis-cart four
// times in an hour (#173).
//
// Only workloads the step demonstrably targets are judged — by name, or by a
// label selector their pod template satisfies.
func checkTargetCompat(m simian.FaultManifest, topo *topology.TargetTopology) error {
	if topo == nil || !executor.WritesRootFS(m.Engine, m.ResourceKind) {
		return nil
	}
	var only []string
	if raw, ok := m.Spec["containerNames"].([]any); ok {
		for _, v := range raw {
			if s, ok := v.(string); ok {
				only = append(only, s)
			}
		}
	}
	volumePath, _ := m.Spec["volumePath"].(string)
	for _, w := range topo.Workloads {
		if !stepTargets(m, w) {
			continue
		}
		for _, c := range w.Containers {
			if len(only) > 0 && !slices.Contains(only, c.Name) {
				continue
			}
			if c.ReadOnlyRootFS {
				return fmt.Errorf("%s cannot inject into %s: container %q is readonly_rootfs (rule 11); choose another kind or workload",
					m.ResourceKind, w.Name, c.Name)
			}
			if m.ResourceKind == "IOChaos" && volumePath != "" && !mountsAt(c, volumePath) {
				return fmt.Errorf("IOChaos volumePath %q is not a mount of %s container %q (mounts: %v; rule 11)",
					volumePath, w.Name, c.Name, c.MountPaths)
			}
		}
	}
	return nil
}

// stepTargets reports whether the step names the workload, or selects its
// pods by labels.
func stepTargets(m simian.FaultManifest, w topology.Workload) bool {
	for _, t := range m.Targets {
		if t.Name == w.Name || (len(t.Labels) > 0 && labelsMatch(t.Labels, w.Labels)) {
			return true
		}
	}
	sel, _ := m.Spec["selector"].(map[string]any)
	ls, _ := sel["labelSelectors"].(map[string]any)
	if len(ls) == 0 {
		return false
	}
	want := make(map[string]string, len(ls))
	for k, v := range ls {
		s, ok := v.(string)
		if !ok {
			return false
		}
		want[k] = s
	}
	return labelsMatch(want, w.Labels)
}

func mountsAt(c topology.ContainerSummary, p string) bool {
	for _, mp := range c.MountPaths {
		if path.Clean(mp) == path.Clean(p) {
			return true
		}
	}
	return false
}

func labelsMatch(want, have map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}
