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
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/go-steer/simian-agent/pkg/eval"
	"github.com/go-steer/simian-agent/pkg/scenario"
)

// fakeSubstrates records deploys and destroys as "verb ns/name" strings, so a
// test can assert the order the run did things in rather than only the counts.
type fakeSubstrates struct {
	mu         sync.Mutex
	known      []string
	calls      []string
	deployErr  error
	destroyErr error
}

func newFakeSubstrates(known ...string) *fakeSubstrates {
	return &fakeSubstrates{known: known}
}

func (f *fakeSubstrates) Known(name string) bool { return slices.Contains(f.known, name) }

func (f *fakeSubstrates) Deploy(_ context.Context, ns, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "deploy "+ns+"/"+name)
	return f.deployErr
}

func (f *fakeSubstrates) Destroy(_ context.Context, ns, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "destroy "+ns+"/"+name)
	return f.destroyErr
}

func (f *fakeSubstrates) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// withSubstrate returns s with a substrate attached.
func withSubstrate(s scenario.Scenario, name string) scenario.Scenario {
	s.Substrate = name
	return s
}

// The substrate is up before the fault, because the fault is aimed at it. A
// NetworkChaos whose selector matches nothing applies, gates and clears
// without having perturbed a single packet.
func TestTheSubstrateIsUpBeforeTheFaultAndGoneAfterTheArena(t *testing.T) {
	subs := newFakeSubstrates("pair")
	arena := newFakeArena()
	inj := newFakeInjector()

	r := &Runner{
		Pack:       packOf(withSubstrate(scenarioIn("s-1", "ns-a", 1), "pair")),
		Subject:    &fakeSubject{},
		Arena:      arena,
		Substrates: subs,
		Injector:   inj,
	}
	if _, err := r.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := []string{"deploy ns-a/pair", "destroy ns-a/pair"}
	if got := subs.recorded(); !slices.Equal(got, want) {
		t.Fatalf("substrate calls = %v, want %v", got, want)
	}
	if got := inj.appliedCount(); got != 1 {
		t.Fatalf("applied %d faults, want 1", got)
	}
	// Deployed after the arena existed and destroyed before it stopped
	// existing: a SUT applied into a namespace that is not there yet fails,
	// and one destroyed after the namespace is gone is a NotFound nobody
	// needed to see.
	if torn := arena.tornDown(); !slices.Equal(torn, []string{"ns-a"}) {
		t.Fatalf("tore down %v, want [ns-a]", torn)
	}
}

// A pack with no substrates never asks for one. The registry is optional
// because most scenarios synthesize their own subject matter.
func TestAScenarioWithNoSubstrateNeverAsksForOne(t *testing.T) {
	subs := newFakeSubstrates("pair")
	r := &Runner{
		Pack:       packOf(scenarioIn("s-1", "ns-a", 1)),
		Subject:    &fakeSubject{},
		Arena:      newFakeArena(),
		Substrates: subs,
		Injector:   newFakeInjector(),
	}
	if _, err := r.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := subs.recorded(); len(got) != 0 {
		t.Errorf("substrate calls = %v, want none", got)
	}
}

func TestARunnerWithNoSubstrateRegistryStillRunsAPackThatNeedsNone(t *testing.T) {
	r := &Runner{
		Pack:     packOf(scenarioIn("s-1", "ns-a", 1)),
		Subject:  &fakeSubject{},
		Arena:    newFakeArena(),
		Injector: newFakeInjector(),
	}
	if _, err := r.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// An unregistered substrate is refused before the first namespace, not on the
// scenario that needs it. Discovering a typo four arenas in means four
// scenarios' worth of cluster time spent on a scorecard that will not be
// produced.
func TestAnUnknownSubstrateIsRefusedBeforeTheClusterIsTouched(t *testing.T) {
	cases := []struct {
		name  string
		subs  Substrates
		match string
	}{
		{"not registered", newFakeSubstrates("pair"), `names substrate "nope", which is not registered`},
		{"no registry at all", nil, `has no substrate registry`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			arena := newFakeArena()
			inj := newFakeInjector()
			r := &Runner{
				Pack: packOf(
					scenarioIn("s-1", "ns-a", 1),
					withSubstrate(scenarioIn("s-2", "ns-b", 1), "nope"),
				),
				Subject:    &fakeSubject{},
				Arena:      arena,
				Substrates: tc.subs,
				Injector:   inj,
			}
			_, err := r.Run(t.Context())
			if err == nil {
				t.Fatal("Run succeeded with an unregistered substrate")
			}
			if !strings.Contains(err.Error(), tc.match) {
				t.Errorf("error = %q, want it to contain %q", err, tc.match)
			}
			if got := len(arena.tornDown()); got != 0 {
				t.Errorf("tore down %d arenas; the run should not have started", got)
			}
			if got := inj.appliedCount(); got != 0 {
				t.Errorf("applied %d faults; the run should not have started", got)
			}
		})
	}
}

