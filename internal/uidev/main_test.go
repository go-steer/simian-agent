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
// it as behind a port-forward. SIMIAN_UIDEV_SLOW (e.g. 20s) makes an apply
// outlast the server's wait, so the page sees a 202. Skipped unless
// SIMIAN_UIDEV is set.
package uidev

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
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

// Apply plays the executor's sequence of audit events, with the shapes
// pkg/executor emits, at a pace a person can watch: received, validated,
// (precheck), driver.applied with its deadline and verified_by,
// lease.registered, then — before Apply returns, as the real one waits for
// them — fault.injected and one fault.efficacy per settle probe. At the
// deadline the lease expires and recovery is checked, as the reaper and
// CheckRecovery do. Workload loadgenerator is refused as excluded; a second
// fault while one runs is refused for the budget (maxConcurrentFaults 1);
// redis-cart does not recover. SIMIAN_UIDEV_SLOW (a duration) holds the
// settle wait that long, so a submit outlasts the 15s the server waits and
// answers 202.
func (w *world) Apply(ctx context.Context, m simian.FaultManifest) (string, error) {
	w.mu.Lock()
	w.n++
	if m.UID == "" {
		m.UID = fmt.Sprintf("f-01UIDEV%04d", w.n)
	}
	uid := m.UID
	busy := len(w.active) > 0
	w.mu.Unlock()
	rec := m.AuditRecord()
	if a := simian.ActorFrom(ctx); a != "" {
		rec["actor"] = a
	}
	w.emit(ctx, simian.AuditEvent{Event: audit.EventExecutorReceived, FaultUID: uid, Mode: m.Source, Payload: rec})
	refuse := func(stage simian.ExecutorStage, reason simian.RejectionReason, msg string) error {
		err := &simian.ExecutorError{Stage: stage, Reason: reason, Message: msg}
		w.emit(ctx, simian.AuditEvent{Event: audit.EventExecutorRejected, FaultUID: uid, Mode: m.Source, Reason: string(reason),
			Payload: map[string]any{"error": err.Error()}})
		return err
	}
	target := m.Targets[0]
	switch {
	case target.Name == "loadgenerator":
		return "", refuse(simian.StageSafety, simian.ReasonWorkloadExcluded,
			fmt.Sprintf("workload %q in namespace %q is excluded", target.Name, target.Namespace))
	case m.Duration > 5*time.Minute:
		return "", refuse(simian.StageSafety, simian.ReasonDurationOverCeiling,
			fmt.Sprintf("duration %s exceeds ceiling 5m0s", m.Duration))
	case busy:
		return "", refuse(simian.StageSafety, simian.ReasonBudgetExceeded, "max concurrent faults reached (1)")
	}
	m.Targets[0].Labels = map[string]string{"app": target.Name}
	var probes []string
	if m.ResourceKind == "NetworkChaos" {
		probes = []string{"simian-fast-before", "simian-delayed"}
	}
	validated := map[string]any{"target_labels_from_workload": []string{target.Namespace + "/" + target.Name}}
	if len(probes) > 0 {
		validated["default_probes"] = probes
	}
	w.emit(ctx, simian.AuditEvent{Event: audit.EventExecutorValidated, FaultUID: uid, Mode: m.Source, Payload: validated})
	time.Sleep(300 * time.Millisecond)
	if len(probes) > 0 {
		w.emit(ctx, simian.AuditEvent{Event: audit.EventFaultPrecheck, FaultUID: uid, Mode: m.Source,
			Payload: map[string]any{"probe": probes[0], "type": "tcp", "mode": "sot", "passed": true, "attempts": 1, "elapsed_ms": 140}})
	}
	now := time.Now().UTC()
	deadline := now.Add(m.Duration)
	w.mu.Lock()
	w.active = append(w.active, simian.ActiveFault{FaultUID: uid, Manifest: m, AppliedAt: now, Deadline: deadline})
	w.mu.Unlock()
	applied := m.AuditRecord()
	applied["engine_uid"] = "8d1c" + uid[len(uid)-6:]
	applied["deadline"] = deadline.Format(time.RFC3339)
	applied["verified_by"] = append([]string{"engine-status"}, probes[min(1, len(probes)):]...)
	w.emit(ctx, simian.AuditEvent{Event: audit.EventDriverApplied, FaultUID: uid, Mode: m.Source, Payload: applied})
	w.emit(ctx, simian.AuditEvent{Event: audit.EventLeaseRegistered, FaultUID: uid, Mode: m.Source})
	time.AfterFunc(m.Duration, func() { w.expire(uid) })

	time.Sleep(time.Second)
	w.emit(ctx, simian.AuditEvent{Event: audit.EventFaultInjected, FaultUID: uid, Mode: m.Source,
		Payload: map[string]any{"passed": true, "observed": "AllInjected=True", "elapsed_ms": 900}})
	if slow, err := time.ParseDuration(os.Getenv("SIMIAN_UIDEV_SLOW")); err == nil {
		time.Sleep(slow)
	} else {
		time.Sleep(time.Second)
	}
	if len(probes) > 1 {
		w.emit(ctx, simian.AuditEvent{Event: audit.EventFaultEfficacy, FaultUID: uid, Mode: m.Source,
			Payload: map[string]any{"probe": probes[1], "type": "tcp", "mode": "settle", "passed": true, "observed": "connect 312ms", "expected": ">= 125ms", "attempts": 2, "elapsed_ms": 2100}})
	}
	return uid, nil
}

