//go:build integration

package gemini_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/go-steer/simian-agent/pkg/llm/gemini"
	"github.com/go-steer/simian-agent/pkg/simian"
)

// Run with: go test -tags=integration ./pkg/llm/gemini/...
//
// Requires Vertex/ADC credentials (GOOGLE_GENAI_USE_VERTEXAI=true,
// GOOGLE_CLOUD_PROJECT, VERTEX_LOCATION) or GEMINI_API_KEY.

func TestGeminiPlainText(t *testing.T) {
	if os.Getenv("GOOGLE_CLOUD_PROJECT") == "" && os.Getenv("GEMINI_API_KEY") == "" {
		t.Skip("no LLM credentials in env")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p, err := gemini.New(ctx, gemini.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := p.Complete(ctx, simian.CompletionRequest{
		System:      "You answer with a single word.",
		Messages:    []simian.Message{{Role: "user", Content: "What is the capital of France?"}},
		Temperature: 0.0,
		MaxTokens:   2048,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text == "" {
		t.Fatalf("expected non-empty Text, got %+v", resp)
	}
	t.Logf("text response: %q (in=%d out=%d)", resp.Text, resp.Usage.InputTokens, resp.Usage.OutputTokens)
}

func TestGeminiStructuredOutput(t *testing.T) {
	if os.Getenv("GOOGLE_CLOUD_PROJECT") == "" && os.Getenv("GEMINI_API_KEY") == "" {
		t.Skip("no LLM credentials in env")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p, err := gemini.New(ctx, gemini.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	schema := json.RawMessage(`{
		"type":"object",
		"required":["city","country"],
		"properties": {
			"city":{"type":"string"},
			"country":{"type":"string"}
		}
	}`)
	resp, err := p.Complete(ctx, simian.CompletionRequest{
		System:         "Return a JSON object with the capital city and its country.",
		Messages:       []simian.Message{{Role: "user", Content: "What is the capital of France?"}},
		ResponseSchema: schema,
		Temperature:    0.0,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.Structured) == 0 {
		t.Fatalf("expected structured response, got %+v", resp)
	}
	var parsed struct {
		City    string `json:"city"`
		Country string `json:"country"`
	}
	if err := json.Unmarshal(resp.Structured, &parsed); err != nil {
		t.Fatalf("unmarshal structured: %v (raw=%s)", err, resp.Structured)
	}
	if parsed.City == "" || parsed.Country == "" {
		t.Fatalf("expected populated city+country, got %+v", parsed)
	}
	t.Logf("structured response: %+v", parsed)
}
