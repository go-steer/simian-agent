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
	"testing"

	"github.com/go-steer/simian-agent/pkg/simian"
)

func TestARefusedFaultIsRememberedWithItsReason(t *testing.T) {
	exec, _, _ := newTestExecutorWithHistory(t, DefaultConfig(), map[string]bool{}, nil)
	if _, err := exec.Apply(context.Background(), goodManifest()); err == nil {
		t.Fatal("Apply succeeded in a namespace that is not eligible")
	}

	refused := exec.Refused("online-boutique", 10)
	if len(refused) != 1 {
		t.Fatalf("refusals = %d, want 1", len(refused))
	}
	rf := refused[0]
	if rf.Stage != simian.StageSafety || rf.Reason == "" || rf.Error == "" || rf.FaultUID == "" {
		t.Errorf("refusal = %+v, want stage, reason, error and uid set", rf)
	}
	if rf.Manifest.ResourceKind != "NetworkChaos" {
		t.Errorf("refusal does not carry the manifest: %+v", rf.Manifest)
	}
	if got := exec.Refused("elsewhere", 10); len(got) != 0 {
		t.Errorf("refusal listed under another namespace: %+v", got)
	}
	if got := len(exec.Recent("", 10)); got != 0 {
		t.Errorf("a refused fault is in the recent-faults history (%d entries)", got)
	}
}

// A budget refusal is about the moment, not the fault: the same fault may run
// a minute later, and a planner told not to repeat it would stop proposing it.
func TestABudgetRefusalIsNotRemembered(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxConcurrentFaults = 1
	exec, _, _ := newTestExecutorWithHistory(t, cfg, map[string]bool{"online-boutique": true}, nil)
	if _, err := exec.Apply(context.Background(), goodManifest()); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	_, err := exec.Apply(context.Background(), goodManifest())
	if ee := asExecutorError(t, err); ee.Reason != simian.ReasonBudgetExceeded {
		t.Fatalf("reason = %q, want %q", ee.Reason, simian.ReasonBudgetExceeded)
	}
	if got := exec.Refused("", 10); len(got) != 0 {
		t.Errorf("budget refusal remembered: %+v", got)
	}
}

func TestRefusalHistoryIsBoundedAndNewestFirst(t *testing.T) {
	h := NewHistory(2)
	for _, uid := range []string{"a", "b", "c"} {
		h.PushRefused(RefusedFault{FaultUID: uid})
	}
	got := h.ListRefused("", 0)
	if len(got) != 2 || got[0].FaultUID != "c" || got[1].FaultUID != "b" {
		t.Errorf("refusals = %+v, want c, b", got)
	}
}
