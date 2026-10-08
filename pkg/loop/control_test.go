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

package loop

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/planner"
	"github.com/go-steer/simian-agent/pkg/simian"
)

// cycleLog counts what the loop did, by namespace.
type cycleLog struct {
	mu      sync.Mutex
	started []string
	skipped map[string]string // namespace → last skip reason
}

func (c *cycleLog) Emit(_ context.Context, e simian.AuditEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ns, _ := e.Payload["namespace"].(string)
	switch e.Event {
	case audit.EventCycleStarted:
		c.started = append(c.started, ns)
	case audit.EventCycleSkipped:
		if c.skipped == nil {
			c.skipped = map[string]string{}
		}
		c.skipped[ns] = e.Reason
	}
}

func (c *cycleLog) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.started)
}

func (c *cycleLog) reason(ns string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.skipped[ns]
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Autonomous mode can be turned on, paused and turned off while the
// controller runs, without restarting it.
func TestAutonomousModeIsControlledWhileItRuns(t *testing.T) {
	l, _ := newLoopUnderTest(t, planJSON(1), &recordingExecutor{}, planner.Budget{
		MaxFaultsPerCycle: 1, MaxConcurrentFaults: 5, MaxSeverityPerCycle: simian.TierNamespace,
	})
	log := &cycleLog{}
	l.Auditor = log
	off := Settings{Namespaces: []string{"boutique"}, Interval: 50 * time.Millisecond, MaxFaultsPerCycle: 1}
	l.Control = NewControl(off)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()

	time.Sleep(150 * time.Millisecond)
	if n := log.count(); n != 0 {
		t.Fatalf("off: %d cycles ran", n)
	}

	on := off
	on.Enabled = true
	l.Control.Set(on, Source{By: "alice@example.com", At: time.Now()})
	eventually(t, "a cycle after turning it on", func() bool { return log.count() > 0 })

	paused := on
	paused.Paused = []string{"boutique"}
	l.Control.Set(paused, Source{By: "alice@example.com"})
	eventually(t, "a paused skip", func() bool { return log.reason("boutique") == ReasonPaused })

	l.Control.Set(off, Source{By: "alice@example.com"})
	time.Sleep(100 * time.Millisecond) // let a cycle already under way finish
	n := log.count()
	time.Sleep(200 * time.Millisecond)
	if log.count() != n {
		t.Errorf("cycles kept running after autonomous mode was turned off")
	}
	cancel()
	<-done
}

func TestSettingsMustFitTheExecutorsLimits(t *testing.T) {
	lim := Limits{MaxConcurrentFaults: 1, PermittedTiers: []simian.BlastRadiusTier{simian.TierNamespace}, Arenas: []string{"boutique", "bank"}}
	good := Settings{Enabled: true, Namespaces: []string{"boutique"}, Interval: 3 * time.Minute, MaxFaultsPerCycle: 1, MaxSeverityPerCycle: simian.TierNamespace}
	if err := good.Validate(lim); err != nil {
		t.Fatalf("valid settings refused: %v", err)
	}
	for name, edit := range map[string]func(*Settings){
		"a namespace that is not an arena":       func(s *Settings) { s.Namespaces = []string{"kube-system"} },
		"on with no namespace":                   func(s *Settings) { s.Namespaces = nil },
		"an interval under a minute":             func(s *Settings) { s.Interval = 10 * time.Second },
		"more faults per cycle than run at once": func(s *Settings) { s.MaxFaultsPerCycle = 2 },
		"zero faults per cycle":                  func(s *Settings) { s.MaxFaultsPerCycle = 0 },
		"a tier the install does not permit":     func(s *Settings) { s.MaxSeverityPerCycle = simian.TierNode },
	} {
		s := good
		edit(&s)
		if err := s.Validate(lim); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSettingsRoundTripWithAReadableInterval(t *testing.T) {
	s := Settings{Enabled: true, Namespaces: []string{"boutique"}, Interval: 3 * time.Minute, MaxFaultsPerCycle: 1, Paused: []string{PauseAll}}
	b, err := s.MarshalJSON()
	if err != nil || !strings.Contains(string(b), `"interval":"3m0s"`) {
		t.Fatalf("marshal: %s, %v", b, err)
	}
	var back Settings
	if err := back.UnmarshalJSON(b); err != nil || back.Interval != 3*time.Minute || !back.IsPaused("anything") {
		t.Errorf("round trip: %+v, %v", back, err)
	}
}

// Pausing must not be refused over a value the install itself chose, such
// as the chart's three faults per cycle under one concurrent fault.
func TestTheInstallsOwnValuesAreAcceptedAsTheyAre(t *testing.T) {
	inst := Settings{Namespaces: []string{"boutique"}, Interval: 30 * time.Second, MaxFaultsPerCycle: 3, MaxSeverityPerCycle: simian.TierNode}
	lim := Limits{MaxConcurrentFaults: 1, PermittedTiers: []simian.BlastRadiusTier{simian.TierNamespace}, Arenas: []string{"boutique"}, Install: &inst}
	paused := inst
	paused.Paused = []string{"boutique"}
	if err := paused.Validate(lim); err != nil {
		t.Errorf("pausing with the install's own values: %v", err)
	}
	raised := inst
	raised.MaxFaultsPerCycle = 2
	if err := raised.Validate(lim); err == nil {
		t.Error("a changed value over the limit was accepted")
	}
}
