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

// Package uidev serves the web UI with stand-ins for the controller, for
// looking at the page and driving it in a browser without a cluster:
//
//	SIMIAN_UIDEV=:18090 go test ./internal/uidev -run TestServe -timeout 0
//
// SIMIAN_UIDEV_AS sets the signed-in user (default a writer); "none" serves
// it as behind a port-forward. Skipped unless SIMIAN_UIDEV is set.
package uidev

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/loop"
	"github.com/go-steer/simian-agent/pkg/simian"
	"github.com/go-steer/simian-agent/pkg/topology"
	"github.com/go-steer/simian-agent/pkg/webui"
)

type as struct{ id webui.Identity }

func (a as) Identify(*http.Request) (webui.Identity, error) { return a.id, nil }

type world struct {
	mu     sync.Mutex
	active []simian.ActiveFault
	log    *audit.FaultLog
	events *webui.Broadcaster
	n      int
}

func (w *world) emit(ctx context.Context, e simian.AuditEvent) {
	w.log.Emit(ctx, e)
	w.events.Emit(ctx, e)
}

func (w *world) Apply(ctx context.Context, m simian.FaultManifest) (string, error) {
	w.mu.Lock()
	w.n++
	uid := fmt.Sprintf("f-01UIDEV%04d", w.n)
	if m.Targets[0].Name == "loadgenerator" {
		// Refused, and recorded as the executor records it, so the page
		// has a refused row to show.
		w.mu.Unlock()
		m.UID = uid
		rec := m.AuditRecord()
		if a := simian.ActorFrom(ctx); a != "" {
			rec["actor"] = a
		}
		w.emit(ctx, simian.AuditEvent{Event: audit.EventExecutorReceived, FaultUID: uid, Mode: m.Source, Payload: rec})
		w.emit(ctx, simian.AuditEvent{Event: audit.EventExecutorRejected, FaultUID: uid, Mode: m.Source, Reason: string(simian.ReasonWorkloadExcluded),
			Payload: map[string]any{"error": "executor[safety:workload-excluded]: workload Deployment/loadgenerator is excluded from chaos"}})
		return "", &simian.ExecutorError{Stage: simian.StageSafety, Reason: simian.ReasonWorkloadExcluded, Message: "workload Deployment/loadgenerator is excluded from chaos"}
	}
	m.UID = uid
	now := time.Now().UTC()
	w.active = append(w.active, simian.ActiveFault{FaultUID: uid, Manifest: m, AppliedAt: now, Deadline: now.Add(m.Duration)})
	w.mu.Unlock()
	rec := m.AuditRecord()
	if a := simian.ActorFrom(ctx); a != "" {
		rec["actor"] = a
	}
	w.emit(ctx, simian.AuditEvent{Event: audit.EventExecutorReceived, FaultUID: uid, Mode: m.Source, Payload: rec})
	w.emit(ctx, simian.AuditEvent{Event: audit.EventDriverApplied, FaultUID: uid, Mode: m.Source, Payload: m.AuditRecord()})
	return uid, nil
}

func (w *world) Clear(ctx context.Context, uid string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i, f := range w.active {
		if f.FaultUID == uid {
			w.active = append(w.active[:i], w.active[i+1:]...)
			p := map[string]any{}
			if a := simian.ActorFrom(ctx); a != "" {
				p["actor"] = a
			}
			go w.emit(ctx, simian.AuditEvent{Event: audit.EventLeaseCleared, FaultUID: uid, Reason: "explicit-clear", Payload: p})
			return nil
		}
	}
	return fmt.Errorf("fault %q not found", uid)
}

func (w *world) ListActive(context.Context, string) ([]simian.ActiveFault, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]simian.ActiveFault(nil), w.active...), nil
}

type topo struct{}

