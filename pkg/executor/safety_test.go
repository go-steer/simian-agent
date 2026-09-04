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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/simian-agent/internal/testutil"
	"github.com/go-steer/simian-agent/pkg/lease"
	"github.com/go-steer/simian-agent/pkg/probe"
	"github.com/go-steer/simian-agent/pkg/simian"
)

// newBudgetExecutor builds an executor over a caller-supplied driver, so a test
// can hold an apply open inside the driver and observe what a second one does
// while the first has no lease yet.
func newBudgetExecutor(t *testing.T, cfg Config, driver simian.ChaosDriver, opts ...Option) (*Executor, *lease.Registry) {
	t.Helper()
	registry := lease.NewRegistry("test-holder")
	elig := &StaticEligibility{Eligible: map[string]bool{"online-boutique": true, "boutique-2": true}}
	exec := New(cfg, map[simian.Engine]simian.ChaosDriver{simian.EngineChaosMesh: driver},
		registry, &testutil.FakeAuditor{}, elig, opts...)
	return exec, registry
}

// blockingProber holds the settle gate open until the test closes release, so
// the window where a fault owns a lease and is still inside Apply is a window
// the test controls.
type blockingProber struct {
	entered chan struct{}
	release chan struct{}
}

func (p *blockingProber) Run(_ context.Context, s simian.ProbeSpec, _ probe.Target) probe.Result {
	p.entered <- struct{}{}
	<-p.release
	return probe.Result{Name: s.Name, Type: s.Type, Passed: true}
}

func TestParsePermittedTiers(t *testing.T) {
	tests := []struct {
		name    string
		in      []string
		want    map[simian.BlastRadiusTier]bool
		wantErr string
	}{
		{
			// Not "permit nothing". An unset flag must leave the built-in
			// policy alone rather than silently disarm the controller.
			name: "empty means leave the default alone",
			in:   nil,
			want: nil,
		},
		{
			name: "the narrowing case operators actually reach for",
			in:   []string{"namespace"},
			want: map[simian.BlastRadiusTier]bool{simian.TierNamespace: true},
		},
		{
			name: "surrounding whitespace survives a comma-separated flag",
			in:   []string{"namespace", " node "},
			want: map[simian.BlastRadiusTier]bool{simian.TierNamespace: true, simian.TierNode: true},
		},
		{
			// The whole point of erroring: dropping the typo would leave
			// {namespace} where the operator wrote {namespace, node}, which
			// is narrower and therefore invisible until something is refused.
			// The dangerous direction is a typo'd sole entry.
			name:    "a typo is an error, not a silent drop",
			in:      []string{"namesapce"},
			wantErr: "unknown blast-radius tier",
		},
		{
			name:    "the empty string is not a tier",
			in:      []string{""},
			wantErr: "unknown blast-radius tier",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParsePermittedTiers(tt.in)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("ParsePermittedTiers(%q) = %v, want error", tt.in, got)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePermittedTiers(%q): %v", tt.in, err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for k := range tt.want {
				if !got[k] {
					t.Errorf("tier %q missing from %v", k, got)
				}
			}
		})
	}
}

func TestANodeTierFaultIsRejectedWhenOnlyNamespaceIsPermitted(t *testing.T) {
	// The setting exists so an operator can keep node-level chaos off a
	// cluster they care about. Until it was wired to a flag it was inert,
	// and this is the assertion that says otherwise.
	cfg := DefaultConfig()
	tiers, err := ParsePermittedTiers([]string{"namespace"})
	if err != nil {
		t.Fatalf("ParsePermittedTiers: %v", err)
	}
	cfg.PermittedTiers = tiers

	driver := &testutil.FakeDriver{EngineName: simian.EngineChaosMesh}
	exec, registry := newBudgetExecutor(t, cfg, driver)

	m := goodManifest()
	m.ResourceKind = "KernelChaos" // classified node-tier by pkg/catalog
	m.Spec = map[string]any{"mode": "one"}

	_, err = exec.Apply(context.Background(), m)
	if err == nil {
		t.Fatal("a node-tier fault was applied under a namespace-only policy")
	}
	ee := asExecutorError(t, err)
	if ee.Stage != simian.StageSafety || ee.Reason != simian.ReasonTierNotPermitted {
		t.Fatalf("stage/reason = %s/%s, want %s/%s",
			ee.Stage, ee.Reason, simian.StageSafety, simian.ReasonTierNotPermitted)
	}
	if len(driver.Applied) != 0 {
		t.Errorf("driver applied %d fault(s); the tier gate is meant to run first", len(driver.Applied))
	}
	if len(registry.List("")) != 0 {
		t.Error("a rejected fault took a lease")
	}
}

