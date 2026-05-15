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
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/go-steer/simian-agent/pkg/arena"
	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/driver/chaosmesh"
	"github.com/go-steer/simian-agent/pkg/executor"
	"github.com/go-steer/simian-agent/pkg/lease"
	"github.com/go-steer/simian-agent/pkg/loop"
	"github.com/go-steer/simian-agent/pkg/planner"
	"github.com/go-steer/simian-agent/pkg/simian"
	"github.com/go-steer/simian-agent/pkg/sut"
	"github.com/go-steer/simian-agent/pkg/topology"
)

func newPlanCmd() *cobra.Command {
	var (
		kubeconfig          string
		namespace           string
		hypothesis          string
		dryRun              bool
		out                 string
		llmProviderID       string
		llmModel            string
		maxFaultsPerCycle   int
		maxSeverityPerCycle string
		topologyResync      time.Duration
	)
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Run an autonomous-mode planning cycle (default dry-run, no apply)",
		Long: `Generate an AttackPlan for an arena namespace by asking the LLM and
exporting it as JSON. By default plans are emitted but NOT applied; pass
--dry-run=false to actually execute the plan via the same in-process
executor pipeline 'simian serve --autonomous' uses.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if namespace == "" {
				return fmt.Errorf("--namespace is required")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Minute)
			defer cancel()

			logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
			auditor := audit.New(logger)

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

			cmDriver := chaosmesh.New(dyn, cached, "simian-")
			drivers := map[simian.Engine]simian.ChaosDriver{simian.EngineChaosMesh: cmDriver}

			// Eligibility honors the same annotation lookup serve uses.
			elig := arena.NewAnnotationEligibility(clientset)

			registry := lease.NewRegistry("simian-plan-cli")
			realExec := executor.New(executor.DefaultConfig(), drivers, registry, auditor, elig)

			// Topology discoverer: cluster-scoped informers, sync once before
			// snapshotting so the plan sees a populated cache.
			disc := topology.New(clientset, topologyResync)
			disc.Start()
			if !disc.WaitForSync(ctx) {
				return fmt.Errorf("topology: cache sync timed out")
			}
			defer disc.Stop()

			sutMgr := sut.NewManager(clientset, dyn, cached, sut.Default)

			llm, err := buildLLM(ctx, llmProviderID, llmModel)
			if err != nil {
				return fmt.Errorf("llm provider: %w", err)
			}
			gen := planner.NewGenerator(llm)

			catalogFn := func(c context.Context) ([]simian.CatalogEntry, error) {
				return cmDriver.Catalog(c)
			}

			var exec simian.FaultExecutor = realExec
			var dry *dryRunExecutor
			if dryRun {
				dry = &dryRunExecutor{}
				exec = dry
			}

			lp := &loop.Loop{
				Namespaces: []string{namespace},
				Interval:   1 * time.Hour, // unused; we call RunOnce directly
				Generator:  gen,
				Executor:   exec,
				Topology:   disc,
				Baselines:  sutMgr,
				Recents:    realExec, // even in dry-run we surface real recents
				Catalog:    catalogFn,
				// No health gate for the plan CLI: we want to see what the
				// LLM proposes even when the arena is mid-recovery. Apply
				// path (when --dry-run=false) still goes through the
				// executor's full safety stage.
				Health: nil,
				Budget: planner.Budget{
					MaxFaultsPerCycle:   maxFaultsPerCycle,
					MaxConcurrentFaults: 1,
					MaxSeverityPerCycle: simian.BlastRadiusTier(maxSeverityPerCycle),
				},
				Auditor:    auditor,
				Logger:     logger,
				Hypothesis: hypothesis,
			}

			plan, applied, err := lp.RunOnce(ctx, namespace)
			if err != nil {
				return err
			}
			if dry != nil {
				logger.Info("plan: dry-run", slog.Int("planned_steps", len(plan.Steps)), slog.Int("dryrun_apply_calls", len(dry.calls)))
			} else {
				logger.Info("plan: applied", slog.Int("applied_count", len(applied)))
			}
			b, err := json.MarshalIndent(plan, "", "  ")
			if err != nil {
				return fmt.Errorf("marshal plan: %w", err)
			}
			if out == "" || out == "-" {
				_, _ = os.Stdout.Write(b)
				_, _ = os.Stdout.WriteString("\n")
				return nil
			}
			return os.WriteFile(out, b, 0o644)
		},
	}
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig (default: in-cluster, then $KUBECONFIG, then ~/.kube/config)")
	cmd.Flags().StringVar(&namespace, "namespace", "", "Arena namespace to plan against (required)")
	cmd.Flags().StringVar(&hypothesis, "hypothesis", "", "Optional hypothesis hint for the planner")
	cmd.Flags().BoolVar(&dryRun, "dry-run", true, "Emit the plan as JSON without applying it")
	cmd.Flags().StringVar(&out, "out", "", "Write plan JSON to this file (default: stdout)")
	cmd.Flags().StringVar(&llmProviderID, "llm-provider", "gemini", "LLM provider id (gemini|stub)")
	cmd.Flags().StringVar(&llmModel, "llm-model", "", "Model override (provider default if empty)")
	cmd.Flags().IntVar(&maxFaultsPerCycle, "max-faults-per-cycle", 3, "Per-cycle cap on faults declared / applied")
	cmd.Flags().StringVar(&maxSeverityPerCycle, "max-severity-per-cycle", "namespace", "Highest blast-radius tier this plan may declare (namespace|node|external)")
	cmd.Flags().DurationVar(&topologyResync, "topology-resync", 30*time.Second, "Topology informer resync interval")
	return cmd
}

// dryRunExecutor records Apply calls without invoking any driver. Clear is
// a no-op; ListActive returns empty so the loop's health gate logic still
// works if used.
type dryRunExecutor struct {
	mu    sync.Mutex
	calls []simian.FaultManifest
}

func (d *dryRunExecutor) Apply(_ context.Context, m simian.FaultManifest) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, m)
	return "dryrun-" + m.ResourceKind, nil
}
func (d *dryRunExecutor) Clear(_ context.Context, _ string) error { return nil }
func (d *dryRunExecutor) ListActive(_ context.Context, _ string) ([]simian.ActiveFault, error) {
	return nil, nil
}
