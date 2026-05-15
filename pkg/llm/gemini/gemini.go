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

// Package gemini implements simian.LLMProvider over the unified Google GenAI
// Go SDK (google.golang.org/genai). Both Vertex/ADC and Gemini Developer API
// (API key) modes are supported via Mode in Config.
//
// Mode is normally inferred from environment when not set in Config:
//   - GOOGLE_GENAI_USE_VERTEXAI=true → Vertex backend; uses GOOGLE_CLOUD_PROJECT
//     and VERTEX_LOCATION (or GOOGLE_CLOUD_LOCATION).
//   - GEMINI_API_KEY set → Gemini API backend.
package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"

	"google.golang.org/genai"

	"github.com/go-steer/simian-agent/pkg/simian"
)

// Mode selects between the Vertex AI and Gemini Developer API backends.
type Mode string

const (
	ModeUnspecified Mode = ""
	ModeVertex      Mode = "vertex"
	ModeAPIKey      Mode = "api-key"
)

// Config governs Gemini client construction.
type Config struct {
	Mode Mode

	// Vertex-only.
	Project  string
	Location string

	// API-key-only.
	APIKey string

	// DefaultModel is used when CompletionRequest.Model is empty. Defaults to
	// "gemini-2.5-pro".
	DefaultModel string
}

// Provider implements simian.LLMProvider.
type Provider struct {
	cfg    Config
	client *genai.Client
}

// New constructs a Gemini provider, inferring Mode from environment when not
// set explicitly. The constructor performs no LLM call.
func New(ctx context.Context, cfg Config) (*Provider, error) {
	if cfg.DefaultModel == "" {
		cfg.DefaultModel = "gemini-2.5-pro"
	}
	if cfg.Mode == ModeUnspecified {
		cfg.Mode = inferMode(cfg)
	}

	clientCfg := &genai.ClientConfig{}
	switch cfg.Mode {
	case ModeVertex:
		project := cfg.Project
		if project == "" {
			project = firstEnv("GOOGLE_CLOUD_PROJECT", "GCP_PROJECT", "PROJECT_ID")
		}
		if project == "" {
			return nil, fmt.Errorf("gemini: vertex mode requires Project (or GOOGLE_CLOUD_PROJECT)")
		}
		location := cfg.Location
		if location == "" {
			location = firstEnv("VERTEX_LOCATION", "GOOGLE_CLOUD_LOCATION")
		}
		if location == "" {
			location = "us-central1"
		}
		clientCfg.Backend = genai.BackendVertexAI
		clientCfg.Project = project
		clientCfg.Location = location
	case ModeAPIKey:
		key := cfg.APIKey
		if key == "" {
			key = firstEnv("GEMINI_API_KEY", "GOOGLE_API_KEY")
		}
		if key == "" {
			return nil, fmt.Errorf("gemini: api-key mode requires APIKey (or GEMINI_API_KEY)")
		}
		clientCfg.Backend = genai.BackendGeminiAPI
		clientCfg.APIKey = key
	default:
		return nil, fmt.Errorf("gemini: unrecognized mode %q", cfg.Mode)
	}

	client, err := genai.NewClient(ctx, clientCfg)
	if err != nil {
		return nil, fmt.Errorf("gemini: new client: %w", err)
	}
	return &Provider{cfg: cfg, client: client}, nil
}

// Name implements simian.LLMProvider.
func (p *Provider) Name() string { return "gemini:" + string(p.cfg.Mode) }

// Complete implements simian.LLMProvider. M1 supports the system + user/assistant
// turns, structured-output via ResponseSchema, and basic function tools. The
// streaming variant is reserved for a later milestone.
func (p *Provider) Complete(ctx context.Context, req simian.CompletionRequest) (simian.CompletionResponse, error) {
	model := req.Model
	if model == "" {
		model = p.cfg.DefaultModel
	}

	contents := messagesToContents(req.Messages)
	gcfg := &genai.GenerateContentConfig{}
	if req.System != "" {
		gcfg.SystemInstruction = &genai.Content{
			Parts: []*genai.Part{{Text: req.System}},
		}
	}
	if req.Temperature > 0 {
		t := req.Temperature
		gcfg.Temperature = &t
	}
	if req.MaxTokens > 0 {
		maxOut := req.MaxTokens
		if maxOut > math.MaxInt32 {
			maxOut = math.MaxInt32
		}
		gcfg.MaxOutputTokens = int32(maxOut) //nolint:gosec // bounded above
	}
	if len(req.ResponseSchema) > 0 {
		schema, err := jsonSchemaToGenaiSchema(req.ResponseSchema)
		if err != nil {
			return simian.CompletionResponse{}, fmt.Errorf("gemini: response schema: %w", err)
		}
		gcfg.ResponseMIMEType = "application/json"
		gcfg.ResponseSchema = schema
	}
	if len(req.Tools) > 0 {
		tools, err := toolDefsToGenaiTools(req.Tools)
		if err != nil {
			return simian.CompletionResponse{}, fmt.Errorf("gemini: tool defs: %w", err)
		}
		gcfg.Tools = tools
	}

	resp, err := p.client.Models.GenerateContent(ctx, model, contents, gcfg)
	if err != nil {
		return simian.CompletionResponse{}, fmt.Errorf("gemini: generate content: %w", err)
	}

	out := simian.CompletionResponse{}
	if resp.UsageMetadata != nil {
		out.Usage = simian.TokenUsage{
			InputTokens:  int(resp.UsageMetadata.PromptTokenCount),
			OutputTokens: int(resp.UsageMetadata.CandidatesTokenCount),
		}
	}
	if len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
		return out, nil
	}
	for _, part := range resp.Candidates[0].Content.Parts {
		if part.Text != "" {
			if len(req.ResponseSchema) > 0 {
				out.Structured = json.RawMessage(part.Text)
			} else {
				out.Text += part.Text
			}
		}
		if part.FunctionCall != nil {
			argsB, _ := json.Marshal(part.FunctionCall.Args)
			out.ToolCalls = append(out.ToolCalls, simian.ToolCall{
				ID:        part.FunctionCall.ID,
				Name:      part.FunctionCall.Name,
				Arguments: argsB,
			})
		}
	}
	return out, nil
}