// A substrate that will not come up is the harness's failure, exactly like a
// failed injection: the subject is not asked, and no fault is applied at
// something that is not there.
func TestASubstrateThatFailsToDeployIsAnInjectError(t *testing.T) {
	subs := newFakeSubstrates("pair")
	subs.deployErr = errBoom
	inj := newFakeInjector()
	subj := &fakeSubject{}

	r := &Runner{
		Pack:       packOf(withSubstrate(scenarioIn("s-1", "ns-a", 1), "pair")),
		Subject:    subj,
		Arena:      newFakeArena(),
		Substrates: subs,
		Injector:   inj,
	}
	runs, err := r.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	run := runs[0]
	if run.InjectError == "" {
		t.Fatal("InjectError is empty; the substrate never came up")
	}
	if !strings.Contains(run.InjectError, "substrate pair in ns-a") {
		t.Errorf("InjectError = %q, want it to name the substrate and the namespace", run.InjectError)
	}
	if run.SubjectError != "" {
		t.Errorf("SubjectError = %q; this is the harness's failure, not the subject's", run.SubjectError)
	}
	if got := subj.asked(); len(got) != 0 {
		t.Errorf("the subject was asked %v about a cluster the substrate never reached", got)
	}
	if got := inj.appliedCount(); got != 0 {
		t.Errorf("applied %d faults at a substrate that is not there", got)
	}
	// Nothing was deployed, so nothing is destroyed — but the arena still is.
	if got := subs.recorded(); !slices.Equal(got, []string{"deploy ns-a/pair"}) {
		t.Errorf("substrate calls = %v, want just the failed deploy", got)
	}
}

// A destroy that fails is logged and the run carries on. The arena teardown
// behind it deletes the namespace anyway, and failing the scenario over
// cleanup would throw away a report that cost cluster time to produce.
func TestAFailedSubstrateTeardownDoesNotFailTheScenario(t *testing.T) {
	subs := newFakeSubstrates("pair")
	subs.destroyErr = errBoom
	arena := newFakeArena()

	r := &Runner{
		Pack:       packOf(withSubstrate(scenarioIn("s-1", "ns-a", 1), "pair")),
		Subject:    &fakeSubject{fn: func(context.Context, string) (eval.Report, error) { return eval.Report{}, nil }},
		Arena:      arena,
		Substrates: subs,
		Injector:   newFakeInjector(),
	}
	runs, err := r.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runs[0].InjectError != "" || runs[0].SubjectError != "" {
		t.Errorf("run failed over cleanup: inject=%q subject=%q", runs[0].InjectError, runs[0].SubjectError)
	}
	if torn := arena.tornDown(); !slices.Equal(torn, []string{"ns-a"}) {
		t.Errorf("tore down %v, want [ns-a] — the arena goes even if the substrate would not", torn)
	}
}

func TestSUTSubstratesWithoutAManagerSaysSoRatherThanPanicking(t *testing.T) {
	var s *SUTSubstrates
	if s.Known("anything") {
		t.Error("a nil registry knows a substrate")
	}
	if err := s.Deploy(t.Context(), "ns", "pair"); err == nil {
		t.Error("Deploy on a nil SUTSubstrates returned no error")
	}
	if err := s.Destroy(t.Context(), "ns", "pair"); err == nil {
		t.Error("Destroy on a nil SUTSubstrates returned no error")
	}
}
