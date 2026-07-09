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
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

func newBaselineCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "baseline",
		Short: "Establish and inspect namespace baselines via the running controller",
		Long: `Baselines are the healthy-state snapshot the autonomous loop's
health gate checks against before applying chaos. Two paths:

  - SUT-driven: the controller applies a registered SUT into the arena,
    waits for its declared workloads to reach Ready, holds a stability
    window, and caches the resulting baseline.
  - Topology-driven: the controller enumerates whatever Deployments +
    StatefulSets currently exist in the namespace (no manifests applied)
    and baselines what it finds. Use this for arbitrary workloads that
    weren't shipped by Simian's SUT registry.

Both paths persist the baseline via the controller's ConfigMap store so it
survives serve restarts. Both are just thin MCP client wrappers over the
running 'simian serve' — no cluster access needed from the CLI, only the
MCP endpoint URL.`,
	}
	cmd.AddCommand(newBaselineEstablishCmd())
	cmd.AddCommand(newBaselineShowCmd())
	return cmd
}

func newBaselineEstablishCmd() *cobra.Command {
	var (
		mcpURL string
		ns     string
		sut    string
	)
	cmd := &cobra.Command{
		Use:   "establish",
		Short: "Ask the controller to establish a baseline for a namespace",
		Long: `Calls the establish_baseline MCP tool on 'simian serve'. Pass --sut
to have the controller deploy a registered SUT and baseline it; omit --sut
to baseline whatever Deployments + StatefulSets currently exist in the
namespace.

Examples:
  # Topology-driven — baselines whatever's already there
  simian baseline establish --namespace payments-staging

  # SUT-driven — deploy + baseline Online Boutique
  simian baseline establish --namespace boutique-m3 --sut online-boutique`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if ns == "" {
				return fmt.Errorf("--namespace is required")
			}
			// establish_baseline can wait through the SUT's stability
			// window (Online Boutique defaults to 8 min) — pick a
			// generous ceiling.
			ctx, cancel := context.WithTimeout(cmd.Context(), 12*time.Minute)
			defer cancel()

			cli, err := newMCPClient(ctx, mcpURL, withResponseTimeout(11*time.Minute))
			if err != nil {
				return err
			}
			defer func() { _ = cli.Close() }()

			args := map[string]any{"namespace": ns}
			if sut != "" {
				args["sut"] = sut
			}
			return callTool(ctx, cli, "establish_baseline", args)
		},
	}
	cmd.Flags().StringVar(&mcpURL, "mcp-url", "http://localhost:8081/sse", "Simian MCP/SSE endpoint URL")
	cmd.Flags().StringVar(&ns, "namespace", "", "Target arena namespace (required)")
	cmd.Flags().StringVar(&sut, "sut", "", "Optional registered SUT name to deploy before baselining. Omit for topology-driven baseline of existing workloads.")
	return cmd
}

func newBaselineShowCmd() *cobra.Command {
	var (
		mcpURL string
		ns     string
	)
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print the cached baseline for a namespace via the controller",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if ns == "" {
				return fmt.Errorf("--namespace is required")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			cli, err := newMCPClient(ctx, mcpURL)
			if err != nil {
				return err
			}
			defer func() { _ = cli.Close() }()
			return callTool(ctx, cli, "get_baseline", map[string]any{"namespace": ns})
		},
	}
	cmd.Flags().StringVar(&mcpURL, "mcp-url", "http://localhost:8081/sse", "Simian MCP/SSE endpoint URL")
	cmd.Flags().StringVar(&ns, "namespace", "", "Namespace whose cached baseline to print (required)")
	return cmd
}
