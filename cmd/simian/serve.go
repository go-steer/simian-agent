package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/driver/chaosmesh"
	"github.com/go-steer/simian-agent/pkg/executor"
	"github.com/go-steer/simian-agent/pkg/lease"
	"github.com/go-steer/simian-agent/pkg/llm/gemini"
	"github.com/go-steer/simian-agent/pkg/llm/stub"
	"github.com/go-steer/simian-agent/pkg/mcp"
	"github.com/go-steer/simian-agent/pkg/planner"
	"github.com/go-steer/simian-agent/pkg/simian"
)

func newServeCmd() *cobra.Command {
	var (
		kubeconfig        string
		mcpAddr           string
		mcpStdio          bool
		llmProviderID     string
		llmModel          string
		eligibleNS        []string
		durationCap       time.Duration
		reapInterval      time.Duration
		holderID          string
		debugLLMPayloads  bool
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the Simian controller (Fault Executor + MCP server)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
			slog.SetDefault(logger)
			auditor := audit.New(logger)

			cfg, err := buildKubeConfig(kubeconfig)
			if err != nil {
				return fmt.Errorf("kubeconfig: %w", err)
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
			if _, err := kubernetes.NewForConfig(cfg); err != nil {
				return fmt.Errorf("kubernetes clientset: %w", err)
			}

			cmDriver := chaosmesh.New(dyn, cached, "simian-")
			drivers := map[simian.Engine]simian.ChaosDriver{
				simian.EngineChaosMesh: cmDriver,
			}

			elig := buildEligibility(eligibleNS)
			execCfg := executor.DefaultConfig()
			if durationCap > 0 {
				execCfg.DurationCeiling = durationCap
			}
			registry := lease.NewRegistry(holderID)
			exec := executor.New(execCfg, drivers, registry, auditor, elig)

			reaper := &lease.Reaper{
				Registry: registry,
				Driver:   cmDriver,
				Interval: reapInterval,
				Auditor:  auditor,
			}
			go reaper.Run(ctx)

			llm, err := buildLLM(ctx, llmProviderID, llmModel)
			if err != nil {
				return fmt.Errorf("llm provider: %w", err)
			}
			translator := planner.New(llm)
			if debugLLMPayloads {
				logger.Warn("debug-llm-payloads is ON — raw LLM responses will be logged. Disable in production.")
				translator.LogResponses = func(attempt int, raw []byte) {
					logger.Info("planner: LLM raw structured response",
						slog.Int("attempt", attempt),
						slog.String("raw_json", string(raw)))
				}
			}

			srv := mcp.New(exec, drivers, translator, version)

			if mcpStdio {
				logger.Info("simian serve: MCP stdio mode")
				return srv.ServeStdio(ctx)
			}

			sse := srv.ServeSSE(mcpAddr)
			httpSrv := &http.Server{Addr: mcpAddr, Handler: sse, ReadHeaderTimeout: 5 * time.Second}
			errCh := make(chan error, 1)
			go func() {
				logger.Info("simian serve: MCP/SSE listening", "addr", mcpAddr)
				errCh <- httpSrv.ListenAndServe()
			}()
			select {
			case <-ctx.Done():
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = httpSrv.Shutdown(shutdownCtx)
				return nil
			case err := <-errCh:
				return err
			}
		},
	}
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig (default: in-cluster, then $KUBECONFIG, then ~/.kube/config)")
	cmd.Flags().StringVar(&mcpAddr, "mcp-addr", ":8081", "MCP/SSE listen address")
	cmd.Flags().BoolVar(&mcpStdio, "mcp-stdio", false, "Serve MCP over stdio instead of SSE")
	cmd.Flags().StringVar(&llmProviderID, "llm-provider", "gemini", "LLM provider id (gemini|stub)")
	cmd.Flags().StringVar(&llmModel, "llm-model", "", "Model override (provider default if empty)")
	cmd.Flags().StringSliceVar(&eligibleNS, "eligible-namespace", nil, "Namespaces to treat as eligible (overrides annotation lookup; can be repeated)")
	cmd.Flags().DurationVar(&durationCap, "duration-ceiling", 0, "Override executor duration ceiling (default 15m)")
	cmd.Flags().DurationVar(&reapInterval, "reap-interval", 30*time.Second, "Lease reaper sweep interval")
	cmd.Flags().StringVar(&holderID, "holder-id", os.Getenv("HOSTNAME"), "Holder ID recorded on leases (defaults to HOSTNAME)")
	cmd.Flags().BoolVar(&debugLLMPayloads, "debug-llm-payloads", false, "Log raw LLM responses (debug only; do not enable in production — see design.md §12.2)")
	return cmd
}

func buildKubeConfig(path string) (*rest.Config, error) {
	if path == "" {
		if cfg, err := rest.InClusterConfig(); err == nil {
			return cfg, nil
		}
	}
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if path != "" {
		loadingRules.ExplicitPath = path
	}
	clientCfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{})
	return clientCfg.ClientConfig()
}

func buildEligibility(eligible []string) executor.EligibilityChecker {
	m := map[string]bool{}
	for _, ns := range eligible {
		m[ns] = true
	}
	return &executor.StaticEligibility{Eligible: m}
}

func buildLLM(ctx context.Context, id, model string) (simian.LLMProvider, error) {
	switch id {
	case "stub":
		p := stub.New("stub")
		p.AlwaysReturnText("stub provider; configure --llm-provider gemini for real translation")
		return p, nil
	case "gemini", "":
		return gemini.New(ctx, gemini.Config{DefaultModel: model})
	default:
		return nil, fmt.Errorf("unknown llm provider %q", id)
	}
}
