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
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/go-steer/simian-agent/pkg/arena"
	"github.com/go-steer/simian-agent/pkg/sut"
	"github.com/go-steer/simian-agent/pkg/sut/onlineboutique"
)

func init() {
	// Register built-in SUTs at process start so 'simian sut list' and the
	// MCP get_baseline tool see them regardless of subcommand.
	onlineboutique.Register()
}

func newSutCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sut",
		Short: "Deploy / verify / tear down a SUT inside an arena",
		Long: `A SUT (System Under Test) is a workload Simian deploys into an arena and
verifies before chaos can begin. Built-in SUTs are listed by 'simian sut list'.

The arena (namespace + chaos-SA RoleBinding) must already exist. Use
'simian arena create' first, or pass --create-arena to compose both steps.`,
	}
	cmd.AddCommand(newSutListCmd())
	cmd.AddCommand(newSutDeployCmd())
	cmd.AddCommand(newSutDestroyCmd())
	return cmd
}

func newSutListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the built-in SUTs available for deployment",
		RunE: func(cmd *cobra.Command, _ []string) error {
			for _, s := range sut.Default.List() {
				fmt.Printf("%-20s %s\n", s.Name(), s.Description())
			}
			return nil
		},
	}
}

func newSutDeployCmd() *cobra.Command {
	var (
		kubeconfig    string
		namespace     string
		sutName       string
		createArena   bool
		chaosSAName   string
		chaosSANS     string
		annotations   []string
		useController bool
		mcpURL        string
		noEnvoyFaults bool
	)
	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Deploy a SUT into an arena and wait for the steady-state baseline",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if namespace == "" {
				return fmt.Errorf("--namespace is required")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Minute)
			defer cancel()

			cfg, err := buildKubeConfig(kubeconfig)
			if err != nil {
				return fmt.Errorf("kubeconfig: %w", err)
			}
			clientset, err := kubernetes.NewForConfig(cfg)
			if err != nil {
				return fmt.Errorf("kubernetes client: %w", err)
			}
			dyn, err := dynamic.NewForConfig(cfg)
			if err != nil {
				return fmt.Errorf("dynamic client: %w", err)
			}
			disco, err := discovery.NewDiscoveryClientForConfig(cfg)
			if err != nil {
				return fmt.Errorf("discovery client: %w", err)
			}

			if createArena {
				extraAnn, err := parseKVList(annotations)
				if err != nil {
					return fmt.Errorf("--annotation: %w", err)
				}
				am := arena.New(clientset, chaosSAName, chaosSANS)
				am.Dyn = dyn
				if err := am.Create(ctx, arena.Spec{Namespace: namespace, ExtraAnnotations: extraAnn}); err != nil {
					return fmt.Errorf("arena create: %w", err)
				}
				fmt.Printf("arena %q ready\n", namespace)
			} else {
				// Verify arena exists before deploying — fail loudly rather
				// than land workloads in a non-eligible namespace.
				am := arena.New(clientset, chaosSAName, chaosSANS)
				st, err := am.Describe(ctx, namespace)
				if err != nil {
					return fmt.Errorf("arena describe: %w", err)
				}
				if !st.Exists {
					return fmt.Errorf("arena %q does not exist; pass --create-arena or run 'simian arena create %s' first",
						namespace, namespace)
				}
				if !st.Eligible {
					return fmt.Errorf("namespace %q exists but is not annotated %s=true; refusing to deploy SUT",
						namespace, arena.EligibilityAnnotation)
				}
			}

			if useController {
				fmt.Printf("deploying SUT %q into %q via controller (%s)...\n", sutName, namespace, mcpURL)
				// SUT cold-start can take minutes (Online Boutique runs ~3-5min on
				// a small NAP node pool). Bump the SSE response timeout so the
				// CLI doesn't return a misleading transport error before the
				// controller's establish_baseline call returns. Match the outer
				// context's 15-minute window above.
				cli, err := newMCPClient(ctx, mcpURL, withResponseTimeout(15*time.Minute))
				if err != nil {
					return fmt.Errorf("connect to controller: %w", err)
				}
				defer func() { _ = cli.Close() }()
				if err := callTool(ctx, cli, "establish_baseline", map[string]any{
					"namespace": namespace,
					"sut":       sutName,
				}); err != nil {
					return err
				}
				return nil
			}

			cached := memory.NewMemCacheClient(disco)
			mgr := sut.NewManager(clientset, dyn, cached, sut.Default)
			withEnvoy := !noEnvoyFaults
			if withEnvoy {
				fmt.Printf("deploying SUT %q into %q (with Envoy fault sidecars)...\n", sutName, namespace)
			} else {
				fmt.Printf("deploying SUT %q into %q (Envoy injection disabled)...\n", sutName, namespace)
			}
			bl, err := mgr.Deploy(ctx, sut.DeployOptions{
				Namespace:       namespace,
				SUTName:         sutName,
				WithEnvoyFaults: withEnvoy,
			})
			if err != nil {
				return err
			}
			fmt.Printf("baseline established at %s (stability window: %s)\n",
				bl.EstablishedAt.Format(time.RFC3339), bl.StabilityWindow)
			for _, w := range bl.Workloads {
				fmt.Printf("  %-15s %s/%s  ready=%d/%d\n", w.Kind, namespace, w.Name, w.ReadyReplicas, w.DesiredReplicas)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig")
	cmd.Flags().StringVar(&namespace, "namespace", "", "Target arena namespace (required)")
	cmd.Flags().StringVar(&sutName, "sut", "online-boutique", "SUT name from the registry (see 'simian sut list')")
	cmd.Flags().BoolVar(&createArena, "create-arena", false, "Compose 'simian arena create' before deploying the SUT")
	cmd.Flags().StringVar(&chaosSAName, "chaos-sa", "simian-controller", "Chaos controller ServiceAccount (used only with --create-arena)")
	cmd.Flags().StringVar(&chaosSANS, "chaos-sa-namespace", "simian-system", "Chaos controller SA namespace (used only with --create-arena)")
	cmd.Flags().StringArrayVar(&annotations, "annotation", nil, "Extra namespace annotation key=value (used only with --create-arena)")
	cmd.Flags().BoolVar(&useController, "use-controller", false, "Trigger the deploy + baseline capture inside a running 'simian serve' via the establish_baseline MCP tool, so the controller's get_baseline cache is populated. Requires --mcp-url to point at the running controller.")
	cmd.Flags().StringVar(&mcpURL, "mcp-url", "http://localhost:8081/sse", "Simian MCP/SSE endpoint URL (only used with --use-controller)")
	cmd.Flags().BoolVar(&noEnvoyFaults, "no-envoy-faults", true, "Skip injecting the Envoy fault-injection sidecar into SUT Deployments. DEFAULT is true (skip) because the current iptables-based interception breaks gRPC liveness/readiness probes — see README.md \"Known limitation: Envoy injection breaks gRPC kubelet probes\". Set --no-envoy-faults=false to enable injection for SUTs whose probes are HTTP-only or TCP-only. Per-workload opt-out via the simian.chaos/no-envoy-injection=true pod-template annotation.")
	return cmd
}

