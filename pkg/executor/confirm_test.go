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
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/go-steer/simian-agent/internal/testutil"
	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/lease"
	"github.com/go-steer/simian-agent/pkg/probe"
	"github.com/go-steer/simian-agent/pkg/simian"
)

// confirmingDriver is a FakeDriver whose engine can say whether a fault took.
type confirmingDriver struct {
	*testutil.FakeDriver
	observed string
	err      error
	calls    int
	// during, if set, runs inside ConfirmInjected — a shutdown arriving
	// while the engine is still being asked.
	during func()
}

func (c *confirmingDriver) ConfirmInjected(context.Context, string) (string, error) {
	c.calls++
	if c.during != nil {
		c.during()
	}
	return c.observed, c.err
}

func newConfirmingExecutor(t *testing.T, d *confirmingDriver, prober probe.Prober) (*Executor, *testutil.FakeAuditor, *lease.Registry) {
	t.Helper()
	d.FakeDriver = &testutil.FakeDriver{EngineName: simian.EngineChaosMesh}
	registry := lease.NewRegistry("test-holder")
	auditor := &testutil.FakeAuditor{}
	elig := &StaticEligibility{Eligible: map[string]bool{"online-boutique": true}}
	opts := []Option{WithHistory(NewHistory(10))}
	if prober != nil {
		opts = append(opts, WithProber(prober))
	}
	exec := New(DefaultConfig(), map[simian.Engine]simian.ChaosDriver{simian.EngineChaosMesh: d},
		registry, auditor, elig, opts...)
	return exec, auditor, registry
}

func TestAFaultTheEngineConfirmsIsRecordedAsConfirmed(t *testing.T) {
	d := &confirmingDriver{observed: "AllInjected=True, 2 target(s)"}
	exec, auditor, registry := newConfirmingExecutor(t, d, nil)

	uid, err := exec.Apply(context.Background(), goodManifest())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if d.calls != 1 {
		t.Fatalf("ConfirmInjected calls = %d, want 1", d.calls)
	}
	if _, ok := registry.Get(uid); !ok {
		t.Fatal("confirmed fault not registered")
	}

	ev, ok := auditor.FindEvent(audit.EventFaultInjected)
	if !ok {
		t.Fatal("no fault.injected event")
	}
	if ev.Payload["passed"] != true || ev.Payload["observed"] != d.observed {
		t.Errorf("fault.injected payload = %v", ev.Payload)
	}
	applied, _ := auditor.FindEvent(audit.EventDriverApplied)
	if got, _ := applied.Payload["verified_by"].([]string); !slices.Equal(got, []string{"engine-status"}) {
		t.Errorf("verified_by = %v, want [engine-status]", applied.Payload["verified_by"])
	}
}

// The trial's DNSChaos: accepted, never injected, recorded as run. With the
// engine asked, it has to come out of the cluster and out of the history, and
// the record has to say the engine refused it rather than that a probe did.
func TestAFaultTheEngineCouldNotInjectIsBackedOut(t *testing.T) {
	d := &confirmingDriver{
		observed: "0 of 3 target(s) injected; bank/web-1/nginx: ls: cannot access '/etc/resolv.conf.chaos.bak'",
		err:      errors.New("chaos-mesh: bank/simian-wl7jn not injected after 30s"),
	}
	prober := &fakeProber{}
	exec, auditor, registry := newConfirmingExecutor(t, d, prober)

	m := goodManifest()
	m.Probes = []simian.ProbeSpec{settleProbe("dns-fails")}
	uid, err := exec.Apply(context.Background(), m)
	if err == nil {
		t.Fatal("Apply succeeded on a fault the engine could not inject")
	}
	if uid != "" {
		t.Errorf("uid = %q, want empty", uid)
	}
	ee := asExecutorError(t, err)
	if ee.Reason != simian.ReasonInjectionFailed {
		t.Errorf("Reason = %q, want %q", ee.Reason, simian.ReasonInjectionFailed)
	}
	if prober.callCount() != 0 {
		t.Errorf("settle probes ran %d time(s) after the engine had already said no", prober.callCount())
	}

	if got := len(d.Cleared); got != 1 {
		t.Fatalf("driver.Cleared = %d, want 1", got)
	}
	if len(registry.List("")) != 0 {
		t.Error("lease still registered after a successful rollback")
	}
	if got := len(exec.Recent("", 10)); got != 0 {
		t.Errorf("history entries = %d, want 0", got)
	}

	injected, ok := auditor.FindEvent(audit.EventFaultInjected)
	if !ok {
		t.Fatal("no fault.injected event on failure")
	}
	if injected.Payload["passed"] != false || !strings.Contains(injected.Payload["observed"].(string), "resolv.conf.chaos.bak") {
		t.Errorf("fault.injected payload = %v, want passed=false carrying the engine's message", injected.Payload)
	}
	cleared, ok := auditor.FindEvent(audit.EventLeaseCleared)
	if !ok {
		t.Fatal("no lease.cleared event")
	}
	if cleared.Reason != string(simian.ReasonInjectionFailed) {
		t.Errorf("lease.cleared reason = %q, want %q", cleared.Reason, simian.ReasonInjectionFailed)
	}
}

