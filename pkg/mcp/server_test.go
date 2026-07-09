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
	"encoding/json"
	"errors"
	"testing"
	"time"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"

	"github.com/go-steer/simian-agent/pkg/executor"
	"github.com/go-steer/simian-agent/pkg/simian"
	"github.com/go-steer/simian-agent/pkg/sut"
	"github.com/go-steer/simian-agent/pkg/topology"
)

// stubExecutor satisfies simian.FaultExecutor with no-op semantics — server
// tests don't need fault dispatch to work, just construction.
type stubExecutor struct{}

func (stubExecutor) Apply(context.Context, simian.FaultManifest) (string, error) { return "", nil }
func (stubExecutor) Clear(context.Context, string) error                         { return nil }
func (stubExecutor) ListActive(context.Context, string) ([]simian.ActiveFault, error) {
	return nil, nil
}

type fakeTopology struct {
	calls int
	err   error
}

func (f *fakeTopology) Snapshot(_ context.Context, ns string) (*topology.TargetTopology, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &topology.TargetTopology{
		Namespace:    ns,
		DiscoveredAt: time.Now().UTC(),
		Workloads:    []topology.Workload{{Kind: "Deployment", Name: "frontend"}},
		Services:     []topology.Service{{Name: "frontend"}},
	}, nil
}

type fakeRecents struct {
	out []executor.RecentFault
}

func (f *fakeRecents) Recent(_ string, _ int) []executor.RecentFault { return f.out }

type fakeEstablisher struct {
	called   int
	gotNS    string
	gotSUT   string
	returnBL *sut.Baseline
	err      error
}

func (f *fakeEstablisher) Deploy(_ context.Context, opts sut.DeployOptions) (*sut.Baseline, error) {
	f.called++
	f.gotNS = opts.Namespace
	f.gotSUT = opts.SUTName
	if f.err != nil {
		return nil, f.err
	}
	return f.returnBL, nil
}

func (f *fakeEstablisher) EstablishBaselineFromTopology(_ context.Context, namespace string, _ sut.BaselineConfig) (*sut.Baseline, error) {
	f.called++
	f.gotNS = namespace
	f.gotSUT = "" // topology path has no SUT
	if f.err != nil {
		return nil, f.err
	}
	return f.returnBL, nil
}

func newServer(t *testing.T, opts ...Option) *Server {
	t.Helper()
	return New(stubExecutor{}, map[simian.Engine]simian.ChaosDriver{}, nil, nil, "test", opts...)
}

func callTool(t *testing.T, s *Server, name string, args map[string]any) string {
	t.Helper()
	// Resolve the registered handler by walking the underlying mcp server's
	// tool list — but mcp-go does not expose a public "GetHandler" API, so we
	// reach for the in-process method dispatch directly via the typed handlers
	// on the Server. This keeps the test independent of mcp-go protocol
	// plumbing while still exercising the production code paths.
	req := mcpsdk.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	var (
		res *mcpsdk.CallToolResult
		err error
	)
	switch name {
	case "get_topology":
		res, err = s.handleGetTopology(context.Background(), req)
	case "get_metrics":
		res, err = s.handleGetMetrics(context.Background(), req)
	case "get_recent_faults":
		res, err = s.handleGetRecentFaults(context.Background(), req)
	case "establish_baseline":
		res, err = s.handleEstablishBaseline(context.Background(), req)
	case "get_baseline":
		res, err = s.handleGetBaseline(context.Background(), req)
	default:
		t.Fatalf("unhandled tool %q in test dispatch", name)
	}
	if err != nil {
		t.Fatalf("%s handler returned error: %v", name, err)
	}
	if res == nil || len(res.Content) == 0 {
		t.Fatalf("%s returned empty result", name)
	}
	tc, ok := res.Content[0].(mcpsdk.TextContent)
	if !ok {
		t.Fatalf("%s content[0] is %T, want TextContent", name, res.Content[0])
	}
	return tc.Text
}

func TestGetMetrics_AlwaysStub(t *testing.T) {
	s := newServer(t)
	got := callTool(t, s, "get_metrics", map[string]any{"query": "up"})
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("unmarshal: %v; raw=%q", err, got)
	}
	if parsed["configured"] != false {
		t.Errorf("configured = %v, want false", parsed["configured"])
	}
}

func TestGetTopology_NoLookupReturnsDisabled(t *testing.T) {
	s := newServer(t)
	got := callTool(t, s, "get_topology", map[string]any{"namespace": "ns"})
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed["enabled"] != false {
		t.Errorf("enabled = %v, want false", parsed["enabled"])
	}
}

