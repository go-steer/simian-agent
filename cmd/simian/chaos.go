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

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
	"github.com/spf13/cobra"
)

func newChaosCmd() *cobra.Command {
	var (
		mcpURL       string
		intent       string
		manifestPath string
		kind         string
		apiVersion   string
		engine       string
		ns           string
		workload     string
		duration     string
		specJSON     string
		stdinSpec    bool
		clear        string
		listActive   bool
		listCatalog  bool
	)
	cmd := &cobra.Command{
		Use:   "chaos",
		Short: "Submit a fault to the Simian controller (directed mode)",
		Long: `Submit a fault either as plain-text intent (LLM-translated) or as a hand-built FaultManifest (deterministic-control path).

Examples:
  # LLM-translated path
  simian chaos --intent "add 250ms latency to paymentservice for 2 minutes" --namespace online-boutique

  # Catalog-picked / deterministic-control path
  simian chaos --kind NetworkChaos --api-version chaos-mesh.org/v1alpha1 --namespace online-boutique --workload paymentservice --duration 2m --spec-file ./latency.json

  # Submit a fully-formed manifest
  simian chaos --manifest ./manifest.json

  # Inspect / clear
  simian chaos --list-active
  simian chaos --list-catalog
  simian chaos --clear f-<uid>`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// 90s accommodates LLM-translated paths that may need a retry round
			// (each LLM call can take 5-15s on Gemini 2.5 Pro).
			ctx, cancel := context.WithTimeout(cmd.Context(), 90*time.Second)
			defer cancel()

			cli, err := newMCPClient(ctx, mcpURL)
			if err != nil {
				return err
			}
			defer func() { _ = cli.Close() }()

			switch {
			case clear != "":
				return callTool(ctx, cli, "clear_fault", map[string]any{"fault_uid": clear})
			case listActive:
				return callTool(ctx, cli, "list_active_faults", map[string]any{"namespace": ns})
			case listCatalog:
				return callTool(ctx, cli, "list_fault_catalog", nil)
			case manifestPath != "":
				m, err := loadManifest(manifestPath)
				if err != nil {
					return err
				}
				return callTool(ctx, cli, "submit_manifest", map[string]any{"manifest": m})
			case intent != "":
				args := map[string]any{"intent": intent}
				if ns != "" {
					args["default_namespace"] = ns
				}
				if duration != "" {
					args["default_duration"] = duration
				}
				return callTool(ctx, cli, "submit_fault", args)
			case kind != "" || apiVersion != "":
				m, err := buildManifestFromFlags(engine, apiVersion, kind, ns, workload, duration, specJSON, stdinSpec)
				if err != nil {
					return err
				}
				return callTool(ctx, cli, "submit_manifest", map[string]any{"manifest": m})
			default:
				return fmt.Errorf("nothing to do — pass --intent, --kind/--api-version, --manifest, --clear, --list-active, or --list-catalog")
			}
		},
	}
	cmd.Flags().StringVar(&mcpURL, "mcp-url", "http://localhost:8081/sse", "Simian MCP/SSE endpoint URL")
	cmd.Flags().StringVar(&intent, "intent", "", "Plain-text chaos intent (LLM-translated)")
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "Path to a JSON FaultManifest to submit verbatim")
	cmd.Flags().StringVar(&engine, "engine", "chaos-mesh", "Chaos engine (chaos-mesh|litmus)")
	cmd.Flags().StringVar(&kind, "kind", "", "ResourceKind (e.g. NetworkChaos, PodChaos, IOChaos)")
	cmd.Flags().StringVar(&apiVersion, "api-version", "chaos-mesh.org/v1alpha1", "CRD apiVersion")
	cmd.Flags().StringVar(&ns, "namespace", "", "Target namespace")
	cmd.Flags().StringVar(&workload, "workload", "", "Target workload name")
	cmd.Flags().StringVar(&duration, "duration", "2m", "Fault duration (Go duration string)")
	cmd.Flags().StringVar(&specJSON, "spec", "", "Inline JSON spec for the fault (use --spec-file or --stdin-spec for larger specs)")
	cmd.Flags().StringVar(&specJSON, "spec-file", "", "Path to a JSON file containing the fault spec")
	cmd.Flags().BoolVar(&stdinSpec, "stdin-spec", false, "Read fault spec JSON from stdin")
	cmd.Flags().StringVar(&clear, "clear", "", "Clear the fault with this UID")
	cmd.Flags().BoolVar(&listActive, "list-active", false, "List active faults")
	cmd.Flags().BoolVar(&listCatalog, "list-catalog", false, "List fault catalog")
	return cmd
}

func newMCPClient(ctx context.Context, baseURL string) (*mcpclient.Client, error) {
	tr, err := transport.NewSSE(baseURL)
	if err != nil {
		return nil, fmt.Errorf("mcp transport: %w", err)
	}
	cli := mcpclient.NewClient(tr)
	if err := cli.Start(ctx); err != nil {
		return nil, fmt.Errorf("mcp start: %w", err)
	}
	if _, err := cli.Initialize(ctx, mcpsdk.InitializeRequest{
		Params: mcpsdk.InitializeParams{
			ProtocolVersion: mcpsdk.LATEST_PROTOCOL_VERSION,
			ClientInfo:      mcpsdk.Implementation{Name: "simian-cli", Version: version},
		},
	}); err != nil {
		return nil, fmt.Errorf("mcp initialize: %w", err)
	}
	return cli, nil
}

func callTool(ctx context.Context, cli *mcpclient.Client, name string, args map[string]any) error {
	req := mcpsdk.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	res, err := cli.CallTool(ctx, req)
	if err != nil {
		return fmt.Errorf("call %s: %w", name, err)
	}
	for _, c := range res.Content {
		if tc, ok := mcpsdk.AsTextContent(c); ok {
			fmt.Println(tc.Text)
		}
	}
	if res.IsError {
		return fmt.Errorf("tool %s returned error", name)
	}
	return nil
}

func loadManifest(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	return m, nil
}

func buildManifestFromFlags(engine, apiVersion, kind, ns, workload, duration, specJSON string, stdinSpec bool) (map[string]any, error) {
	if kind == "" {
		return nil, fmt.Errorf("--kind is required")
	}
	if ns == "" {
		return nil, fmt.Errorf("--namespace is required")
	}
	spec, err := loadSpec(specJSON, stdinSpec)
	if err != nil {
		return nil, err
	}
	if duration == "" {
		duration = "2m"
	}
	target := map[string]any{"namespace": ns}
	if workload != "" {
		target["name"] = workload
	}
	return map[string]any{
		"engine":        engine,
		"api_version":   apiVersion,
		"resource_kind": kind,
		"spec":          spec,
		"targets":       []any{target},
		"duration":      duration,
		"source":        "directed",
	}, nil
}

func loadSpec(spec string, fromStdin bool) (map[string]any, error) {
	var raw []byte
	switch {
	case fromStdin:
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
		raw = b
	case strings.HasPrefix(spec, "@"):
		path := spec[1:]
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read spec file: %w", err)
		}
		raw = b
	case spec == "":
		return map[string]any{}, nil
	default:
		raw = []byte(spec)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode spec: %w", err)
	}
	return m, nil
}