// verified_by lists everything that will check the fault, in the order it
// runs, and is empty — not missing — when nothing will. The trial's 840
// unchecked faults were indistinguishable from checked ones in the log.
func TestVerifiedByNamesEveryCheckAndAdmitsToNone(t *testing.T) {
	t.Run("engine then probes", func(t *testing.T) {
		d := &confirmingDriver{observed: "AllInjected=True"}
		exec, auditor, _ := newConfirmingExecutor(t, d, &fakeProber{})
		m := goodManifest()
		m.Probes = []simian.ProbeSpec{settleProbe("slow")}
		if _, err := exec.Apply(context.Background(), m); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		applied, _ := auditor.FindEvent(audit.EventDriverApplied)
		if got, _ := applied.Payload["verified_by"].([]string); !slices.Equal(got, []string{"engine-status", "slow"}) {
			t.Errorf("verified_by = %v", applied.Payload["verified_by"])
		}
	})
	t.Run("nothing", func(t *testing.T) {
		exec, _, auditor := newTestExecutor(t, DefaultConfig(), map[string]bool{"online-boutique": true}, nil)
		if _, err := exec.Apply(context.Background(), goodManifest()); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		applied, _ := auditor.FindEvent(audit.EventDriverApplied)
		got, ok := applied.Payload["verified_by"].([]string)
		if !ok || got == nil || len(got) != 0 {
			t.Errorf("verified_by = %#v, want an empty, non-nil list", applied.Payload["verified_by"])
		}
		if _, ok := auditor.FindEvent(audit.EventFaultInjected); ok {
			t.Error("fault.injected emitted by a driver that cannot confirm")
		}
	})
}

// Killing the controller mid-wait on the 2026-09-30 trial recorded the fault
// as "injection-failed ... not injected after 30s" nineteen seconds in, and
// then failed to delete it, because the rollback ran on the context the
// shutdown had just cancelled (#158). A shutdown is not a verdict on the
// fault, and the rollback is the one thing that has to outlive it.
func TestAShutdownMidConfirmationIsRecordedAsInterruptedAndStillRolledBack(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &confirmingDriver{
		observed: "AllInjected=False",
		err:      errors.New("chaos-mesh: bank/simian-w9xdk: stopped waiting after 19s (context canceled)"),
		during:   cancel,
	}
	exec, auditor, registry := newConfirmingExecutor(t, d, nil)
	var clearCtxErr error
	d.ClearFn = func(c context.Context, _ string) error {
		clearCtxErr = c.Err()
		return clearCtxErr
	}

	_, err := exec.Apply(ctx, goodManifest())
	if ee := asExecutorError(t, err); ee.Reason != simian.ReasonInterrupted {
		t.Errorf("Reason = %q, want %q", ee.Reason, simian.ReasonInterrupted)
	}

	injected, ok := auditor.FindEvent(audit.EventFaultInjected)
	if !ok {
		t.Fatal("no fault.injected event")
	}
	if injected.Reason != string(simian.ReasonInterrupted) {
		t.Errorf("fault.injected reason = %q, want %q", injected.Reason, simian.ReasonInterrupted)
	}
	if _, has := injected.Payload["passed"]; has {
		t.Errorf("fault.injected carries passed=%v; the engine never answered", injected.Payload["passed"])
	}

	if len(d.Cleared) != 1 {
		t.Fatalf("driver.Cleared = %d, want 1", len(d.Cleared))
	}
	if clearCtxErr != nil {
		t.Errorf("rollback ran on a context that was already done (%v); the delete would fail and the fault outlive the process", clearCtxErr)
	}
	if len(registry.List("")) != 0 {
		t.Error("lease still registered after a successful rollback")
	}
	cleared, _ := auditor.FindEvent(audit.EventLeaseCleared)
	if cleared.Reason != string(simian.ReasonInterrupted) || cleared.Payload["left_to_reaper"] != nil {
		t.Errorf("lease.cleared = %q %v, want interrupted and actually cleared", cleared.Reason, cleared.Payload)
	}
}
