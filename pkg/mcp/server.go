// Package mcp wires the Simian Agent toolset to a Model Context Protocol
// server. M1 exposes the directed-mode tools (submit_fault, clear_fault,
// list_active_faults, list_fault_catalog) plus the read-only catalog tool.
//
// The implementation uses github.com/mark3labs/mcp-go for protocol plumbing.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/go-steer/simian-agent/pkg/executor"
	"github.com/go-steer/simian-agent/pkg/planner"
	"github.com/go-steer/simian-agent/pkg/simian"
	"github.com/go-steer/simian-agent/pkg/sut"
	"github.com/go-steer/simian-agent/pkg/topology"
)

// BaselineLookup is the read-only interface the MCP server uses to expose
// SUT baselines via get_baseline. *sut.Manager satisfies it. nil is
// permitted — when nil, get_baseline returns {exists: false} for every
// namespace (useful for installations without M2 Part B wired in).
type BaselineLookup interface {
	Baseline(namespace string) (sut.Baseline, bool)
}

// TopologyLookup is the read-only interface the MCP server uses to expose
// per-namespace cluster topology via get_topology. *topology.Discoverer
// satisfies it. nil disables get_topology cleanly.
type TopologyLookup interface {
	Snapshot(ctx context.Context, namespace string) (*topology.TargetTopology, error)
}

// RecentLookup is the read-only interface backing get_recent_faults.
// *executor.Executor satisfies it via Recent(). nil disables the tool
// cleanly (returns empty list).
type RecentLookup interface {
	Recent(namespace string, limit int) []executor.RecentFault
}

// BaselineEstablisher is the write-side interface backing establish_baseline.
// *sut.Manager satisfies it via its existing Deploy method. nil disables the
// tool with a clear error.
type BaselineEstablisher interface {
	Deploy(ctx context.Context, opts sut.DeployOptions) (*sut.Baseline, error)
}

// Server bundles the Simian dependencies the tools need.
type Server struct {
	Executor    simian.FaultExecutor
	Drivers     map[simian.Engine]simian.ChaosDriver
	Translator  *planner.Translator
	Baselines   BaselineLookup       // optional
	Topology    TopologyLookup       // optional
	Recents     RecentLookup         // optional
	Establisher BaselineEstablisher  // optional
	Version     string

	mcpServer *server.MCPServer
}

// Option configures a Server at construction time.
type Option func(*Server)

// WithTopology wires the get_topology tool.
func WithTopology(t TopologyLookup) Option { return func(s *Server) { s.Topology = t } }

// WithRecents wires the get_recent_faults tool.
func WithRecents(r RecentLookup) Option { return func(s *Server) { s.Recents = r } }

// WithBaselineEstablisher wires the establish_baseline tool.
func WithBaselineEstablisher(e BaselineEstablisher) Option {
	return func(s *Server) { s.Establisher = e }
}

// New returns a Server with all currently-shipped tools registered.
func New(exec simian.FaultExecutor, drivers map[simian.Engine]simian.ChaosDriver, translator *planner.Translator, baselines BaselineLookup, version string, opts ...Option) *Server {
	if version == "" {
		version = "0.1.0"
	}
	s := &Server{
		Executor:   exec,
		Drivers:    drivers,
		Translator: translator,
		Baselines:  baselines,
		Version:    version,
	}
	for _, opt := range opts {
		opt(s)
	}
	s.mcpServer = server.NewMCPServer("simian-agent", version,
		server.WithToolCapabilities(true),
	)
	s.registerTools()
	return s
}

// MCP returns the underlying MCP server (stdio / SSE plumbing is the caller's
// choice).
func (s *Server) MCP() *server.MCPServer { return s.mcpServer }

// ServeStdio runs the server over stdio. Blocks until ctx is done or stdin
// closes.
func (s *Server) ServeStdio(ctx context.Context) error {
	stdio := server.NewStdioServer(s.mcpServer)
	return stdio.Listen(ctx, nil, nil)
}

