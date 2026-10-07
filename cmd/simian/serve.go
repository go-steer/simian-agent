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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"sync"
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
	"github.com/go-steer/simian-agent/pkg/driver/kubestate"
	"github.com/go-steer/simian-agent/pkg/driver/networkpolicy"
	"github.com/go-steer/simian-agent/pkg/executor"
	"github.com/go-steer/simian-agent/pkg/lease"
	"github.com/go-steer/simian-agent/pkg/llm/gemini"
	"github.com/go-steer/simian-agent/pkg/llm/stub"
	"github.com/go-steer/simian-agent/pkg/loop"
	"github.com/go-steer/simian-agent/pkg/mcp"
	"github.com/go-steer/simian-agent/pkg/metrics"
	"github.com/go-steer/simian-agent/pkg/planner"
	"github.com/go-steer/simian-agent/pkg/probe"
	"github.com/go-steer/simian-agent/pkg/simian"
	"github.com/go-steer/simian-agent/pkg/sut"
	"github.com/go-steer/simian-agent/pkg/topology"
	"github.com/go-steer/simian-agent/pkg/webui"
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
		auditFile            string
		auditFileMaxBytes    int64
		topologyResync       time.Duration
		autonomous           bool
		cycleInterval        time.Duration
		autonomousNS         []string
		maxFaultsPerCycle    int
		maxSeverityPerCycle  string
		hypothesisHint       string
		sutInjectEnvoyFault  bool
		metricsAddr          string
		ui                   bool
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the Simian controller (Fault Executor + MCP server)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
			slog.SetDefault(logger)
			var auditor simian.Auditor = audit.New(logger)
			// Run once the lease registry exists, so what the previous
			// process left running can be adopted into it (#172).
			takeOverFaults := func(context.Context, simian.Auditor, func(audit.FaultRow) bool) {}
			var pastFaults []audit.FaultRow
			// What the loop decided, cycle by cycle, for get_recent_cycles
			// and simian watch. Seeded from the audit file so a restart does
			// not wipe it.
			cycleLog := audit.NewCycleLog(0)
			// Each fault's record — how it ended, whether it took and the
			// workload recovered — for the web UI, likewise seeded.
			faultLog := audit.NewFaultLog(0)
			if auditFile != "" {
				fileAuditor, records, takeOver, err := openAuditFile(auditFile, auditFileMaxBytes, logger)
				if err != nil {
					return err
				}
				defer func() { _ = fileAuditor.Close() }()
				auditor = audit.Multi{auditor, fileAuditor}
				takeOverFaults = takeOver
				pastFaults = audit.Faults(records)
				for _, r := range records {
					cycleLog.Add(r)
					faultLog.Add(r)
				}
			}
			auditor = audit.Multi{auditor, cycleLog, faultLog}
			// The web UI's live stream of audit events.
			var broadcaster *webui.Broadcaster
			if ui {
				broadcaster = webui.NewBroadcaster(ctx)
				auditor = audit.Multi{auditor, broadcaster}
			}
			// Prometheus metrics, counted from the same audit events.
			var recorder *metrics.Recorder
			if metricsAddr != "" {
				recorder = metrics.New(version)
				auditor = audit.Multi{auditor, recorder}
				msrv := &http.Server{Addr: metricsAddr, Handler: recorder.Handler(), ReadHeaderTimeout: 5 * time.Second}
				go func() {
					if err := msrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
						logger.Error("simian serve: metrics listener", slog.String("error", err.Error()))
					}
				}()
				defer func() { _ = msrv.Close() }()
				logger.Info("simian serve: metrics listening", slog.String("addr", metricsAddr))
			}

			// Background loops that emit audit events. Deferred after the
			// audit file's Close, so it runs first: stop them and let them
			// write their last events — an interrupted fault being rolled
			// back — into the file rather than after it has closed (#158).
			var workers sync.WaitGroup
			defer func() {
				cancel()
				waitForWorkers(&workers, shutdownWait, logger)
			}()

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
			// kube-state driver — declarative-state faults. The other three
			// engines perturb a running dataplane; this one synthesizes an
			// object that is wrong in the API server, which is the half of the
			// failure space an SRE agent actually lives in.
			ksDriver := kubestate.New(clientset)

			drivers := map[simian.Engine]simian.ChaosDriver{
				simian.EngineChaosMesh:     cmDriver,
				simian.EngineNetworkPolicy: npDriver,
				simian.EngineEnvoyFault:    envoyDriver,
				simian.EngineKubeState:     ksDriver,
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
			// What the last process ran and was refused, so the planner is
			// told and the loop's repeated-refusal limit holds across a
			// restart (#173, #178).
			for _, rf := range executor.RecentFromAudit(pastFaults, time.Now().Add(-historyMemory), time.Now()) {
				history.Push(rf)
			}
			for _, rf := range executor.RefusalsFromAudit(pastFaults, time.Now().Add(-historyMemory)) {
				history.PushRefused(rf)
			}
			// Efficacy gate: a fault with Settle probes is not reported as
			// applied until they pass, and one with SOT probes is not applied
			// at all until they do. The k8s prober shares the chaos driver's
			// REST mapper so a probe can name any resource the cluster knows
			// about; the http prober dials pod IPs directly, the same way the
			// envoy-fault driver reaches each sidecar's admin API.
			httpProber := probe.NewKubernetesHTTPProber(clientset)
			prober := probe.NewMux(map[string]probe.Prober{
				simian.ProbeTypeK8s:  probe.NewK8sProber(dyn, restmapper.NewDeferredDiscoveryRESTMapper(cached)),
				simian.ProbeTypeHTTP: httpProber,
				simian.ProbeTypeTCP:  httpProber.TCP(),
				simian.ProbeTypeLogs: probe.NewKubernetesLogsProber(clientset),
			})
			execOpts := []executor.Option{
				executor.WithHistory(history),
				executor.WithProber(prober),
				executor.WithWorkloadSelectors(executor.KubernetesWorkloadSelectors{Client: clientset}),
				executor.WithTargetPods(executor.KubernetesTargetPods{Client: clientset}),
			}
			if defaultProbes {
				execOpts = append(execOpts, executor.WithDefaultProbes(catalog.DefaultProbes))
			}
			exec := executor.New(execCfg, drivers, registry, auditor, elig, execOpts...)
			if recorder != nil {
				recorder.WatchActive(exec)
			}

			// Before anything plans or applies: a fault still running from
			// the previous process has to be counted from the first cycle.
			takeOverFaults(ctx, auditor, func(r audit.FaultRow) bool {
				af, ok := r.ActiveFault(time.Now().UTC())
				if !ok || drivers[af.Manifest.Engine] == nil {
					return false
				}
				registry.Adopt(af)
				return true
			})
			// Then whatever is still running that the file did not cover: it
			// is lost with an emptyDir when the pod is rescheduled (#177).
			if arenas, err := arenaNamespaces(ctx); err != nil {
				logger.Warn("simian serve: cannot resolve arenas to adopt live faults from", slog.String("error", err.Error()))
			} else if n := lease.AdoptLive(ctx, registry, drivers, arenas, auditor, time.Now().UTC()); n > 0 {
				logger.Info("simian serve: adopted live faults found in the cluster", slog.Int("faults", n))
			}
			// A fault adopted from the cluster alone is in no history yet;
			// the planner should still see it running (#178). The metrics
			// need its kind and namespace too, which this process never saw
			// an event for.
			for _, af := range registry.List("") {
				if recorder != nil {
					recorder.Remember(af)
				}
				if !history.Has(af.FaultUID) {
					history.Push(executor.RecentFault{FaultUID: af.FaultUID, Manifest: af.Manifest, AppliedAt: af.AppliedAt})
				}
			}

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
					// Ending on time is not the workload recovering; check,
					// and say so in the audit trail if it did not.
					workers.Go(func() { exec.CheckRecovery(ctx, af) })
				},
			}
			// Sweep before the first tick. If this process is the restart of
			// one that was killed holding a NetworkPolicy partition, that
			// partition is still in the cluster and no lease remembers it;
			// waiting a full reap interval to notice would extend an outage
			// that the crash already made unbounded.
			reaper.SweepOrphans(ctx)
			workers.Go(func() { reaper.Run(ctx) })

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
			sutMgr.OnPersistFailure = func(ctx context.Context, ns string, err error) {
				auditor.Emit(ctx, simian.AuditEvent{
					Event:   audit.EventBaselinePersistFailed,
					Reason:  "store-save-failed",
					Payload: map[string]any{"namespace": ns, "error": err.Error()},
				})
			}
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
				mcp.WithCycles(cycleLog),
				mcp.WithBaselineEstablisher(establisher),
			)

			// The metrics' series, at 0, for every arena and catalog entry,
			// and again as arenas are opted in: a series that first appears
			// at 1 loses that fault in increase() and in Managed Prometheus.
			if recorder != nil {
				workers.Go(func() { primeMetrics(ctx, recorder, arenaNamespaces, drivers, reapInterval, logger) })
			}

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
						MaxFaultDuration:    execCfg.DurationCeiling,
					},
					Auditor:    auditor,
					Logger:     logger,
					Hypothesis: hypothesisHint,
				}
				workers.Go(func() {
					logger.Info("simian serve: autonomous loop starting",
						slog.Any("namespaces", autonomousNS),
						slog.Duration("interval", cycleInterval))
					if err := lp.Run(ctx); err != nil && err != context.Canceled {
						logger.Warn("autonomous loop exited", slog.String("err", err.Error()))
					}
				})
			}

			if mcpStdio {
				logger.Info("simian serve: MCP stdio mode")
				return srv.ServeStdio(ctx)
			}

			sse := srv.ServeSSE(mcpAddr)
			var handler http.Handler = sse
			if ui {
				// The web UI shares the MCP port: one port-forward reaches
				// both, and the page needs no second deployment.
				var autonomousIn []string
				if autonomous {
					autonomousIn = autonomousNS
				}
				uiHandler := webui.Handler(webui.Deps{
					Version: version, Active: exec, Faults: faultLog, Cycles: cycleLog, Topology: disco2,
					Arenas: arenaNamespaces, Autonomous: autonomousIn, Events: broadcaster,
				})
				mux := http.NewServeMux()
				mux.Handle("/ui", uiHandler)
				mux.Handle("/ui/", uiHandler)
				mux.Handle("/api/", uiHandler)
				mux.Handle("/", sse)
				handler = mux
				logger.Info("simian serve: web UI at /ui/", slog.String("addr", mcpAddr))
			}
			httpSrv := &http.Server{Addr: mcpAddr, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
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
	cmd.Flags().BoolVar(&ui, "ui", true, "Serve the read-only web UI at /ui/ (and its /api/) on the MCP address")
	cmd.Flags().StringVar(&metricsAddr, "metrics-addr", ":9090", "Serve Prometheus metrics on this address (/metrics); empty disables")
	cmd.Flags().BoolVar(&debugLLMPayloads, "debug-llm-payloads", false, "Log raw LLM responses (debug only; do not enable in production — see design.md §12.2)")
	cmd.Flags().StringVar(&auditFile, "audit-file", "", "Also append audit events to this file as JSON lines, so the trail outlives the process. Read it with 'simian audit export'. At start-up, faults a previous process applied and never closed get a closing event from it.")
	cmd.Flags().Int64Var(&auditFileMaxBytes, "audit-file-max-bytes", audit.DefaultFileMaxBytes, "Rotate --audit-file to <file>.1 past this size (one previous generation is kept)")
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

// primeMetrics primes recorder for the current arenas and catalog now and
// every interval until ctx ends. A driver whose catalog cannot be read is
// left out of that round, not the others.
func primeMetrics(ctx context.Context, recorder *metrics.Recorder, arenas func(context.Context) ([]string, error),
	drivers map[simian.Engine]simian.ChaosDriver, interval time.Duration, logger *slog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if ns, err := arenas(ctx); err != nil {
			logger.Debug("simian serve: metrics: cannot resolve arenas", slog.String("error", err.Error()))
		} else {
			var catalog []simian.CatalogEntry
			for _, d := range drivers {
				if entries, err := d.Catalog(ctx); err == nil {
					catalog = append(catalog, entries...)
				}
			}
			recorder.Prime(ns, catalog)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
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

// shutdownWait bounds how long serve waits for its background loops after it
// is told to stop. Long enough for an interrupted fault's rollback (the
// executor gives that clear 10s), short of the 30s a pod gets by default
// before SIGKILL.
const shutdownWait = 15 * time.Second

// historyMemory is how far back a restarted controller looks for faults and
// refusals to carry over. Bounded so one refused under a policy or build
// since changed is not held against a planner indefinitely.
const historyMemory = 24 * time.Hour

// waitForWorkers waits for wg, giving up after limit so a wedged loop cannot
// hold the process past its grace period.
func waitForWorkers(wg *sync.WaitGroup, limit time.Duration, logger *slog.Logger) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(limit):
		logger.Warn("simian serve: background loops still running at shutdown; exiting anyway",
			slog.Duration("waited", limit))
	}
}

// openAuditFile reads what a previous process left in the audit file, opens
// it for appending, and returns the records it read and a function that takes over the faults that
// process left open. The taking over is deferred to the caller so the events
// go through every sink, not just the file, and so the lease registry exists
// by then.
//
// A fault still running is offered to adopt; one it takes gets a
// lease.adopted event and is from then on this process's to count and clear.
// Every other open fault — ended, or not adoptable — is closed on the record
// as untracked-after-restart. A nil adopt closes everything.
func openAuditFile(path string, maxBytes int64, logger *slog.Logger) (*audit.FileAuditor, []audit.Record, func(context.Context, simian.Auditor, func(audit.FaultRow) bool), error) {
	fa, err := audit.OpenFile(path, maxBytes, func(err error) {
		logger.Error("simian serve: audit file", slog.String("error", err.Error()))
	})
	if err != nil {
		return nil, nil, nil, err
	}
	var records []audit.Record
	for _, p := range fa.Paths() {
		f, err := os.Open(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			_ = fa.Close()
			return nil, nil, nil, fmt.Errorf("audit file: %w", err)
		}
		recs, err := audit.ReadRecords(f)
		_ = f.Close()
		if err != nil {
			logger.Warn("simian serve: audit file partly unreadable", slog.String("path", p), slog.String("error", err.Error()))
		}
		records = append(records, recs...)
	}
	rows := audit.Faults(records)
	return fa, records, func(ctx context.Context, a simian.Auditor, adopt func(audit.FaultRow) bool) {
		var left []audit.FaultRow
		adopted := 0
		for _, r := range rows {
			if r.Outcome != audit.OutcomeOpen {
				continue
			}
			if adopt == nil || !adopt(r) {
				left = append(left, r)
				continue
			}
			adopted++
			a.Emit(ctx, simian.AuditEvent{
				Event: audit.EventLeaseAdopted, FaultUID: r.FaultUID, PlanID: r.PlanID, ScenarioID: r.ScenarioID,
				Mode: simian.ManifestSource(r.Source), Reason: audit.ReasonUntrackedAfterRestart,
				Payload: map[string]any{"engine_uid": r.EngineUID, "deadline": r.Deadline, "found_in": "audit-file"},
			})
		}
		closing := audit.ClosingEvents(left, time.Now().UTC())
		for _, c := range closing {
			a.Emit(ctx, simian.AuditEvent{
				Event: c.Event, FaultUID: c.FaultUID, PlanID: c.PlanID, ScenarioID: c.ScenarioID,
				Mode: simian.ManifestSource(c.Mode), Reason: c.Reason, Payload: c.Payload,
			})
		}
		if adopted > 0 {
			logger.Info("simian serve: adopted faults a previous process left running", slog.Int("faults", adopted))
		}
		if len(closing) > 0 {
			logger.Info("simian serve: closed faults a previous process left open", slog.Int("faults", len(closing)))
		}
	}, nil
}
