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

// Package planner translates external inputs into FaultManifests / AttackPlans.
// translate.go is the directed-mode path (plain-text → FaultManifest);
// generate.go is the autonomous-mode path (cluster context → AttackPlan).
package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/go-steer/simian-agent/pkg/simian"
)

// IntentInput is a directed-mode translation request.
type IntentInput struct {
	Intent           string                // free-text user intent
	Catalog          []simian.CatalogEntry // available faults the LLM may target
	DefaultDuration  time.Duration         // used if the LLM does not pick one
	DefaultNamespace string                // used when the user's intent does not name a namespace
}

// Translator converts a plain-text intent into a validated FaultManifest by
// asking the LLM for structured output.
type Translator struct {
	LLM        simian.LLMProvider
	Model      string // optional model override
	MaxRetries int    // schema-invalid retries; default 1
	// LogResponses, if set, is invoked with each raw LLM structured response.
	// Used for debugging; production code should leave nil.
	LogResponses func(attempt int, raw []byte)
}

// New constructs a Translator with the given provider.
func New(llm simian.LLMProvider) *Translator {
	return &Translator{LLM: llm, MaxRetries: 1}
}

// Translate returns a single FaultManifest. The returned manifest still has to
// pass Fault Executor validation; this function only ensures the LLM's output
// is well-formed.
func (t *Translator) Translate(ctx context.Context, in IntentInput) (simian.FaultManifest, error) {
	if in.Intent == "" {
		return simian.FaultManifest{}, fmt.Errorf("translator: intent is empty")
	}
	if len(in.Catalog) == 0 {
		return simian.FaultManifest{}, fmt.Errorf("translator: empty catalog — no faults are installed or permitted")
	}

	system := buildSystemPrompt(in.Catalog)
	user := buildUserPrompt(in.Intent, in.DefaultDuration, in.DefaultNamespace)

	var lastErr error
	for attempt := 0; attempt <= t.MaxRetries; attempt++ {
		// We deliberately do NOT pass ResponseSchema here. With Gemini's strict
		// structured-output mode, schema-constrained generation cannot fill
		// nested object properties not enumerated in the schema — and we want
		// the LLM to populate engine-specific spec fields from its training
		// knowledge of Chaos Mesh / Litmus, not from a hardcoded schema.
		// We still parse + validate the returned JSON ourselves below.
		req := simian.CompletionRequest{
			System:      system,
			Messages:    []simian.Message{{Role: "user", Content: user}},
			Temperature: 0.2,
			MaxTokens:   8192,
			Model:       t.Model,
		}
		resp, err := t.LLM.Complete(ctx, req)
		if err != nil {
			return simian.FaultManifest{}, fmt.Errorf("translator: LLM call failed: %w", err)
		}
		var raw []byte
		switch {
		case len(resp.Structured) > 0:
			raw = resp.Structured
		default:
			raw = extractJSON(resp.Text)
		}
		if t.LogResponses != nil {
			t.LogResponses(attempt, raw)
		}
		manifest, perr := parseManifest(raw, in.DefaultDuration)
		if perr == nil {
			return manifest, nil
		}
		lastErr = perr
		// On parse failure, append a corrective user turn and retry.
		user = user + "\n\nYour previous response failed validation: " + perr.Error() +
			"\nReturn a JSON object that conforms to the schema."
	}
	return simian.FaultManifest{}, fmt.Errorf("translator: exhausted retries: %w", lastErr)
}

// extractJSON pulls a JSON object out of the LLM's text response, tolerating
// surrounding markdown code fences and stray prose.
func extractJSON(text string) []byte {
	t := strings.TrimSpace(text)
	// Strip ```json ... ``` or ``` ... ``` fences.
	if strings.HasPrefix(t, "```") {
		if idx := strings.Index(t, "\n"); idx >= 0 {
			t = t[idx+1:]
		}
		if end := strings.LastIndex(t, "```"); end >= 0 {
			t = t[:end]
		}
		t = strings.TrimSpace(t)
	}
	// Find the first `{` and the matching last `}`.
	start := strings.Index(t, "{")
	end := strings.LastIndex(t, "}")
	if start < 0 || end < 0 || end <= start {
		return []byte(t)
	}
	return []byte(t[start : end+1])
}

