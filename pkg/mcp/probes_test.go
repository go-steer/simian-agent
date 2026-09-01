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

package mcp

import (
	"context"
	"testing"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"

	"github.com/go-steer/simian-agent/pkg/simian"
)

// recordingExecutor keeps the manifest it was handed.
type recordingExecutor struct {
	got simian.FaultManifest
}

func (r *recordingExecutor) Apply(_ context.Context, m simian.FaultManifest) (string, error) {
	r.got = m
	return "f-recorded", nil
}
func (r *recordingExecutor) Clear(context.Context, string) error { return nil }
func (r *recordingExecutor) ListActive(context.Context, string) ([]simian.ActiveFault, error) {
	return nil, nil
}

// TestSubmitManifestCarriesSettleProbesToTheExecutor guards the wiring rather
// than the logic. The efficacy gate is only ever reached if the probes survive
// the JSON round-trip through submit_manifest; a manifest whose probes were
// dropped in transit applies happily and unverified, which is exactly the
// silent-success failure the gate exists to prevent.
func TestSubmitManifestCarriesSettleProbesToTheExecutor(t *testing.T) {
	rec := &recordingExecutor{}
	s := New(rec, map[simian.Engine]simian.ChaosDriver{}, nil, nil, "test")

	manifest := map[string]any{
		"engine":        "chaos-mesh",
		"api_version":   "chaos-mesh.org/v1alpha1",
		"resource_kind": "PodChaos",
		"duration":      "2m",
		"spec":          map[string]any{"action": "pod-failure"},
		"targets":       []any{map[string]any{"namespace": "boutique", "name": "payments"}},
		"probes": []any{map[string]any{
			"name": "crashloop",
			"type": "k8s",
			"mode": "Settle",
			"spec": map[string]any{
				"resource":        "pods",
				"jsonpath":        "{.items[*].status.containerStatuses[*].state.waiting.reason}",
				"expect_contains": "CrashLoopBackOff",
				"timeout":         "90s",
			},
		}},
	}

	req := mcpsdk.CallToolRequest{}
	req.Params.Name = "submit_manifest"
	req.Params.Arguments = map[string]any{"manifest": manifest}
	res, err := s.handleSubmitManifest(context.Background(), req)
	if err != nil {
		t.Fatalf("handleSubmitManifest: %v", err)
	}
	if res.IsError {
		t.Fatalf("submit_manifest returned an error result: %+v", res.Content)
	}

	settle := rec.got.SettleProbes()
	if len(settle) != 1 {
		t.Fatalf("executor received %d Settle probe(s), want 1 — probes were dropped in transit", len(settle))
	}
	p := settle[0]
	if p.Name != "crashloop" || p.Type != simian.ProbeTypeK8s {
		t.Errorf("probe = %+v, want name=crashloop type=k8s", p)
	}
	// The Spec map is what the prober actually reads; an empty one would make
	// the gate fail for the wrong reason.
	if got := p.Spec["expect_contains"]; got != "CrashLoopBackOff" {
		t.Errorf("probe spec expect_contains=%v, want CrashLoopBackOff", got)
	}
	if got := p.Spec["jsonpath"]; got == nil || got == "" {
		t.Error("probe spec lost its jsonpath")
	}
}
