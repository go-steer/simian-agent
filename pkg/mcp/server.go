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

	"github.com/go-steer/simian-agent/pkg/planner"
	"github.com/go-steer/simian-agent/pkg/simian"
)

// Server bundles the Simian dependencies the tools need.
type Server struct {
	Executor   simian.FaultExecutor
	Drivers    map[simian.Engine]simian.ChaosDriver
	Translator *planner.Translator
	Version    string

	mcpServer *server.MCPServer
}

// New returns a Server with all M1 tools registered.
func New(exec simian.FaultExecutor, drivers map[simian.Engine]simian.ChaosDriver, translator *planner.Translator, version string) *Server {
	if version == "" {
		version = "0.1.0"
	}
	s := &Server{Executor: exec, Drivers: drivers, Translator: translator, Version: version}
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
