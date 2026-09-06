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
	"fmt"
	"log/slog"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"

	"github.com/go-steer/simian-agent/pkg/arena"
	"github.com/go-steer/simian-agent/pkg/catalog"
	"github.com/go-steer/simian-agent/pkg/driver/chaosmesh"
	"github.com/go-steer/simian-agent/pkg/driver/envoyfault"
	"github.com/go-steer/simian-agent/pkg/driver/kubestate"
	"github.com/go-steer/simian-agent/pkg/driver/networkpolicy"
	"github.com/go-steer/simian-agent/pkg/executor"
	"github.com/go-steer/simian-agent/pkg/harness"
	"github.com/go-steer/simian-agent/pkg/lease"
	"github.com/go-steer/simian-agent/pkg/probe"
	"github.com/go-steer/simian-agent/pkg/simian"
	"github.com/go-steer/simian-agent/pkg/sut"
	"github.com/go-steer/simian-agent/pkg/sut/edgeupstream"
	"github.com/go-steer/simian-agent/pkg/sut/onlineboutique"
)

func init() {
	// Built-in SUTs register at process start, not inside buildPlane:
	// MustRegister panics on a duplicate name, and a plane built twice in one
	// process — which a test does — would take the whole binary down.
	onlineboutique.Register()
	edgeupstream.Register()
}

// plane is everything the run needs on the cluster side, built once.
type plane struct {
	k8s      kubernetes.Interface
	executor *executor.Executor
	arenas   *arena.Manager
	reaper   *lease.Reaper

	// substrates is nil-free but idle unless a scenario asks for a SUT. It
	// carries no Store, so the baselines it captures live and die with the
	// process — an eval run has no use for a baseline that outlives it, and
	// writing one into a ConfigMap in the arena would leave an object in the
	// namespace the subject is about to be asked to describe.
	substrates *harness.SUTSubstrates
}

// buildPlane wires the same four engines, the same safety config, the same
// probe mux and the same lease registry that `simian serve` wires.
//
// Deliberately the same, and deliberately built from the exported pieces
// rather than from a test double. The harness injects through
// executor.Apply — validation, safety stages, eligibility, efficacy gates and
// all — because an eval that skipped any of those would be measuring a
// Simian nobody runs. The first thing it would stop catching is the executor
// rejecting a fault it should have applied.
func buildPlane(cfg *rest.Config, o *options, auditor simian.Auditor, logger *slog.Logger) (*plane, error) {
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}
	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("discovery client: %w", err)
	}
	cached := memory.NewMemCacheClient(disco)

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes clientset: %w", err)
	}

	drivers := map[simian.Engine]simian.ChaosDriver{
		simian.EngineChaosMesh:     chaosmesh.New(dyn, cached, "simian-"),
		simian.EngineNetworkPolicy: networkpolicy.New(clientset, ""),
		simian.EngineEnvoyFault:    envoyfault.New(clientset),
		simian.EngineKubeState:     kubestate.New(clientset),
	}

	execCfg := executor.DefaultConfig()
	if o.durationCap > 0 {
		execCfg.DurationCeiling = o.durationCap
	}

	prober := probe.NewMux(map[string]probe.Prober{
		simian.ProbeTypeK8s:  probe.NewK8sProber(dyn, restmapper.NewDeferredDiscoveryRESTMapper(cached)),
		simian.ProbeTypeHTTP: probe.NewKubernetesHTTPProber(clientset),
		simian.ProbeTypeLogs: probe.NewKubernetesLogsProber(clientset),
	})
	execOpts := []executor.Option{executor.WithProber(prober)}
	if o.defaultProbes {
		execOpts = append(execOpts, executor.WithDefaultProbes(catalog.DefaultProbes))
	}

	registry := lease.NewRegistry("simian-eval")
	exec := executor.New(execCfg, drivers, registry, auditor, buildEligibility(clientset, o.eligibleNS, logger), execOpts...)

	mgr := arena.New(clientset, o.chaosSA, o.chaosSANS)
	mgr.Dyn = dyn

	return &plane{
		k8s:      clientset,
		executor: exec,
		arenas:   mgr,
		substrates: &harness.SUTSubstrates{
			Manager:  sut.NewManager(clientset, dyn, cached, sut.Default),
			Registry: sut.Default,
		},
		reaper: &lease.Reaper{
			Registry: registry,
			Drivers:  drivers,
			Interval: o.reapInterval,
			Auditor:  auditor,
			// Namespaces stays nil: the orphan scan deletes chaos this
			// process did not apply, and a rig pointed at somebody's real
			// cluster has no business doing that. The reaper here is only the
			// backstop for leases this run took out and failed to release.
		},
	}, nil
}

// buildEligibility picks between the annotation lookup and a static allowlist.
//
// Annotation by default, which means the only thing standing between a
// scenario and a namespace is that somebody annotated it — and the harness
// annotates the arenas it creates. --eligible-namespace is the tighter
// setting: it fences the run to a fixed list regardless of what is annotated,
// which is what to reach for when the cluster has other tenants.
func buildEligibility(k8s kubernetes.Interface, eligible []string, logger *slog.Logger) executor.EligibilityChecker {
	if len(eligible) > 0 {
		m := make(map[string]bool, len(eligible))
		for _, ns := range eligible {
			m[ns] = true
		}
		logger.Info("simian-eval: eligibility fenced to a static allowlist", slog.Any("namespaces", eligible))
		return &executor.StaticEligibility{Eligible: m}
	}
	logger.Info("simian-eval: eligibility by annotation", slog.String("annotation", arena.EligibilityAnnotation+`="true"`))
	return arena.NewAnnotationEligibility(k8s)
}

// checkReachable fails the run before it creates anything if the cluster is
// not there. The alternative is the same connection error once per scenario,
// with a scorecard full of inject failures on the end of it.
func checkReachable(k8s kubernetes.Interface) (string, error) {
	v, err := k8s.Discovery().ServerVersion()
	if err != nil {
		return "", fmt.Errorf("cluster is not reachable: %w", err)
	}
	return v.GitVersion, nil
}
