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

// Package stub provides a deterministic LLMProvider implementation for tests
// and for environments without LLM credentials. The stub returns canned
// responses keyed by request signature.
package stub

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/go-steer/simian-agent/pkg/simian"
)

// Provider is an in-memory test double for simian.LLMProvider.
type Provider struct {
	NameStr string

	mu        sync.Mutex
	responses []ResponseRule
	calls     []simian.CompletionRequest
}

// ResponseRule pairs a matcher with the response to return when the matcher
// fires. Rules are evaluated in registration order; the first match wins.
type ResponseRule struct {
	Match    func(req simian.CompletionRequest) bool
	Response simian.CompletionResponse
}

// New constructs a stub provider. name defaults to "stub".
func New(name string) *Provider {
	if name == "" {
		name = "stub"
	}
	return &Provider{NameStr: name}
}

// Name implements LLMProvider.
func (p *Provider) Name() string { return p.NameStr }

// AddRule registers a matcher → response rule.
func (p *Provider) AddRule(rule ResponseRule) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.responses = append(p.responses, rule)
}

// AlwaysReturnStructured installs a single rule that returns the given
// JSON-serializable value as the structured response for any request.
func (p *Provider) AlwaysReturnStructured(value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	p.AddRule(ResponseRule{
		Match: func(_ simian.CompletionRequest) bool { return true },
		Response: simian.CompletionResponse{
			Structured: b,
		},
	})
	return nil
}

// AlwaysReturnText installs a single rule that returns the given text for any
// request.
func (p *Provider) AlwaysReturnText(text string) {
	p.AddRule(ResponseRule{
		Match:    func(_ simian.CompletionRequest) bool { return true },
		Response: simian.CompletionResponse{Text: text},
	})
}

// Calls returns a snapshot of all received requests in order. Useful for
// assertion in tests.
func (p *Provider) Calls() []simian.CompletionRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]simian.CompletionRequest, len(p.calls))
	copy(out, p.calls)
	return out
}

// Complete implements LLMProvider.
func (p *Provider) Complete(_ context.Context, req simian.CompletionRequest) (simian.CompletionResponse, error) {
	p.mu.Lock()
	p.calls = append(p.calls, req)
	rules := p.responses
	p.mu.Unlock()
	for _, r := range rules {
		if r.Match == nil || r.Match(req) {
			return r.Response, nil
		}
	}
	return simian.CompletionResponse{}, fmt.Errorf("stub: no matching rule for request")
}