// ServeSSE returns an SSE-ready server bound to addr (e.g. ":8081"). The
// returned *server.SSEServer's Start method blocks.
func (s *Server) ServeSSE(addr string) *server.SSEServer {
	return server.NewSSEServer(s.mcpServer, server.WithBaseURL("http://"+addr))
}

func (s *Server) registerTools() {
	s.mcpServer.AddTool(mcpsdk.NewTool("submit_fault",
		mcpsdk.WithDescription("Translate a plain-text chaos intent into a FaultManifest and apply it via the Fault Executor. The Executor enforces namespace eligibility, blast-radius tier policy, and duration ceiling — translation does not bypass safety."),
		mcpsdk.WithString("intent",
			mcpsdk.Required(),
			mcpsdk.Description("Plain-text intent — e.g. 'add 250ms latency to paymentservice for 2 minutes'."),
		),
		mcpsdk.WithString("default_namespace",
			mcpsdk.Description("Namespace to default the manifest target to if the intent does not name one."),
		),
		mcpsdk.WithString("default_duration",
			mcpsdk.Description("Default Go duration string (e.g. '2m') if the intent does not name one."),
		),
	), s.handleSubmitFault)

	s.mcpServer.AddTool(mcpsdk.NewTool("submit_manifest",
		mcpsdk.WithDescription("Apply a fully-formed FaultManifest, bypassing LLM translation. The deterministic-control path used by CI jobs and power users."),
		mcpsdk.WithObject("manifest",
			mcpsdk.Required(),
			mcpsdk.Description("FaultManifest JSON object."),
		),
	), s.handleSubmitManifest)

	s.mcpServer.AddTool(mcpsdk.NewTool("clear_fault",
		mcpsdk.WithDescription("Clear an active fault by its Simian fault UID."),
		mcpsdk.WithString("fault_uid", mcpsdk.Required()),
	), s.handleClearFault)

	s.mcpServer.AddTool(mcpsdk.NewTool("list_active_faults",
		mcpsdk.WithDescription("List currently leased faults. Optional namespace filter."),
		mcpsdk.WithString("namespace"),
	), s.handleListActive)

	s.mcpServer.AddTool(mcpsdk.NewTool("list_fault_catalog",
		mcpsdk.WithDescription("List all fault types installed in the cluster and permitted by current policy."),
	), s.handleListCatalog)

	s.mcpServer.AddTool(mcpsdk.NewTool("get_baseline",
		mcpsdk.WithDescription("Return the cached SUT baseline for a namespace. The baseline is captured by 'simian sut deploy' once all expected workloads are Ready and have held steady for the configured stability window. Returns {exists: false} if no SUT has been deployed there yet."),
		mcpsdk.WithString("namespace", mcpsdk.Required()),
	), s.handleGetBaseline)

	s.mcpServer.AddTool(mcpsdk.NewTool("get_topology",
		mcpsdk.WithDescription("Return a read-only snapshot of an arena namespace: workloads, services, dependency graph (inferred from NetworkPolicy ingress rules and env-var service references), pod status, and recent events. Cached via informers; cheap to call. Returns {error: ...} if topology discovery is not enabled."),
		mcpsdk.WithString("namespace", mcpsdk.Required()),
	), s.handleGetTopology)

	s.mcpServer.AddTool(mcpsdk.NewTool("get_metrics",
		mcpsdk.WithDescription("Range-query a metrics backend (Prometheus / Cloud Monitoring). M3 ships a stub; configure a metrics provider in a later milestone for real telemetry."),
		mcpsdk.WithString("query", mcpsdk.Required()),
		mcpsdk.WithString("namespace"),
	), s.handleGetMetrics)

	s.mcpServer.AddTool(mcpsdk.NewTool("get_recent_faults",
		mcpsdk.WithDescription("List faults the executor recently handled. Used by the autonomous-mode planner so it doesn't repeat the same attack with no observation gap. Each entry includes the original FaultManifest plus applied/cleared timestamps and clear reason."),
		mcpsdk.WithString("namespace"),
		mcpsdk.WithNumber("limit"),
	), s.handleGetRecentFaults)

	s.mcpServer.AddTool(mcpsdk.NewTool("establish_baseline",
		mcpsdk.WithDescription("Deploy a SUT into the given arena namespace and capture its baseline in the controller's in-memory cache. Once captured, get_baseline returns the snapshot. The arena (eligibility annotation + chaos-SA RoleBinding) must already exist."),
		mcpsdk.WithString("namespace", mcpsdk.Required()),
		mcpsdk.WithString("sut", mcpsdk.Required()),
	), s.handleEstablishBaseline)
}