func newSutDestroyCmd() *cobra.Command {
	var (
		kubeconfig string
		namespace  string
		sutName    string
		withArena  bool
		force      bool
	)
	cmd := &cobra.Command{
		Use:   "destroy",
		Short: "Tear down a SUT, optionally also tearing down its arena",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if namespace == "" {
				return fmt.Errorf("--namespace is required")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
			defer cancel()

			cfg, err := buildKubeConfig(kubeconfig)
			if err != nil {
				return fmt.Errorf("kubeconfig: %w", err)
			}
			clientset, err := kubernetes.NewForConfig(cfg)
			if err != nil {
				return fmt.Errorf("kubernetes client: %w", err)
			}
			dyn, err := dynamic.NewForConfig(cfg)
			if err != nil {
				return fmt.Errorf("dynamic client: %w", err)
			}
			disco, err := discovery.NewDiscoveryClientForConfig(cfg)
			if err != nil {
				return fmt.Errorf("discovery client: %w", err)
			}
			cached := memory.NewMemCacheClient(disco)

			mgr := sut.NewManager(clientset, dyn, cached, sut.Default)
			fmt.Printf("destroying SUT %q from %q...\n", sutName, namespace)
			if err := mgr.Destroy(ctx, sut.DestroyOptions{Namespace: namespace, SUTName: sutName}); err != nil {
				return err
			}
			fmt.Printf("SUT removed\n")

			if withArena {
				am := arena.New(clientset, "simian-controller", "simian-system")
				am.Dyn = dyn
				if err := am.Destroy(ctx, namespace, force); err != nil {
					return fmt.Errorf("arena destroy: %w", err)
				}
				fmt.Printf("arena %q destroyed\n", namespace)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig")
	cmd.Flags().StringVar(&namespace, "namespace", "", "Arena namespace (required)")
	cmd.Flags().StringVar(&sutName, "sut", "online-boutique", "SUT name from the registry")
	cmd.Flags().BoolVar(&withArena, "with-arena", false, "Also destroy the arena (namespace + RoleBinding) after removing the SUT")
	cmd.Flags().BoolVar(&force, "force", false, "Force arena destroy even with active faults (only with --with-arena)")
	return cmd
}
