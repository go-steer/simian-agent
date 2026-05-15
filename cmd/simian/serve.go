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

	"github.com/go-steer/simian-agent/pkg/arena"
	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/driver/chaosmesh"
	"github.com/go-steer/simian-agent/pkg/executor"
	"github.com/go-steer/simian-agent/pkg/lease"
	"github.com/go-steer/simian-agent/pkg/llm/gemini"
	"github.com/go-steer/simian-agent/pkg/llm/stub"
	"github.com/go-steer/simian-agent/pkg/loop"
	"github.com/go-steer/simian-agent/pkg/mcp"
	"github.com/go-steer/simian-agent/pkg/planner"
	"github.com/go-steer/simian-agent/pkg/simian"
	"github.com/go-steer/simian-agent/pkg/sut"
	"github.com/go-steer/simian-agent/pkg/topology"
)

func newServeCmd() *cobra.Command {
	var (
		kubeconfig           string
		mcpAddr              string
		mcpStdio             bool
		llmProviderID        string
		llmModel             string
		eligibleNS           []string
		durationCap          time.Duration
		reapInterval         time.Duration
		holderID             string
		debugLLMPayloads     bool
		recentFaultsCapacity int
		topologyResync       time.Duration
		autonomous           bool
		cycleInterval        time.Duration
		autonomousNS         []string
		maxFaultsPerCycle    int
		maxSeverityPerCycle  string
		hypothesisHint       string
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

			cmDriver := chaosmesh.New(dyn, cached, "simian-")
			drivers := map[simian.Engine]simian.ChaosDriver{
				simian.EngineChaosMesh: cmDriver,
			}

			clientset, err := kubernetes.NewForConfig(cfg)
			if err != nil {
				return fmt.Errorf("kubernetes clientset: %w", err)
			}
			elig := buildEligibility(clientset, eligibleNS, logger)
			execCfg := executor.DefaultConfig()
			if durationCap > 0 {
				execCfg.DurationCeiling = durationCap
			}
			registry := lease.NewRegistry(holderID)
			history := executor.NewHistory(recentFaultsCapacity)
			exec := executor.New(execCfg, drivers, registry, auditor, elig, executor.WithHistory(history))

			reaper := &lease.Reaper{
				Registry: registry,
				Driver:   cmDriver,
				Interval: reapInterval,
				Auditor:  auditor,
				OnExpire: func(af simian.ActiveFault, reason string) {
					history.UpdateCleared(af.FaultUID, time.Now().UTC(), reason)
				},
			}
			go reaper.Run(ctx)

			disco2 := topology.New(clientset, topologyResync)
			go func() {
				if err := disco2.Run(ctx); err != nil {
					logger.Warn("topology: discoverer exited", slog.String("err", err.Error()))
				}
			}()

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

			// SUT manager owns the baseline cache and is the BaselineEstablisher
			// behind establish_baseline (M3). Out-of-process 'simian sut deploy'
			// callers wanting the controller to know about a baseline pass
			// --use-controller, which proxies through the new MCP tool.
			sutMgr := sut.NewManager(clientset, dyn, cached, sut.Default)

			srv := mcp.New(exec, drivers, translator, sutMgr, version,
				mcp.WithTopology(disco2),
				mcp.WithRecents(exec),
				mcp.WithBaselineEstablisher(sutMgr),
			)

			if autonomous {
				if len(autonomousNS) == 0 {
					return fmt.Errorf("--autonomous requires at least one --autonomous-namespace")
				}
				generator := planner.NewGenerator(llm)
				if debugLLMPayloads {
					generator.LogResponses = func(attempt int, raw []byte) {
						logger.Info("planner: LLM raw plan response",
							slog.Int("attempt", attempt),
							slog.String("raw_json", string(raw)))
					}
				}
				gate := &loop.BaselineHealthGate{
					Baselines:    sutMgr,
					Topology:     disco2,
					ActiveFaults: exec,
				}
				lp := &loop.Loop{
					Namespaces: autonomousNS,
					Interval:   cycleInterval,
					Generator:  generator,
					Executor:   exec,
					Topology:   disco2,
					Baselines:  sutMgr,
					Recents:    exec,
					Catalog: func(c context.Context) ([]simian.CatalogEntry, error) {
						return srv.GatherCatalog(c)
					},
					Health: gate,
					Budget: planner.Budget{
						MaxFaultsPerCycle:   maxFaultsPerCycle,
						MaxConcurrentFaults: execCfg.MaxConcurrentFaults,
						MinCooldown:         execCfg.MinCooldown,
						MaxSeverityPerCycle: simian.BlastRadiusTier(maxSeverityPerCycle),
					},
					Auditor:    auditor,
					Logger:     logger,
					Hypothesis: hypothesisHint,
				}
				go func() {
					logger.Info("simian serve: autonomous loop starting",
						slog.Any("namespaces", autonomousNS),
						slog.Duration("interval", cycleInterval))
					if err := lp.Run(ctx); err != nil && err != context.Canceled {
						logger.Warn("autonomous loop exited", slog.String("err", err.Error()))
					}
				}()
			}

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
	cmd.Flags().IntVar(&recentFaultsCapacity, "recent-faults-capacity", executor.DefaultHistoryCapacity, "Bounded ring size backing the get_recent_faults MCP tool")
	cmd.Flags().DurationVar(&topologyResync, "topology-resync", 30*time.Second, "Topology informer resync interval")
	cmd.Flags().BoolVar(&autonomous, "autonomous", false, "Enable autonomous-mode planning loop (M3)")
	cmd.Flags().DurationVar(&cycleInterval, "cycle-interval", 5*time.Minute, "Time between autonomous cycles")
	cmd.Flags().StringSliceVar(&autonomousNS, "autonomous-namespace", nil, "Arena namespace(s) the autonomous loop targets (required with --autonomous; can be repeated)")
	cmd.Flags().IntVar(&maxFaultsPerCycle, "max-faults-per-cycle", 3, "Per-cycle cap on faults applied")
	cmd.Flags().StringVar(&maxSeverityPerCycle, "max-severity-per-cycle", "namespace", "Highest blast-radius tier the autonomous loop will apply (namespace|node|external)")
	cmd.Flags().StringVar(&hypothesisHint, "hypothesis-hint", "", "Optional hypothesis text passed to the planner as a soft preference")
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

func buildEligibility(k8s kubernetes.Interface, eligible []string, logger *slog.Logger) executor.EligibilityChecker {
	if len(eligible) > 0 {
		m := map[string]bool{}
		for _, ns := range eligible {
			m[ns] = true
		}
		logger.Info("eligibility: using static --eligible-namespace allowlist",
			slog.Any("namespaces", eligible))
		return &executor.StaticEligibility{Eligible: m}
	}
	logger.Info("eligibility: using annotation-based lookup (simian.chaos/eligible=\"true\")")
	return arena.NewAnnotationEligibility(k8s)
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
