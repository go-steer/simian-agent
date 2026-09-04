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
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/go-steer/simian-agent/pkg/arena"
	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/catalog"
	"github.com/go-steer/simian-agent/pkg/driver/chaosmesh"
	"github.com/go-steer/simian-agent/pkg/driver/envoyfault"
	"github.com/go-steer/simian-agent/pkg/driver/networkpolicy"
	"github.com/go-steer/simian-agent/pkg/executor"
	"github.com/go-steer/simian-agent/pkg/lease"
	"github.com/go-steer/simian-agent/pkg/llm/gemini"
	"github.com/go-steer/simian-agent/pkg/llm/stub"
	"github.com/go-steer/simian-agent/pkg/loop"
	"github.com/go-steer/simian-agent/pkg/mcp"
	"github.com/go-steer/simian-agent/pkg/planner"
	"github.com/go-steer/simian-agent/pkg/probe"
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
		permittedTiers       []string
		maxConcurrentFaults  int
		minCooldown          time.Duration
		defaultProbes        bool
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
		sutInjectEnvoyFault  bool
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

			clientset, err := kubernetes.NewForConfig(cfg)
			if err != nil {
				return fmt.Errorf("kubernetes clientset: %w", err)
			}
			// NetworkPolicy partition driver — works on GKE Dataplane V2,
			// where Chaos Mesh's NetworkChaos is silently bypassed
			// (see https://go-steer.github.io/simian-agent/docs/dpv2-chaos-engines/).
			npDriver := networkpolicy.New(clientset, "")
			// Envoy fault driver — pokes the per-pod Envoy admin API
			// installed by pkg/sut/envoy at SUT-deploy time. Works on
			// DPv2 because faults are applied above the dataplane.
			envoyDriver := envoyfault.New(clientset)

			drivers := map[simian.Engine]simian.ChaosDriver{
				simian.EngineChaosMesh:     cmDriver,
				simian.EngineNetworkPolicy: npDriver,
				simian.EngineEnvoyFault:    envoyDriver,
			}

			elig, arenaNamespaces := buildEligibility(clientset, eligibleNS, logger)
			execCfg := executor.DefaultConfig()
			if durationCap > 0 {
				execCfg.DurationCeiling = durationCap
			}
			if maxConcurrentFaults > 0 {
				execCfg.MaxConcurrentFaults = maxConcurrentFaults
			}
			if minCooldown > 0 {
				execCfg.MinCooldown = minCooldown
			}
			// Parsed before anything is built. A controller whose safety
			// policy does not parse must not come up holding the default
			// one — the operator asked for something narrower and would
			// have no way to tell they did not get it.
			if tiers, err := executor.ParsePermittedTiers(permittedTiers); err != nil {
				return fmt.Errorf("--permitted-tiers: %w", err)
			} else if tiers != nil {
				execCfg.PermittedTiers = tiers
			}
			// Validated here rather than where the loop reads it. An
			// unparseable cap makes the loop skip every step, which looks
			// exactly like a planner producing nothing.
			severityCap, err := simian.ParseBlastRadiusTier(maxSeverityPerCycle)
			if err != nil {
				return fmt.Errorf("--max-severity-per-cycle: %w", err)
			}
			registry := lease.NewRegistry(holderID)
			history := executor.NewHistory(recentFaultsCapacity)
			// Efficacy gate: a fault with Settle probes is not reported as
			// applied until they pass, and one with SOT probes is not applied
			// at all until they do. The k8s prober shares the chaos driver's
			// REST mapper so a probe can name any resource the cluster knows
			// about; the http prober dials pod IPs directly, the same way the
			// envoy-fault driver reaches each sidecar's admin API.
			prober := probe.NewMux(map[string]probe.Prober{
				simian.ProbeTypeK8s:  probe.NewK8sProber(dyn, restmapper.NewDeferredDiscoveryRESTMapper(cached)),
				simian.ProbeTypeHTTP: probe.NewKubernetesHTTPProber(clientset),
			})
			execOpts := []executor.Option{
				executor.WithHistory(history),
				executor.WithProber(prober),
			}
			if defaultProbes {
				execOpts = append(execOpts, executor.WithDefaultProbes(catalog.DefaultProbes))
			}
			exec := executor.New(execCfg, drivers, registry, auditor, elig, execOpts...)

			reaper := &lease.Reaper{
				Registry: registry,
				Drivers:  drivers,
				Interval: reapInterval,
				Auditor:  auditor,
				// Where the orphan scan looks for faults this process did
				// not apply. Bounded to the declared arenas — Simian must
				// never delete objects in a namespace nobody opted in. Under
				// the default (annotation) mode this list is empty at startup
				// and fills in as namespaces opt in, so it is resolved per
				// sweep rather than captured here.
				Namespaces: arenaNamespaces,
				OnExpire: func(af simian.ActiveFault, reason string) {
					history.UpdateCleared(af.FaultUID, time.Now().UTC(), reason)
				},
			}
			// Sweep before the first tick. If this process is the restart of
			// one that was killed holding a NetworkPolicy partition, that
			// partition is still in the cluster and no lease remembers it;
			// waiting a full reap interval to notice would extend an outage
			// that the crash already made unbounded.
			reaper.SweepOrphans(ctx)
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
			//
			// ConfigMap-backed persistence: baselines are mirrored to
			// <sut-namespace>/simian-baseline so they survive a serve restart.
			// Without this, autonomous mode dies on every restart with
			// cycle.health_gate_failed until establish_baseline is called by
			// hand.
			sutMgr := sut.NewManager(clientset, dyn, cached, sut.Default)
			// Same arena resolver the orphan reaper uses: baselines live in
			// arena namespaces, so that is exactly where List should look.
			sutMgr.Store = sut.NewConfigMapStore(clientset, arenaNamespaces)
			if n, err := sutMgr.LoadCachedBaselines(cmd.Context()); err != nil {
				logger.Warn("simian serve: baseline cache warm incomplete",
					slog.Int("loaded", n), slog.String("error", err.Error()))
			} else if n > 0 {
				logger.Info("simian serve: baseline cache warmed", slog.Int("namespaces", n))
			}

			// Wrap the manager so calls coming through MCP establish_baseline
			// honor the controller's WithEnvoyFaults policy. The flag default
			// is false (matches sutInjection.envoyFaults in the chart) because
			// the iptables interception breaks gRPC kubelet probes — see
			// README.md "Known limitation".
			establisher := &envoyOptingEstablisher{mgr: sutMgr, withEnvoyFaults: sutInjectEnvoyFault}

			srv := mcp.New(exec, drivers, translator, sutMgr, version,
				mcp.WithTopology(disco2),
				mcp.WithRecents(exec),
				mcp.WithBaselineEstablisher(establisher),
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
						MaxSeverityPerCycle: severityCap,
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
	cmd.Flags().StringSliceVar(&permittedTiers, "permitted-tiers", nil, "Blast-radius tiers this installation permits (namespace|node|external). Repeatable or comma-separated. Unset keeps the default policy of namespace,node; set it to just namespace to keep node-level chaos off the cluster entirely.")
	cmd.Flags().IntVar(&maxConcurrentFaults, "max-concurrent-faults", 0, "Cap on total leased faults across all namespaces (0 = no cap). Enforced by the safety stage; rejected applies surface as executor.rejected with reason safety:budget-exceeded.")
	cmd.Flags().DurationVar(&minCooldown, "min-cooldown", 0, "Minimum gap between consecutive faults applied to the same namespace (0 = disabled)")
	cmd.Flags().BoolVar(&defaultProbes, "default-efficacy-probes", true, "Attach Simian's built-in efficacy probes to fault kinds that have one (see the catalog's efficacy_gate field). Turning this off applies dataplane faults unverified: a fault the cluster accepts but silently drops is then indistinguishable from one that worked.")
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
	cmd.Flags().BoolVar(&sutInjectEnvoyFault, "sut-inject-envoy-faults", false, "When true, the controller injects an Envoy fault sidecar into each Deployment of any SUT applied via the establish_baseline MCP tool. Required by the envoy-fault chaos engine to deliver HTTP delay/abort on GKE Dataplane V2. DEFAULT is false because the current iptables interception breaks gRPC liveness/readiness probes — see README.md \"Known limitation: Envoy injection breaks gRPC kubelet probes\". Only enable for SUTs whose probes are HTTP-only or TCP-only.")
	return cmd
}

