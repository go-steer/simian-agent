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
		kubeconfig   string
		namespace    string
		sutName      string
		createArena  bool
		chaosSAName  string
		chaosSANS    string
		annotations  []string
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

			cached := memory.NewMemCacheClient(disco)
			mgr := sut.NewManager(clientset, dyn, cached, sut.Default)
			fmt.Printf("deploying SUT %q into %q...\n", sutName, namespace)
			bl, err := mgr.Deploy(ctx, sut.DeployOptions{Namespace: namespace, SUTName: sutName})
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

