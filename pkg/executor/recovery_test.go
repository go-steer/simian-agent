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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/go-steer/simian-agent/internal/testutil"
	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/lease"
	"github.com/go-steer/simian-agent/pkg/simian"
)

func recoveryPod(name string, ready bool, waiting string) *corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	cs := corev1.ContainerStatus{Name: "front", Ready: ready, RestartCount: 6}
	if waiting != "" {
		cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: waiting}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "bank", Labels: map[string]string{"app": "frontend"}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}},
			ContainerStatuses: []corev1.ContainerStatus{cs}},
	}
}

func newRecoveryExecutor(t *testing.T, pods ...*corev1.Pod) (*Executor, *fake.Clientset, *testutil.FakeAuditor) {
	t.Helper()
	old := recoveryPoll
	recoveryPoll = 10 * time.Millisecond
	t.Cleanup(func() { recoveryPoll = old })
	cs := fake.NewClientset()
	for _, p := range pods {
		if _, err := cs.CoreV1().Pods("bank").Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := DefaultConfig()
	cfg.RecoveryTimeout = 200 * time.Millisecond
	aud := &testutil.FakeAuditor{}
	exec := New(cfg, nil, lease.NewRegistry("h"), aud, &StaticEligibility{}, WithTargetPods(KubernetesTargetPods{Client: cs}))
	return exec, cs, aud
}

func clearedFault(engine simian.Engine) simian.ActiveFault {
	return simian.ActiveFault{FaultUID: "f-1", Manifest: simian.FaultManifest{UID: "f-1", Engine: engine, ResourceKind: "HTTPChaos",
		Targets: []simian.TargetRef{{Namespace: "bank", Name: "frontend", Labels: map[string]string{"app": "frontend"}}}}}
}

func recovered(t *testing.T, aud *testutil.FakeAuditor) []simian.AuditEvent {
	t.Helper()
	var out []simian.AuditEvent
	for _, e := range aud.Events {
		if e.Event == audit.EventFaultRecovered {
			out = append(out, e)
		}
	}
	return out
}

// The soak's case: the fault ended on time and the workload did not come back.
func TestAFaultWhoseTargetsStayDownIsRecordedAsNotRecovered(t *testing.T) {
	exec, _, aud := newRecoveryExecutor(t, recoveryPod("frontend-a", false, "CrashLoopBackOff"), recoveryPod("frontend-b", true, ""))
	exec.CheckRecovery(context.Background(), clearedFault(simian.EngineChaosMesh))
	ev := recovered(t, aud)
	if len(ev) != 1 || ev[0].Payload["passed"] != false || ev[0].FaultUID != "f-1" {
		t.Fatalf("fault.recovered = %+v, want one, passed false", ev)
	}
	unready, _ := ev[0].Payload["unready"].([]string)
	if len(unready) != 1 || !strings.Contains(unready[0], "bank/frontend-a: container front CrashLoopBackOff, 6 restarts") {
		t.Errorf("unready = %v", unready)
	}
}

func TestAFaultWhoseTargetsComeBackIsRecordedAsRecovered(t *testing.T) {
	exec, cs, aud := newRecoveryExecutor(t, recoveryPod("frontend-a", false, ""))
	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _ = cs.CoreV1().Pods("bank").Update(context.Background(), recoveryPod("frontend-a", true, ""), metav1.UpdateOptions{})
	}()
	exec.CheckRecovery(context.Background(), clearedFault(simian.EngineChaosMesh))
	ev := recovered(t, aud)
	if len(ev) != 1 || ev[0].Payload["passed"] != true || ev[0].Payload["unready"] != nil {
		t.Fatalf("fault.recovered = %+v, want one, passed true", ev)
	}
}

// A shutdown is not an observation that the workload stayed down, and a
// fault whose objects are the fault has nothing to come back.
func TestNoRecoveryVerdictWithoutOne(t *testing.T) {
	exec, _, aud := newRecoveryExecutor(t, recoveryPod("frontend-a", false, ""))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	exec.CheckRecovery(ctx, clearedFault(simian.EngineChaosMesh))
	exec.CheckRecovery(context.Background(), clearedFault(simian.EngineKubeState))
	if ev := recovered(t, aud); len(ev) != 0 {
		t.Errorf("fault.recovered = %+v, want none", ev)
	}
}