func TestGetTopology_DelegatesToLookup(t *testing.T) {
	ft := &fakeTopology{}
	s := newServer(t, WithTopology(ft))
	got := callTool(t, s, "get_topology", map[string]any{"namespace": "boutique"})
	if ft.calls != 1 {
		t.Errorf("Snapshot calls=%d, want 1", ft.calls)
	}
	var snap topology.TargetTopology
	if err := json.Unmarshal([]byte(got), &snap); err != nil {
		t.Fatalf("unmarshal: %v; raw=%q", err, got)
	}
	if snap.Namespace != "boutique" {
		t.Errorf("Namespace=%q, want boutique", snap.Namespace)
	}
}

func TestGetTopology_PropagatesError(t *testing.T) {
	ft := &fakeTopology{err: errors.New("boom")}
	s := newServer(t, WithTopology(ft))
	got := callTool(t, s, "get_topology", map[string]any{"namespace": "ns"})
	if got == "" {
		t.Fatal("expected error text")
	}
}

func TestGetRecentFaults_NoLookupReturnsEmpty(t *testing.T) {
	s := newServer(t)
	got := callTool(t, s, "get_recent_faults", map[string]any{"namespace": "ns"})
	var parsed struct {
		Enabled bool                   `json:"enabled"`
		Recent  []executor.RecentFault `json:"recent"`
	}
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.Enabled || len(parsed.Recent) != 0 {
		t.Errorf("got enabled=%v recent=%v, want false / empty", parsed.Enabled, parsed.Recent)
	}
}

func TestGetRecentFaults_ReturnsList(t *testing.T) {
	s := newServer(t, WithRecents(&fakeRecents{
		out: []executor.RecentFault{{FaultUID: "f-1", AppliedAt: time.Now()}},
	}))
	got := callTool(t, s, "get_recent_faults", map[string]any{"namespace": "ns", "limit": float64(10)})
	var parsed struct {
		Enabled bool                   `json:"enabled"`
		Recent  []executor.RecentFault `json:"recent"`
	}
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("unmarshal: %v; raw=%q", err, got)
	}
	if !parsed.Enabled || len(parsed.Recent) != 1 || parsed.Recent[0].FaultUID != "f-1" {
		t.Errorf("got %+v, want one entry f-1", parsed)
	}
}

func TestEstablishBaseline_NoEstablisher(t *testing.T) {
	s := newServer(t)
	got := callTool(t, s, "establish_baseline", map[string]any{"namespace": "ns", "sut": "online-boutique"})
	if got == "" {
		t.Fatal("expected error text")
	}
}

func TestEstablishBaseline_DelegatesToManager(t *testing.T) {
	bl := &sut.Baseline{Namespace: "ns", SUT: "online-boutique"}
	fe := &fakeEstablisher{returnBL: bl}
	s := newServer(t, WithBaselineEstablisher(fe))
	got := callTool(t, s, "establish_baseline", map[string]any{"namespace": "ns", "sut": "online-boutique"})
	if fe.called != 1 || fe.gotNS != "ns" || fe.gotSUT != "online-boutique" {
		t.Errorf("Deploy called=%d ns=%q sut=%q", fe.called, fe.gotNS, fe.gotSUT)
	}
	var parsed struct {
		Established bool         `json:"established"`
		Baseline    sut.Baseline `json:"baseline"`
	}
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("unmarshal: %v; raw=%q", err, got)
	}
	if !parsed.Established || parsed.Baseline.Namespace != "ns" {
		t.Errorf("got %+v, want established=true ns=ns", parsed)
	}
}

func TestEstablishBaseline_TopologyPathWhenSUTOmitted(t *testing.T) {
	bl := &sut.Baseline{Namespace: "payments", SUT: ""}
	fe := &fakeEstablisher{returnBL: bl}
	s := newServer(t, WithBaselineEstablisher(fe))
	got := callTool(t, s, "establish_baseline", map[string]any{"namespace": "payments"})
	if fe.called != 1 || fe.gotNS != "payments" || fe.gotSUT != "" {
		t.Errorf("EstablishBaselineFromTopology called=%d ns=%q sut=%q (want called=1 ns=payments sut=\"\")", fe.called, fe.gotNS, fe.gotSUT)
	}
	var parsed struct {
		Established bool         `json:"established"`
		Baseline    sut.Baseline `json:"baseline"`
	}
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("unmarshal: %v; raw=%q", err, got)
	}
	if !parsed.Established || parsed.Baseline.Namespace != "payments" || parsed.Baseline.SUT != "" {
		t.Errorf("got %+v, want established=true ns=payments sut=\"\"", parsed)
	}
}

func TestEstablishBaseline_NamespaceRequired(t *testing.T) {
	bl := &sut.Baseline{}
	fe := &fakeEstablisher{returnBL: bl}
	s := newServer(t, WithBaselineEstablisher(fe))
	got := callTool(t, s, "establish_baseline", map[string]any{})
	if got == "" {
		t.Fatal("expected error text about missing namespace")
	}
	if fe.called != 0 {
		t.Errorf("establisher should not have been called; got called=%d", fe.called)
	}
}