func TestTheSameFaultIsAllowedWhenNodeTierIsPermitted(t *testing.T) {
	// The other half of the pair. Without it, the test above passes just as
	// well against a policy that rejects everything.
	driver := &testutil.FakeDriver{EngineName: simian.EngineChaosMesh}
	exec, _ := newBudgetExecutor(t, DefaultConfig(), driver)

	m := goodManifest()
	m.ResourceKind = "KernelChaos"
	m.Spec = map[string]any{"mode": "one"}

	if _, err := exec.Apply(context.Background(), m); err != nil {
		t.Fatalf("Apply under the default namespace+node policy: %v", err)
	}
	if len(driver.Applied) != 1 {
		t.Fatalf("driver applied %d fault(s), want 1", len(driver.Applied))
	}
	if got := driver.Applied[0].BlastRadiusTier; got != simian.TierNode {
		t.Fatalf("tier = %q, want %q — the test is not exercising what it claims", got, simian.TierNode)
	}
}

// holdingDriver blocks inside Apply until the test releases it, so a second
// Apply can be observed during the window where the first has passed the
// budget check but has not yet registered a lease.
//
// Only the first `hold` calls block. Calls past that return immediately, which
// is what makes these tests report rather than hang: if a budget check regresses
// and lets a fault through that should have been refused, the assertion fires
// instead of the extra Apply parking on a channel nobody will close.
type holdingDriver struct {
	testutil.FakeDriver
	entered chan struct{}
	proceed chan struct{}
	err     error

	mu    sync.Mutex
	hold  int
	calls int
}

func newHoldingDriver(hold int) *holdingDriver {
	return &holdingDriver{
		FakeDriver: testutil.FakeDriver{EngineName: simian.EngineChaosMesh},
		entered:    make(chan struct{}, hold),
		proceed:    make(chan struct{}),
		hold:       hold,
	}
}

func (d *holdingDriver) Apply(ctx context.Context, m simian.FaultManifest) (string, error) {
	d.mu.Lock()
	d.calls++
	blocking := d.calls <= d.hold
	d.mu.Unlock()
	if blocking {
		d.entered <- struct{}{}
		<-d.proceed
	}
	if d.err != nil {
		return "", d.err
	}
	return d.FakeDriver.Apply(ctx, m)
}

func TestTheConcurrencyCapHoldsWhileAnEarlierApplyIsStillInFlight(t *testing.T) {
	// The cap used to be read off the lease registry, but the lease is not
	// registered until the driver returns -- and between the two sit the
	// default-probe attach, the driver lookup and the SOT probes, which alone
	// may poll for 90 seconds. pkg/loop fans applies out across exactly that
	// window, so two callers both saw the same free slot and both took it.
	cfg := DefaultConfig()
	cfg.MaxConcurrentFaults = 1

	driver := newHoldingDriver(1)
	exec, registry := newBudgetExecutor(t, cfg, driver)

	first := make(chan error, 1)
	go func() { _, err := exec.Apply(context.Background(), goodManifest()); first <- err }()

	<-driver.entered // the first apply is inside the driver, holding no lease
	if len(registry.List("")) != 0 {
		t.Fatal("precondition failed: a lease exists, so this is not the racy window")
	}

	second := goodManifest()
	second.Targets[0].Namespace = "boutique-2" // a different namespace: only the global cap should bite
	_, err := exec.Apply(context.Background(), second)
	if err == nil {
		t.Fatal("a second fault was admitted while the first was mid-apply, over a cap of 1")
	}
	ee := asExecutorError(t, err)
	if ee.Reason != simian.ReasonBudgetExceeded {
		t.Fatalf("reason = %q, want %q", ee.Reason, simian.ReasonBudgetExceeded)
	}

	close(driver.proceed)
	if err := <-first; err != nil {
		t.Fatalf("first Apply: %v", err)
	}
}