func buildSystemPrompt(cat []simian.CatalogEntry) string {
	var sb strings.Builder
	sb.WriteString(`You are Simian Agent's intent translator. Your job is to convert a user's plain-text chaos engineering request into a single, valid FaultManifest JSON object.

Rules you MUST follow:
1. Choose exactly one fault type from the catalog provided. Do not invent fault types.
2. The "engine" field must match the catalog entry's engine.
3. The "api_version" and "resource_kind" fields must match the catalog entry exactly.
4. The "spec" field must be the engine-native spec for the chosen fault type — populated with all REQUIRED fields. NEVER return an empty spec object. Use the canonical examples below as templates.
5. Always populate "targets" with at least one TargetRef. For "namespace": if the user names one in the intent, use that; otherwise use the default namespace given to you in the user prompt. The same namespace MUST also appear inside the spec selector (e.g. PodChaos.spec.selector.namespaces). NEVER use the literal string "default" as the namespace unless the user's intent text explicitly says "default" — when in doubt, use the default namespace from the user prompt.
6. Set "name" on each target to the workload (Deployment / StatefulSet) name when one is named in the intent.
7. Set "duration" as a Go duration string (e.g. "30s", "2m", "5m").
8. Set "rationale" to one sentence explaining what you chose and why.
9. Output ONLY the JSON object — no commentary, no markdown.

Canonical Chaos Mesh spec templates (copy these and adapt to the user's intent):

PodChaos — kill / fail / container-kill:
{
  "action": "pod-kill",          // or "pod-failure" or "container-kill"
  "mode": "one",                 // or "all" | "fixed" | "fixed-percent" | "random-max-percent"
  "selector": {
    "namespaces": ["<namespace>"],
    "labelSelectors": {"app": "<workload>"}
  }
}

NetworkChaos — delay / loss / corrupt / duplicate:
{
  "action": "delay",             // or "loss" | "corrupt" | "duplicate" | "bandwidth"
  "mode": "all",
  "selector": {
    "namespaces": ["<namespace>"],
    "labelSelectors": {"app": "<workload>"}
  },
  "delay": {"latency": "250ms", "correlation": "0", "jitter": "0ms"}
  // For "loss": "loss": {"loss": "10", "correlation": "0"}
  // For "bandwidth": "bandwidth": {"rate": "1mbps", "limit": 20971520, "buffer": 10000}
}

StressChaos — CPU or memory stress:
{
  "mode": "one",
  "selector": {
    "namespaces": ["<namespace>"],
    "labelSelectors": {"app": "<workload>"}
  },
  "stressors": {
    "cpu": {"workers": 2, "load": 80}
    // or "memory": {"workers": 2, "size": "256MB"}
  }
}

IOChaos — file-system latency / faults:
{
  "action": "latency",           // or "fault" | "attrOverride" | "mistake"
  "mode": "one",
  "selector": {
    "namespaces": ["<namespace>"],
    "labelSelectors": {"app": "<workload>"}
  },
  "volumePath": "/data",
  "path": "/data/**",
  "delay": "100ms",
  "percent": 100
}

TimeChaos — clock skew:
{
  "mode": "one",
  "selector": {
    "namespaces": ["<namespace>"],
    "labelSelectors": {"app": "<workload>"}
  },
  "timeOffset": "-10m"
}

For other fault types not shown, consult chaos-mesh.org docs and use the same shape conventions: a "selector" block, an action verb if applicable, and an action-specific parameter block.

Available fault catalog (kinds you may choose from):
`)
	for _, e := range cat {
		fmt.Fprintf(&sb, "- engine=%s kind=%s api_version=%s tier=%s\n",
			e.Engine, e.ResourceKind, e.APIVersion, e.BlastRadiusTier)
	}
	return sb.String()
}

func buildUserPrompt(intent string, defaultDuration time.Duration, defaultNamespace string) string {
	dur := "2m"
	if defaultDuration > 0 {
		dur = defaultDuration.String()
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "User intent: %q\n\nIf the user did not specify a duration, default to %s.", intent, dur)
	if defaultNamespace != "" {
		fmt.Fprintf(&sb, "\nIf the user did not name a namespace, default to %q (NOT \"default\").", defaultNamespace)
	}
	return sb.String()
}

func parseManifest(raw json.RawMessage, defaultDuration time.Duration) (simian.FaultManifest, error) {
	if len(raw) == 0 {
		return simian.FaultManifest{}, fmt.Errorf("empty structured response")
	}
	// Decode into a permissive intermediate so we can normalize duration.
	var tmp struct {
		Engine          string             `json:"engine"`
		APIVersion      string             `json:"api_version"`
		ResourceKind    string             `json:"resource_kind"`
		Kind            string             `json:"kind"` // accept "kind" as alias
		Spec            map[string]any     `json:"spec"`
		Targets         []simian.TargetRef `json:"targets"`
		DurationStr     string             `json:"duration"`
		BlastRadiusTier string             `json:"blast_radius_tier"`
		Rationale       string             `json:"rationale"`
	}
	if err := json.Unmarshal(raw, &tmp); err != nil {
		return simian.FaultManifest{}, fmt.Errorf("decode: %w", err)
	}
	if tmp.ResourceKind == "" {
		tmp.ResourceKind = tmp.Kind
	}
	if tmp.Engine == "" || tmp.ResourceKind == "" || tmp.APIVersion == "" {
		return simian.FaultManifest{}, fmt.Errorf("engine, api_version, and resource_kind (or kind) are all required")
	}
	if len(tmp.Targets) == 0 {
		return simian.FaultManifest{}, fmt.Errorf("at least one target is required")
	}
	if tmp.Spec == nil {
		return simian.FaultManifest{}, fmt.Errorf("spec is required")
	}
	dur := defaultDuration
	if tmp.DurationStr != "" {
		d, err := time.ParseDuration(tmp.DurationStr)
		if err != nil {
			return simian.FaultManifest{}, fmt.Errorf("invalid duration %q: %w", tmp.DurationStr, err)
		}
		dur = d
	}
	if dur <= 0 {
		dur = 2 * time.Minute
	}
	tier := simian.BlastRadiusTier(tmp.BlastRadiusTier)
	return simian.FaultManifest{
		Source:          simian.SourceDirected,
		Engine:          simian.Engine(tmp.Engine),
		APIVersion:      tmp.APIVersion,
		ResourceKind:    tmp.ResourceKind,
		Spec:            tmp.Spec,
		Targets:         tmp.Targets,
		Duration:        dur,
		BlastRadiusTier: tier,
		Rationale:       tmp.Rationale,
	}, nil
}
