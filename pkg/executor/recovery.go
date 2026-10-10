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

package executor

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/catalog"
	"github.com/go-steer/simian-agent/pkg/simian"
)

// DefaultRecoveryTimeout is how long CheckRecovery gives a fault's targets to
// be Ready again after it is cleared.
const DefaultRecoveryTimeout = 5 * time.Minute

// recoveryPoll is how often CheckRecovery looks. A variable for tests.
var recoveryPoll = 15 * time.Second

// CheckRecovery waits for the pods a cleared fault targeted to be Ready again,
// and records the verdict as fault.recovered: passed, how long it waited, and
// on failure which pods are still down and why.
//
// Clearing a fault is not the same as the workload recovering from it. On the
// 2026-10-03 soak an HTTPChaos ran out its deadline and was recorded as
// lease.expired deadline-reached, while the proxy it left behind kept bank's
// frontend unreachable until someone deleted the pod. The health gate paused
// the arena, rightly, but nothing in the record said why. This is that record.
//
// Only Chaos Mesh faults are checked: they target workloads that exist
// before and after the fault. A kube-state or network-policy fault's objects
// are the fault, and are gone when it is. A shutdown before the verdict
// records nothing rather than a failure it did not observe.
func (e *Executor) CheckRecovery(ctx context.Context, af simian.ActiveFault) {
	if e.pods == nil || af.Manifest.Engine != simian.EngineChaosMesh {
		return
	}
	timeout := e.cfg.RecoveryTimeout
	if timeout <= 0 {
		timeout = DefaultRecoveryTimeout
	}
	start := time.Now()
	deadline := start.Add(timeout)
	for {
		unready, checked := e.unreadyTargets(ctx, af.Manifest)
		if !checked {
			return // nothing it can judge: no labelled target, or pods unlistable
		}
		if len(unready) == 0 || !time.Now().Before(deadline) {
			payload := map[string]any{"passed": len(unready) == 0, "waited": time.Since(start).Round(time.Second).String()}
			if len(unready) > 0 {
				payload["unready"] = unready
			}
			e.auditor.Emit(ctx, simian.AuditEvent{
				Event: audit.EventFaultRecovered, FaultUID: af.FaultUID, PlanID: af.Manifest.PlanID, Mode: af.Manifest.Source,
				Payload: payload,
			})
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(recoveryPoll):
		}
	}
}

// unreadyTargets lists the target pods that are not Ready, as "ns/pod: why".
// checked is false when no target could be judged at all.
func (e *Executor) unreadyTargets(ctx context.Context, m simian.FaultManifest) (unready []string, checked bool) {
	// An outage aimed at the arena as a whole took down whatever had pods in
	// the zone, so recovery is judged over every pod in the namespace.
	wholeArena := catalog.IsOutageKind(m.Engine, m.ResourceKind)
	for _, t := range m.Targets {
		if t.Namespace == "" || (len(t.Labels) == 0 && (!wholeArena || t.Name != "")) {
			continue
		}
		pods, err := e.pods.Pods(ctx, t.Namespace, t.Labels)
		if err != nil {
			continue
		}
		checked = true
		live := 0
		for _, p := range pods {
			if p.DeletionTimestamp != nil || (len(t.Labels) == 0 && p.Status.Phase == corev1.PodSucceeded) {
				continue
			}
			live++
			if why := notReadyReason(p); why != "" {
				unready = append(unready, fmt.Sprintf("%s/%s: %s", p.Namespace, p.Name, why))
			}
		}
		if live == 0 {
			unready = append(unready, fmt.Sprintf("%s/%v: no pods", t.Namespace, t.Labels))
		}
	}
	sort.Strings(unready)
	return unready, checked
}

// notReadyReason is empty for a Ready pod, and otherwise the most telling
// thing its status says.
func notReadyReason(p corev1.Pod) string {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return ""
		}
	}
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			return fmt.Sprintf("container %s %s, %d restarts", cs.Name, cs.State.Waiting.Reason, cs.RestartCount)
		}
		if !cs.Ready {
			return fmt.Sprintf("container %s not ready, %d restarts", cs.Name, cs.RestartCount)
		}
	}
	if p.Status.Phase != "" {
		return "phase " + string(p.Status.Phase)
	}
	return "not ready"
}