func (s *Server) handleSubmitFault(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	args := req.GetArguments()
	intent, _ := args["intent"].(string)
	if intent == "" {
		return mcpsdk.NewToolResultError("intent is required"), nil
	}
	defaultNS, _ := args["default_namespace"].(string)
	durStr, _ := args["default_duration"].(string)
	defaultDur := 2 * time.Minute
	if durStr != "" {
		if d, err := time.ParseDuration(durStr); err == nil {
			defaultDur = d
		}
	}

	cat, err := s.gatherCatalog(ctx)
	if err != nil {
		return mcpsdk.NewToolResultError(fmt.Sprintf("catalog: %v", err)), nil
	}

	manifest, err := s.Translator.Translate(ctx, planner.IntentInput{
		Intent:          intent,
		Catalog:         cat,
		DefaultDuration: defaultDur,
	})
	if err != nil {
		return mcpsdk.NewToolResultError(fmt.Sprintf("translate: %v", err)), nil
	}
	if defaultNS != "" {
		for i := range manifest.Targets {
			if manifest.Targets[i].Namespace == "" {
				manifest.Targets[i].Namespace = defaultNS
			}
		}
	}
	manifest.Source = simian.SourceDirected

	uid, err := s.Executor.Apply(ctx, manifest)
	if err != nil {
		return mcpsdk.NewToolResultError(fmt.Sprintf("apply: %v", err)), nil
	}
	return mcpsdk.NewToolResultText(fmt.Sprintf(`{"fault_uid":%q,"engine":%q,"resource_kind":%q}`,
		uid, manifest.Engine, manifest.ResourceKind)), nil
}

func (s *Server) handleSubmitManifest(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	args := req.GetArguments()
	raw, ok := args["manifest"]
	if !ok {
		return mcpsdk.NewToolResultError("manifest is required"), nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return mcpsdk.NewToolResultError(fmt.Sprintf("manifest re-marshal: %v", err)), nil
	}
	var m simian.FaultManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return mcpsdk.NewToolResultError(fmt.Sprintf("manifest decode: %v", err)), nil
	}
	if m.Source == "" {
		m.Source = simian.SourceDirected
	}
	uid, err := s.Executor.Apply(ctx, m)
	if err != nil {
		return mcpsdk.NewToolResultError(fmt.Sprintf("apply: %v", err)), nil
	}
	return mcpsdk.NewToolResultText(fmt.Sprintf(`{"fault_uid":%q}`, uid)), nil
}

func (s *Server) handleClearFault(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	args := req.GetArguments()
	uid, _ := args["fault_uid"].(string)
	if uid == "" {
		return mcpsdk.NewToolResultError("fault_uid is required"), nil
	}
	if err := s.Executor.Clear(ctx, uid); err != nil {
		return mcpsdk.NewToolResultError(err.Error()), nil
	}
	return mcpsdk.NewToolResultText(`{"cleared":true}`), nil
}

func (s *Server) handleListActive(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	args := req.GetArguments()
	ns, _ := args["namespace"].(string)
	active, err := s.Executor.ListActive(ctx, ns)
	if err != nil {
		return mcpsdk.NewToolResultError(err.Error()), nil
	}
	b, _ := json.Marshal(active)
	return mcpsdk.NewToolResultText(string(b)), nil
}

func (s *Server) handleListCatalog(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	cat, err := s.gatherCatalog(ctx)
	if err != nil {
		return mcpsdk.NewToolResultError(err.Error()), nil
	}
	b, _ := json.Marshal(cat)
	return mcpsdk.NewToolResultText(string(b)), nil
}

