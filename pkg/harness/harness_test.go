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

package harness

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/eval"
	"github.com/go-steer/simian-agent/pkg/scenario"
	"github.com/go-steer/simian-agent/pkg/simian"
)

// fakeArena records what was set up and torn down, in order.
type fakeArena struct {
	mu       sync.Mutex
	setup    []string
	torndown []string
	fail     map[string]error

	// live counts namespaces currently provisioned, and peak is the most that
	// were ever provisioned at once. Together they are how the fencing tests
	// observe overlap without sleeping.
	live map[string]int
	peak map[string]int
}

func newFakeArena() *fakeArena {
	return &fakeArena{fail: map[string]error{}, live: map[string]int{}, peak: map[string]int{}}
}

func (a *fakeArena) Setup(_ context.Context, ns string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.fail[ns]; err != nil {
		return err
	}
	a.setup = append(a.setup, ns)
	a.live[ns]++
	if a.live[ns] > a.peak[ns] {
		a.peak[ns] = a.live[ns]
	}
	return nil
}

func (a *fakeArena) Teardown(_ context.Context, ns string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.torndown = append(a.torndown, ns)
	a.live[ns]--
	return nil
}

func (a *fakeArena) tornDown() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.torndown...)
}

func (a *fakeArena) peakFor(ns string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.peak[ns]
}

// fakeInjector stands in for the executor. It hands out predictable UIDs and
// keeps a per-namespace active set, so ListActive answers the way the real one
// would.
type fakeInjector struct {
	mu      sync.Mutex
	n       int
	applied []simian.FaultManifest
	cleared []string
	active  map[string]simian.ActiveFault // uid -> fault

	applyErr    error
	failAfter   int // apply this many, then fail. 0 means never.
	clearErr    error
	listErr     error
	onApply     func(m simian.FaultManifest)
	clearIsNoop bool
}

func newFakeInjector() *fakeInjector {
	return &fakeInjector{active: map[string]simian.ActiveFault{}}
}

func (f *fakeInjector) Apply(_ context.Context, m simian.FaultManifest) (string, error) {
	f.mu.Lock()
	if f.applyErr != nil && (f.failAfter == 0 || f.n >= f.failAfter) {
		f.mu.Unlock()
		return "", f.applyErr
	}
	f.n++
	uid := fmt.Sprintf("f-%03d", f.n)
	f.applied = append(f.applied, m)
	f.active[uid] = simian.ActiveFault{FaultUID: uid, Manifest: m}
	onApply := f.onApply
	f.mu.Unlock()

	if onApply != nil {
		onApply(m)
	}
	return uid, nil
}

func (f *fakeInjector) Clear(_ context.Context, uid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleared = append(f.cleared, uid)
	if f.clearErr != nil {
		return f.clearErr
	}
	if !f.clearIsNoop {
		delete(f.active, uid)
	}
	return nil
}

func (f *fakeInjector) ListActive(_ context.Context, ns string) ([]simian.ActiveFault, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []simian.ActiveFault
	for _, af := range f.active {
		for _, t := range af.Manifest.Targets {
			if t.Namespace == ns {
				out = append(out, af)
				break
			}
		}
	}
	return out, nil
}

// remove drops a fault from the active set without going through Clear, the
// way an outside remediator would.
func (f *fakeInjector) remove(uid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.active, uid)
}

func (f *fakeInjector) clearedUIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cleared...)
}

func (f *fakeInjector) appliedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.applied)
}

// fakeSubject answers with whatever it was told to answer.
type fakeSubject struct {
	name string
	fn   func(ctx context.Context, prompt string) (eval.Report, error)

	mu      sync.Mutex
	prompts []string
}

func (s *fakeSubject) Name() string {
	if s.name == "" {
		return "fake"
	}
	return s.name
}

func (s *fakeSubject) Investigate(ctx context.Context, prompt string) (eval.Report, error) {
	s.mu.Lock()
	s.prompts = append(s.prompts, prompt)
	s.mu.Unlock()
	if s.fn == nil {
		return eval.Report{Findings: []scenario.Finding{}}, nil
	}
	return s.fn(ctx, prompt)
}

func (s *fakeSubject) asked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.prompts...)
}

// recordingAuditor keeps every event, so a test can assert the harness left a
// trace of a scenario it attempted.
type recordingAuditor struct {
	mu     sync.Mutex
	events []simian.AuditEvent
}

func (a *recordingAuditor) Emit(ctx context.Context, ev simian.AuditEvent) {
	if ev.ScenarioID == "" {
		ev.ScenarioID = audit.ScenarioIDFrom(ctx)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, ev)
}

func (a *recordingAuditor) named(name string) []simian.AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []simian.AuditEvent
	for _, ev := range a.events {
		if ev.Event == name {
			out = append(out, ev)
		}
	}
	return out
}

// scenarioIn builds a scenario with one fault targeting ns.
func scenarioIn(id, ns string, faults int) scenario.Scenario {
	s := scenario.Scenario{
		ID:     id,
		Name:   id,
		Prompt: "what is wrong in " + ns + "?",
		Expect: []scenario.ExpectedFinding{{Kind: "Pod", Name: "api", Root: true}},
	}
	for i := range faults {
		s.Faults = append(s.Faults, simian.FaultManifest{
			Engine:       simian.EngineKubeState,
			ResourceKind: "ImageUnresolvable",
			Targets:      []simian.TargetRef{{Namespace: ns, Name: fmt.Sprintf("w-%d", i)}},
			Duration:     5 * time.Minute,
		})
	}
	return s
}

// controlScenario has no faults and no expectations: the cluster is healthy
// and the right answer is to say so.
func controlScenario(id string) scenario.Scenario {
	return scenario.Scenario{ID: id, Name: id, Prompt: "anything wrong?"}
}

func packOf(ss ...scenario.Scenario) scenario.Pack {
	return scenario.Pack{Name: "test", Scenarios: ss}
}

var errBoom = errors.New("boom")