// envoyOptingEstablisher overlays a fixed WithEnvoyFaults preference on
// every Deploy() call. Used to wire the controller's --sut-inject-envoy-faults
// flag through the MCP establish_baseline path: the MCP tool itself is
// argument-poor (just namespace + sut name), so the policy is set once at
// controller boot.
type envoyOptingEstablisher struct {
	mgr             *sut.Manager
	withEnvoyFaults bool
}

func (e *envoyOptingEstablisher) Deploy(ctx context.Context, opts sut.DeployOptions) (*sut.Baseline, error) {
	opts.WithEnvoyFaults = e.withEnvoyFaults
	return e.mgr.Deploy(ctx, opts)
}

// EstablishBaselineFromTopology is a straight pass-through — the
// WithEnvoyFaults preference is only meaningful when Simian applies a
// SUT (Deploy path). Topology-derived baselines observe whatever
// workloads already exist; the operator picked the injection posture
// when they deployed those workloads.
func (e *envoyOptingEstablisher) EstablishBaselineFromTopology(ctx context.Context, namespace string, cfg sut.BaselineConfig) (*sut.Baseline, error) {
	return e.mgr.EstablishBaselineFromTopology(ctx, namespace, cfg)
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

// buildEligibility returns the executor's per-fault eligibility check and,
// alongside it, the resolver the orphan reaper uses to decide which namespaces
// it may sweep. The two must agree: anything the reaper is allowed to delete in
// has to be a namespace the executor would have allowed a fault in.
func buildEligibility(k8s kubernetes.Interface, eligible []string, logger *slog.Logger) (executor.EligibilityChecker, func(context.Context) ([]string, error)) {
	if len(eligible) > 0 {
		m := map[string]bool{}
		for _, ns := range eligible {
			m[ns] = true
		}
		logger.Info("eligibility: using static --eligible-namespace allowlist",
			slog.Any("namespaces", eligible))
		arenas := slices.Clone(eligible)
		return &executor.StaticEligibility{Eligible: m}, func(context.Context) ([]string, error) {
			return arenas, nil
		}
	}
	logger.Info("eligibility: using annotation-based lookup (simian.chaos/eligible=\"true\")")
	ae := arena.NewAnnotationEligibility(k8s)
	return ae, ae.ListEligible
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
