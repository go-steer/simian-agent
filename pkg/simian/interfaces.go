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

package simian

import (
	"context"
	"encoding/json"
)

// FaultExecutor is the chokepoint between any fault source and the chaos
// drivers. Per design.md §3 it is the only code path that may invoke a driver.
type FaultExecutor interface {
	// Apply runs the full pipeline: schema validate, safety validate, audit
	// pre-apply, driver.Apply, lease register, audit post-apply. Returns the
	// executor-assigned fault UID on success, or a typed *ExecutorError.
	Apply(ctx context.Context, m FaultManifest) (faultUID string, err error)

	// Clear removes an active fault before its lease expires. Idempotent.
	Clear(ctx context.Context, faultUID string) error

	// ListActive returns the current set of leased faults. Optional namespace
	// filter; empty matches all eligible namespaces.
	ListActive(ctx context.Context, namespace string) ([]ActiveFault, error)
}

// ChaosDriver is a thin engine adapter. It performs no validation, no audit,
// no lifecycle — those concerns live in the executor.
type ChaosDriver interface {
	Engine() Engine
	Apply(ctx context.Context, m FaultManifest) (engineUID string, err error)
	Clear(ctx context.Context, engineUID string) error
	Catalog(ctx context.Context) ([]CatalogEntry, error)
}

// CompletionRequest is the LLMProvider input. Tools are read-only context tools
// the model may call during reasoning. ResponseSchema is the JSON Schema for
// structured output (e.g. the FaultManifest schema).
type CompletionRequest struct {
	System         string
	Messages       []Message
	Tools          []ToolDef
	ResponseSchema json.RawMessage
	Temperature    float32
	MaxTokens      int
	Model          string // optional override; provider has a default
}

// Message is one chat turn. Role is "system", "user", "assistant", or "tool".
type Message struct {
	Role    string
	Content string
	// ToolCallID is set on tool-result messages; matches a prior ToolCall.ID.
	ToolCallID string
	Name       string
}

// ToolDef describes a function the model may call. The provider is responsible
// for shaping this into the engine-specific function/tool format.
type ToolDef struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// ToolCall is a request from the model to invoke a registered tool. The caller
// is responsible for executing it and feeding the result back as a Message
// with role="tool" and ToolCallID matching ID.
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// CompletionResponse is the LLMProvider output. Exactly one of Text and
// Structured is populated depending on whether ResponseSchema was set in the
// request. ToolCalls is populated when the model wants the caller to invoke
// tools and feed results back in another Complete() call.
type CompletionResponse struct {
	Text       string
	Structured json.RawMessage
	ToolCalls  []ToolCall
	Usage      TokenUsage
}

// TokenUsage is reported for telemetry and cost tracking.
type TokenUsage struct {
	InputTokens  int
	OutputTokens int
}

// LLMProvider is the pluggable LLM interface. Per requirements R-LLM-01 the
// interface is intentionally low-level — a single completion with optional
// structured output and tool calling. Higher-level operations (translate
// intent, generate plan, generate incident page) compose this in the planner
// and redphone packages.
type LLMProvider interface {
	Name() string
	Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error)
}

// EventDispatcher is the Red Phone outbound transport (M5). Pulled into the
// shared interface set so executor / planner code can hold the dependency
// without importing the concrete redphone package.
type EventDispatcher interface {
	Dispatch(ctx context.Context, n IncidentNotification) error
}

// Auditor is the append-only structured audit log. Implementations write to
// slog by default; alternative sinks (Cloud Logging, file) are pluggable.
type Auditor interface {
	Emit(ctx context.Context, event AuditEvent)
}

// AuditEvent is the canonical structure of one audit-log line.
type AuditEvent struct {
	Event      string         `json:"event"`
	FaultUID   string         `json:"fault_uid,omitempty"`
	PlanID     string         `json:"plan_id,omitempty"`
	ScenarioID string         `json:"scenario_id,omitempty"`
	Mode       ManifestSource `json:"mode,omitempty"`
	Reason     string         `json:"reason,omitempty"`
	Payload    map[string]any `json:"payload,omitempty"`
}