func (topo) Snapshot(_ context.Context, ns string) (*topology.TargetTopology, error) {
	t := &topology.TargetTopology{Namespace: ns, PodStatus: map[string][]topology.PodSummary{}}
	for _, n := range []string{"cartservice", "frontend", "loadgenerator", "redis-cart"} {
		t.Workloads = append(t.Workloads, topology.Workload{Kind: "Deployment", Name: n, DesiredReplicas: 1})
		t.PodStatus[n] = []topology.PodSummary{{Name: n + "-x", Ready: true}}
	}
	return t, nil
}

func TestServe(t *testing.T) {
	addr := os.Getenv("SIMIAN_UIDEV")
	if addr == "" {
		t.Skip("SIMIAN_UIDEV not set")
	}
	ctx := context.Background()
	w := &world{log: audit.NewFaultLog(0), events: webui.NewBroadcaster(ctx)}
	var auth webui.Authenticator = as{webui.Identity{Email: "alice@example.com", CanWrite: true, CanAdmin: true, Auth: "iap"}}
	switch who := os.Getenv("SIMIAN_UIDEV_AS"); who {
	case "":
	case "none":
		auth = webui.NoAuth{}
	default:
		auth = as{webui.Identity{Email: who, Auth: "iap"}}
	}
	control := loop.NewControl(loop.Settings{Namespaces: []string{"boutique"}, Interval: 10 * time.Minute, MaxFaultsPerCycle: 1, MaxSeverityPerCycle: simian.TierNamespace})
	h := webui.Handler(webui.Deps{
		Install:  webui.Install{MaxConcurrentFaults: 1, DurationCeiling: "5m0s", MinCooldown: "1m0s", PermittedTiers: []simian.BlastRadiusTier{simian.TierNamespace}, DefaultProbes: true, LLM: "gemini"},
		Autonomy: &webui.Autonomy{Control: control},
		Limits: func(context.Context) (loop.Limits, error) {
			return loop.Limits{MaxConcurrentFaults: 1, PermittedTiers: []simian.BlastRadiusTier{simian.TierNamespace}, Arenas: []string{"bank", "boutique"}}, nil
		},
		Excluded: func(_ context.Context, ns string) ([]string, error) {
			if ns == "boutique" {
				return []string{"loadgenerator"}, nil
			}
			return nil, nil
		},
		Auditor: w.log,
		Version: "dev", Active: w, Faults: w.log, Topology: topo{}, Events: w.events, Auth: auth, Executor: w,
		Arenas: func(context.Context) ([]string, error) { return []string{"bank", "boutique"}, nil },
		Catalog: func(context.Context) ([]simian.CatalogEntry, error) {
			return []simian.CatalogEntry{
				{Engine: simian.EngineChaosMesh, APIVersion: "chaos-mesh.org/v1alpha1", ResourceKind: "PodChaos",
					SpecTemplate: "action MUST be one of: \"pod-kill\" | \"pod-failure\" | \"container-kill\"\n{\"action\": \"pod-kill\", \"mode\": \"one\",\n \"selector\": {\"namespaces\": [\"<ns>\"], \"labelSelectors\": {\"app\": \"<workload>\"}}}"},
				{Engine: simian.EngineChaosMesh, APIVersion: "chaos-mesh.org/v1alpha1", ResourceKind: "NetworkChaos",
					SpecTemplate: "{\"action\": \"delay\", \"mode\": \"all\",\n \"selector\": {\"namespaces\": [\"<ns>\"]},\n \"delay\": {\"latency\": \"250ms\", \"correlation\": \"0\", \"jitter\": \"0ms\"}}"},
			}, nil
		},
		Translate: func(_ context.Context, intent, ns string, d time.Duration) (simian.FaultManifest, error) {
			return simian.FaultManifest{Engine: simian.EngineChaosMesh, APIVersion: "chaos-mesh.org/v1alpha1", ResourceKind: "NetworkChaos",
				Spec: map[string]any{"action": "delay"}, Targets: []simian.TargetRef{{Namespace: ns, Name: "cartservice"}}, Duration: d, Rationale: intent}, nil
		},
	})
	t.Logf("serving the UI at http://localhost%s/ui/", addr)
	if err := http.ListenAndServe(addr, h); err != nil {
		t.Fatal(err)
	}
}