func inferMode(cfg Config) Mode {
	if cfg.APIKey != "" {
		return ModeAPIKey
	}
	if strings.EqualFold(os.Getenv("GOOGLE_GENAI_USE_VERTEXAI"), "true") {
		return ModeVertex
	}
	if os.Getenv("GEMINI_API_KEY") != "" {
		return ModeAPIKey
	}
	if os.Getenv("GOOGLE_CLOUD_PROJECT") != "" {
		return ModeVertex
	}
	return ModeVertex
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func messagesToContents(messages []simian.Message) []*genai.Content {
	out := make([]*genai.Content, 0, len(messages))
	for _, m := range messages {
		role := m.Role
		switch role {
		case "assistant":
			role = "model"
		case "tool":
			role = "function"
		}
		c := &genai.Content{Role: role}
		if m.ToolCallID != "" {
			c.Parts = append(c.Parts, &genai.Part{
				FunctionResponse: &genai.FunctionResponse{
					Name:     m.Name,
					Response: map[string]any{"content": m.Content},
				},
			})
		} else {
			c.Parts = append(c.Parts, &genai.Part{Text: m.Content})
		}
		out = append(out, c)
	}
	return out
}

// jsonSchemaToGenaiSchema converts a JSON-Schema-shaped raw message into a
// genai.Schema. M1 supports the subset needed by FaultManifest and AttackPlan
// schemas (object/array/string/integer/number/boolean with properties, items,
// required, enum).
func jsonSchemaToGenaiSchema(raw json.RawMessage) (*genai.Schema, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return mapToSchema(m)
}

func mapToSchema(m map[string]any) (*genai.Schema, error) {
	s := &genai.Schema{}
	if t, ok := m["type"].(string); ok {
		s.Type = jsonSchemaTypeToGenai(t)
	}
	if d, ok := m["description"].(string); ok {
		s.Description = d
	}
	if e, ok := m["enum"].([]any); ok {
		for _, v := range e {
			if str, ok := v.(string); ok {
				s.Enum = append(s.Enum, str)
			}
		}
	}
	if r, ok := m["required"].([]any); ok {
		for _, v := range r {
			if str, ok := v.(string); ok {
				s.Required = append(s.Required, str)
			}
		}
	}
	if items, ok := m["items"].(map[string]any); ok {
		child, err := mapToSchema(items)
		if err != nil {
			return nil, err
		}
		s.Items = child
	}
	if props, ok := m["properties"].(map[string]any); ok {
		s.Properties = map[string]*genai.Schema{}
		for k, v := range props {
			vmap, ok := v.(map[string]any)
			if !ok {
				continue
			}
			child, err := mapToSchema(vmap)
			if err != nil {
				return nil, err
			}
			s.Properties[k] = child
		}
	}
	return s, nil
}

func jsonSchemaTypeToGenai(t string) genai.Type {
	switch strings.ToLower(t) {
	case "string":
		return genai.TypeString
	case "integer":
		return genai.TypeInteger
	case "number":
		return genai.TypeNumber
	case "boolean":
		return genai.TypeBoolean
	case "array":
		return genai.TypeArray
	case "object":
		return genai.TypeObject
	case "null":
		return genai.TypeNULL
	default:
		return genai.TypeUnspecified
	}
}

func toolDefsToGenaiTools(defs []simian.ToolDef) ([]*genai.Tool, error) {
	tool := &genai.Tool{}
	for _, d := range defs {
		schema, err := jsonSchemaToGenaiSchema(d.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("tool %q: %w", d.Name, err)
		}
		tool.FunctionDeclarations = append(tool.FunctionDeclarations, &genai.FunctionDeclaration{
			Name:        d.Name,
			Description: d.Description,
			Parameters:  schema,
		})
	}
	return []*genai.Tool{tool}, nil
}