// expire is the reaper at the deadline, then CheckRecovery: Chaos Mesh
// faults only, and only for a fault that ran out (a cleared one is not
// checked).
func (w *world) expire(uid string) {
	ctx := context.Background()
	w.mu.Lock()
	var af *simian.ActiveFault
	for i, f := range w.active {
		if f.FaultUID == uid {
			af = &f
			w.active = append(w.active[:i], w.active[i+1:]...)
			break
		}
	}
	w.mu.Unlock()
	if af == nil {
		return // cleared before its deadline
	}
	w.emit(ctx, simian.AuditEvent{Event: audit.EventLeaseExpired, FaultUID: uid, Reason: "deadline-reached"})
	if af.Manifest.Engine != simian.EngineChaosMesh {
		return
	}
	time.Sleep(2 * time.Second)
	p := map[string]any{"passed": true, "waited": "2s"}
	if t := af.Manifest.Targets[0]; t.Name == "redis-cart" {
		p = map[string]any{"passed": false, "waited": "5m0s", "unready": []string{t.Namespace + "/redis-cart-x: CrashLoopBackOff"}}
	}
	w.emit(ctx, simian.AuditEvent{Event: audit.EventFaultRecovered, FaultUID: uid, Mode: af.Manifest.Source, Payload: p})
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
			if strings.Contains(intent, "nonsense") {
				return simian.FaultManifest{}, errors.New("translate: the model's answer is not a fault: no catalog kind matches")
			}
			time.Sleep(1200 * time.Millisecond) // the LLM takes a moment
			return simian.FaultManifest{Engine: simian.EngineChaosMesh, APIVersion: "chaos-mesh.org/v1alpha1", ResourceKind: "NetworkChaos",
				Spec:    map[string]any{"action": "delay", "mode": "all", "delay": map[string]any{"latency": "300ms", "correlation": "0", "jitter": "50ms"}},
				Targets: []simian.TargetRef{{Namespace: ns, Name: "cartservice"}}, Duration: d,
				Rationale: "Asked: \"" + intent + "\". cartservice sits on the checkout path (frontend → checkout → cart), so delaying its traffic slows checkout without taking it down."}, nil
		},
	})
	t.Logf("serving the UI at http://localhost%s/ui/", addr)
	if err := http.ListenAndServe(addr, h); err != nil {
		t.Fatal(err)
	}
}