func TestAFailedApplyGivesItsSlotBack(t *testing.T) {
	// The reservation is the whole fix, so leaking one is the whole way to
	// break it: a controller under a cap of 1 whose first fault failed would
	// refuse every fault afterwards, and look exactly like a working cap.
	cfg := DefaultConfig()
	cfg.MaxConcurrentFaults = 1

	// hold 0: nothing needs to block here, only to fail and then succeed.
	driver := newHoldingDriver(0)
	driver.err = errors.New("cluster said no")
	exec, _ := newBudgetExecutor(t, cfg, driver)

	if _, err := exec.Apply(context.Background(), goodManifest()); err == nil {
		t.Fatal("expected the driver failure to surface")
	}

	driver.err = nil
	if _, err := exec.Apply(context.Background(), goodManifest()); err != nil {
		t.Fatalf("Apply after a failed one: %v — the reservation leaked", err)
	}
}

func TestAFaultWaitingOnItsSettleGateIsCountedOnceNotTwice(t *testing.T) {
	// Once the lease is registered the registry counts the fault, so Apply
	// hands the reservation back there rather than at return. Holding both
	// would halve the effective cap for the length of the gate — up to the
	// probe's full 90s budget — and reject faults there was room for.
	cfg := DefaultConfig()
	cfg.MaxConcurrentFaults = 2

	prober := &blockingProber{entered: make(chan struct{}, 1), release: make(chan struct{})}
	driver := newHoldingDriver(0)
	exec, registry := newBudgetExecutor(t, cfg, driver, WithProber(prober))

	gated := goodManifest()
	gated.Probes = []simian.ProbeSpec{settleProbe("settling")}
	first := make(chan error, 1)
	go func() { _, err := exec.Apply(context.Background(), gated); first <- err }()

	<-prober.entered
	if len(registry.List("")) != 1 {
		t.Fatalf("leases=%d, want 1 — the fault should hold its lease while settling", len(registry.List("")))
	}

	if _, err := exec.Apply(context.Background(), goodManifest()); err != nil {
		t.Fatalf("second Apply under a cap of 2 with one fault settling: %v", err)
	}

	close(prober.release)
	if err := <-first; err != nil {
		t.Fatalf("first Apply: %v", err)
	}
}

func TestCooldownCountsAnInFlightApplyAsConsecutive(t *testing.T) {
	// Two faults that overlap in the same namespace are as close together as
	// two faults get. Checking only lastApplyByNS -- stamped after the driver
	// returns -- let both of them through.
	cfg := DefaultConfig()
	cfg.MinCooldown = time.Hour

	driver := newHoldingDriver(1)
	exec, _ := newBudgetExecutor(t, cfg, driver)

	first := make(chan error, 1)
	go func() { _, err := exec.Apply(context.Background(), goodManifest()); first <- err }()
	<-driver.entered

	_, err := exec.Apply(context.Background(), goodManifest())
	if err == nil {
		t.Fatal("a second fault hit the namespace while the first was still being applied")
	}
	if ee := asExecutorError(t, err); ee.Reason != simian.ReasonBudgetExceeded {
		t.Fatalf("reason = %q, want %q", ee.Reason, simian.ReasonBudgetExceeded)
	}

	close(driver.proceed)
	if err := <-first; err != nil {
		t.Fatalf("first Apply: %v", err)
	}
}

func TestAnInFlightApplyDoesNotBlockAnotherNamespace(t *testing.T) {
	// The cooldown is per-namespace, and the in-flight check has to stay that
	// way: serialising the whole controller would be a much bigger behaviour
	// change than the bug being fixed.
	cfg := DefaultConfig()
	cfg.MinCooldown = time.Hour

	driver := newHoldingDriver(2)
	exec, _ := newBudgetExecutor(t, cfg, driver)

	first := make(chan error, 1)
	go func() { _, err := exec.Apply(context.Background(), goodManifest()); first <- err }()
	<-driver.entered

	other := goodManifest()
	other.Targets[0].Namespace = "boutique-2"
	second := make(chan error, 1)
	go func() { _, err := exec.Apply(context.Background(), other); second <- err }()

	<-driver.entered // it reached the driver, so the cooldown did not reject it
	close(driver.proceed)
	if err := <-first; err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("second Apply in a different namespace: %v", err)
	}
}