func (s *Server) handleGetBaseline(_ context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	args := req.GetArguments()
	ns, _ := args["namespace"].(string)
	if ns == "" {
		return mcpsdk.NewToolResultError("namespace is required"), nil
	}
	if s.Baselines == nil {
		return mcpsdk.NewToolResultText(`{"exists":false,"reason":"baseline subsystem not enabled in this controller"}`), nil
	}
	bl, ok := s.Baselines.Baseline(ns)
	if !ok {
		return mcpsdk.NewToolResultText(fmt.Sprintf(`{"exists":false,"namespace":%q}`, ns)), nil
	}
	out := struct {
		Exists   bool         `json:"exists"`
		Baseline sut.Baseline `json:"baseline"`
	}{Exists: true, Baseline: bl}
	b, _ := json.Marshal(out)
	return mcpsdk.NewToolResultText(string(b)), nil
}

func (s *Server) gatherCatalog(ctx context.Context) ([]simian.CatalogEntry, error) {
	var out []simian.CatalogEntry
	for _, d := range s.Drivers {
		entries, err := d.Catalog(ctx)
		if err != nil {
			return nil, err
		}
		out = append(out, entries...)
	}
	return out, nil
}

func (s *Server) handleGetTopology(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	args := req.GetArguments()
	ns, _ := args["namespace"].(string)
	if ns == "" {
		return mcpsdk.NewToolResultError("namespace is required"), nil
	}
	if s.Topology == nil {
		return mcpsdk.NewToolResultText(`{"enabled":false,"reason":"topology discovery not enabled in this controller"}`), nil
	}
	snap, err := s.Topology.Snapshot(ctx, ns)
	if err != nil {
		return mcpsdk.NewToolResultError(fmt.Sprintf("topology: %v", err)), nil
	}
	b, _ := json.Marshal(snap)
	return mcpsdk.NewToolResultText(string(b)), nil
}

func (s *Server) handleGetMetrics(_ context.Context, _ mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	// M3 ships a deliberate stub. The autonomous-mode planner is told the
	// metrics surface exists so it can emit hypotheses about it; landing a
	// real provider (Prometheus / Cloud Monitoring) is scoped to a later
	// milestone with its own deployment + auth story.
	return mcpsdk.NewToolResultText(
		`{"configured":false,"reason":"metrics provider not configured (deferred); see roadmap.md M3 risks."}`,
	), nil
}

func (s *Server) handleGetRecentFaults(_ context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	args := req.GetArguments()
	ns, _ := args["namespace"].(string)
	limit := 0
	if v, ok := args["limit"]; ok {
		switch n := v.(type) {
		case float64:
			limit = int(n)
		case int:
			limit = n
		}
	}
	if s.Recents == nil {
		return mcpsdk.NewToolResultText(`{"recent":[],"enabled":false}`), nil
	}
	out := s.Recents.Recent(ns, limit)
	if out == nil {
		out = []executor.RecentFault{}
	}
	wrapper := struct {
		Enabled bool                    `json:"enabled"`
		Recent  []executor.RecentFault `json:"recent"`
	}{Enabled: true, Recent: out}
	b, _ := json.Marshal(wrapper)
	return mcpsdk.NewToolResultText(string(b)), nil
}

func (s *Server) handleEstablishBaseline(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	args := req.GetArguments()
	ns, _ := args["namespace"].(string)
	sutName, _ := args["sut"].(string)
	if ns == "" || sutName == "" {
		return mcpsdk.NewToolResultError("namespace and sut are required"), nil
	}
	if s.Establisher == nil {
		return mcpsdk.NewToolResultError("establish_baseline: SUT manager not enabled in this controller"), nil
	}
	bl, err := s.Establisher.Deploy(ctx, sut.DeployOptions{Namespace: ns, SUTName: sutName})
	if err != nil {
		return mcpsdk.NewToolResultError(fmt.Sprintf("establish_baseline: %v", err)), nil
	}
	out := struct {
		Established bool         `json:"established"`
		Baseline    sut.Baseline `json:"baseline"`
	}{Established: true, Baseline: *bl}
	b, _ := json.Marshal(out)
	return mcpsdk.NewToolResultText(string(b)), nil
}
