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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/eval"
	"github.com/go-steer/simian-agent/pkg/scenario"
	"github.com/go-steer/simian-agent/pkg/simian"
)

func TestARunProducesOneResultPerScenario(t *testing.T) {
	arena := newFakeArena()
	inj := newFakeInjector()
	subj := &fakeSubject{name: "agent", fn: func(context.Context, string) (eval.Report, error) {
		return eval.Report{Findings: []scenario.Finding{{Kind: "Pod", ResourceName: "api"}}}, nil
	}}

	r := &Runner{
		Pack:     packOf(scenarioIn("s-1", "ns-a", 1), scenarioIn("s-2", "ns-b", 2)),
		Subject:  subj,
		Arena:    arena,
		Injector: inj,
	}
	runs, err := r.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(runs))
	}
	for _, run := range runs {
		if !run.Manifested {
			t.Errorf("%s: not manifested", run.ScenarioID)
		}
		if run.InjectError != "" || run.SubjectError != "" {
			t.Errorf("%s: inject=%q subject=%q, want neither", run.ScenarioID, run.InjectError, run.SubjectError)
		}
		if run.Report == nil {
			t.Errorf("%s: no report", run.ScenarioID)
		}
		if run.Subject != "agent" {
			t.Errorf("%s: Subject = %q, want agent", run.ScenarioID, run.Subject)
		}
		if run.InjectedAt.IsZero() || run.DetectedAt.IsZero() {
			t.Errorf("%s: timestamps not stamped: %+v", run.ScenarioID, run)
		}
	}
	if got := inj.appliedCount(); got != 3 {
		t.Errorf("applied %d faults, want 3", got)
	}
}

// Results come back in pack order however the scenarios finished. A scorecard
// whose rows shuffle with concurrency and scheduling luck is a scorecard
// nobody can diff against yesterday's.
func TestResultsAreInPackOrderNotCompletionOrder(t *testing.T) {
	var started atomic.Int32
	subj := &fakeSubject{fn: func(_ context.Context, prompt string) (eval.Report, error) {
		// The first scenario in the pack is deliberately the slowest.
		if strings.Contains(prompt, "ns-a") {
			time.Sleep(60 * time.Millisecond)
		}
		started.Add(1)
		return eval.Report{Findings: []scenario.Finding{}}, nil
	}}
	r := &Runner{
		Pack:        packOf(scenarioIn("s-1", "ns-a", 1), scenarioIn("s-2", "ns-b", 1), scenarioIn("s-3", "ns-c", 1)),
		Subject:     subj,
		Arena:       newFakeArena(),
		Injector:    newFakeInjector(),
		Concurrency: 3,
	}
	runs, err := r.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var ids []string
	for _, run := range runs {
		ids = append(ids, run.ScenarioID)
	}
	if want := []string{"s-1", "s-2", "s-3"}; !slices.Equal(ids, want) {
		t.Errorf("ids = %v, want %v", ids, want)
	}
}

// Injection failure is the harness's failure, and the subject is not asked.
// A report about a cluster that was never broken cannot be scored either way,
// and charging the subject for it would be charging it for our bug.
func TestAFailedInjectionDoesNotReachTheSubject(t *testing.T) {
	inj := newFakeInjector()
	inj.applyErr = errBoom
	subj := &fakeSubject{}

	r := &Runner{
		Pack:     packOf(scenarioIn("s-1", "ns-a", 1)),
		Subject:  subj,
		Arena:    newFakeArena(),
		Injector: inj,
	}
	runs, err := r.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	run := runs[0]
	if run.InjectError == "" {
		t.Fatal("InjectError is empty; the apply failed")
	}
	if run.Manifested {
		t.Error("Manifested is true after a failed apply")
	}
	if !run.InjectedAt.IsZero() {
		t.Error("InjectedAt was stamped for a fault that never landed")
	}
	if got := subj.asked(); len(got) != 0 {
		t.Errorf("the subject was asked %v about a cluster that was never broken", got)
	}
	if run.Report != nil {
		t.Error("a report exists for a scenario the subject was never asked about")
	}
}

// Half a cascade is not the incident. The second fault failing stops the
// scenario, and the first one is cleared on the way out rather than left in
// the cluster.
func TestAPartialCascadeIsAnInjectFailureAndIsCleanedUp(t *testing.T) {
	inj := newFakeInjector()
	inj.applyErr = errBoom
	inj.failAfter = 1

	r := &Runner{
		Pack:     packOf(scenarioIn("s-1", "ns-a", 3)),
		Subject:  &fakeSubject{},
		Arena:    newFakeArena(),
		Injector: inj,
	}
	runs, err := r.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runs[0].InjectError == "" {
		t.Fatal("a partial cascade was reported as a clean injection")
	}
	if want := []string{"f-001"}; !slices.Equal(inj.clearedUIDs(), want) {
		t.Errorf("cleared %v, want %v — the half that landed must not be left behind", inj.clearedUIDs(), want)
	}
}

// A subject that crashes scores zero rather than being skipped. Skipping it
// would let a subject improve its mean by crashing on the scenarios it finds
// hard.
func TestASubjectFailureIsAZeroNotASkip(t *testing.T) {
	subj := &fakeSubject{fn: func(context.Context, string) (eval.Report, error) {
		return eval.Report{}, errors.New("subject panicked")
	}}
	r := &Runner{
		Pack:     packOf(scenarioIn("s-1", "ns-a", 1)),
		Subject:  subj,
		Arena:    newFakeArena(),
		Injector: newFakeInjector(),
	}
	runs, err := r.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	run := runs[0]
	if run.SubjectError == "" {
		t.Fatal("SubjectError is empty")
	}
	if run.InjectError != "" {
		t.Errorf("InjectError = %q; the harness did its job, the subject did not", run.InjectError)
	}
	if !run.Manifested {
		t.Error("Manifested is false; the fault landed, the subject is what failed")
	}
	if run.Report != nil {
		t.Error("a report exists for a subject that failed")
	}
}

func TestEveryFaultIsClearedAndEveryArenaTornDown(t *testing.T) {
	arena := newFakeArena()
	inj := newFakeInjector()
	r := &Runner{
		Pack:     packOf(scenarioIn("s-1", "ns-a", 2), scenarioIn("s-2", "ns-b", 1)),
		Subject:  &fakeSubject{},
		Arena:    arena,
		Injector: inj,
	}
	if _, err := r.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(inj.clearedUIDs()); got != 3 {
		t.Errorf("cleared %d faults, want 3", got)
	}
	torn := arena.tornDown()
	slices.Sort(torn)
	if want := []string{"ns-a", "ns-b"}; !slices.Equal(torn, want) {
		t.Errorf("tore down %v, want %v", torn, want)
	}
}

// Cleanup runs on a context that ignores cancellation. Ctrl-C in the middle of
// a suite must still take the chaos out of the cluster — the alternative is a
// partition left in someone's namespace because they changed their mind.
func TestCleanupSurvivesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	arena := newFakeArena()
	inj := newFakeInjector()
	subj := &fakeSubject{fn: func(context.Context, string) (eval.Report, error) {
		cancel()
		return eval.Report{Findings: []scenario.Finding{}}, nil
	}}

	r := &Runner{
		Pack:     packOf(scenarioIn("s-1", "ns-a", 1)),
		Subject:  subj,
		Arena:    arena,
		Injector: inj,
	}
	if _, err := r.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := []string{"f-001"}; !slices.Equal(inj.clearedUIDs(), want) {
		t.Errorf("cleared %v after cancellation, want %v", inj.clearedUIDs(), want)
	}
	if want := []string{"ns-a"}; !slices.Equal(arena.tornDown(), want) {
		t.Errorf("tore down %v after cancellation, want %v", arena.tornDown(), want)
	}
}

// An arena that will not come up is an inject failure, and the arenas that did
// come up are still torn down. Leaving half a scenario's namespaces standing
// because the last one failed is how a cluster fills with debris.
func TestAPartialArenaSetupTearsDownWhatItMade(t *testing.T) {
	arena := newFakeArena()
	arena.fail["ns-z"] = errBoom

	s := scenario.Scenario{
		ID: "s-1", Name: "s-1", Prompt: "?",
		Expect: []scenario.ExpectedFinding{{Kind: "Pod", Name: "api", Root: true}},
		Faults: []simian.FaultManifest{{
			Engine:  simian.EngineKubeState,
			Targets: []simian.TargetRef{{Namespace: "ns-a"}, {Namespace: "ns-z"}},
		}},
	}
	inj := newFakeInjector()
	r := &Runner{Pack: packOf(s), Subject: &fakeSubject{}, Arena: arena, Injector: inj}

	runs, err := r.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runs[0].InjectError == "" {
		t.Fatal("an arena that failed to come up was not reported")
	}
	if want := []string{"ns-a"}; !slices.Equal(arena.tornDown(), want) {
		t.Errorf("tore down %v, want %v", arena.tornDown(), want)
	}
	if got := inj.appliedCount(); got != 0 {
		t.Errorf("applied %d faults into a scenario whose arena failed", got)
	}
}

func TestSelectionResolvesOnlyInPackOrder(t *testing.T) {
	r := &Runner{Pack: packOf(
		scenarioIn("s-1", "ns-a", 1),
		scenarioIn("s-2", "ns-b", 1),
		scenarioIn("s-3", "ns-c", 1),
	)}

	t.Run("empty runs the pack", func(t *testing.T) {
		got, err := r.Selection()
		if err != nil {
			t.Fatalf("Selection: %v", err)
		}
		if len(got) != 3 {
			t.Errorf("selected %d, want the whole pack", len(got))
		}
	})

	t.Run("flag order does not become row order", func(t *testing.T) {
		r.Only = []string{"s-3", "s-1"}
		got, err := r.Selection()
		if err != nil {
			t.Fatalf("Selection: %v", err)
		}
		if len(got) != 2 || got[0].ID != "s-1" || got[1].ID != "s-3" {
			t.Errorf("selected %v, want s-1 then s-3", ids(got))
		}
	})

	// A typo that silently grades nothing is how a suite comes back green
	// having measured nothing at all.
	t.Run("an unknown id is an error", func(t *testing.T) {
		r.Only = []string{"s-1", "s-nope"}
		if _, err := r.Selection(); err == nil {
			t.Fatal("an ID that is not in the pack was accepted")
		}
	})
}

func TestARunnerMissingACollaboratorSaysSoBeforeTouchingTheCluster(t *testing.T) {
	pack := packOf(scenarioIn("s-1", "ns-a", 1))
	cases := []struct {
		name string
		r    *Runner
		want error
	}{
		{"no subject", &Runner{Pack: pack, Arena: newFakeArena(), Injector: newFakeInjector()}, ErrNoSubject},
		{"no arena", &Runner{Pack: pack, Subject: &fakeSubject{}, Injector: newFakeInjector()}, ErrNoArena},
		{"no injector", &Runner{Pack: pack, Subject: &fakeSubject{}, Arena: newFakeArena()}, ErrNoInjector},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.r.Run(t.Context())
			if !errors.Is(err, tc.want) {
				t.Fatalf("Run = %v, want %v", err, tc.want)
			}
		})
	}
}

// Two scenarios in one namespace never overlap. If they did, the first one's
// fault would be live while the second one's subject investigated, and the
// second one's ground truth — which says nothing about the first fault —
// would charge the subject for reporting something that was really there.
func TestScenariosSharingANamespaceAreSerialised(t *testing.T) {
	arena := newFakeArena()
	subj := &fakeSubject{fn: func(context.Context, string) (eval.Report, error) {
		time.Sleep(20 * time.Millisecond)
		return eval.Report{Findings: []scenario.Finding{}}, nil
	}}
	r := &Runner{
		Pack: packOf(
			scenarioIn("s-1", "shared", 1),
			scenarioIn("s-2", "shared", 1),
			scenarioIn("s-3", "shared", 1),
		),
		Subject:     subj,
		Arena:       arena,
		Injector:    newFakeInjector(),
		Concurrency: 3,
	}
	if _, err := r.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := arena.peakFor("shared"); got != 1 {
		t.Errorf("%d scenarios held the shared namespace at once, want 1", got)
	}
}

// A control declares no namespace, so there is nothing to fence it against.
// It takes the cluster instead — because a control running beside a live fault
// sees real breakage, reports it correctly, and is scored as having invented it.
func TestAControlGetsTheClusterToItself(t *testing.T) {
	var (
		mu       sync.Mutex
		inFlight int
		overlap  bool
		control  bool
	)
	subj := &fakeSubject{fn: func(_ context.Context, prompt string) (eval.Report, error) {
		isControl := prompt == "anything wrong?"
		mu.Lock()
		inFlight++
		if inFlight > 1 && (isControl || control) {
			overlap = true
		}
		if isControl {
			control = true
		}
		mu.Unlock()

		time.Sleep(20 * time.Millisecond)

		mu.Lock()
		inFlight--
		if isControl {
			control = false
		}
		mu.Unlock()
		return eval.Report{Findings: []scenario.Finding{}}, nil
	}}

	r := &Runner{
		Pack: packOf(
			scenarioIn("s-1", "ns-a", 1),
			controlScenario("c-1"),
			scenarioIn("s-2", "ns-b", 1),
			scenarioIn("s-3", "ns-c", 1),
		),
		Subject:     subj,
		Arena:       newFakeArena(),
		Injector:    newFakeInjector(),
		Concurrency: 4,
	}
	if _, err := r.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if overlap {
		t.Error("a control ran beside another scenario; anything it saw could have been someone else's fault")
	}
}

// A control has no faults, so there is nothing to inject and nothing to clear,
// but it still reaches the subject. A control that never runs measures nothing,
// and measuring invention is the only reason controls are in the pack.
func TestAControlStillReachesTheSubject(t *testing.T) {
	arena := newFakeArena()
	inj := newFakeInjector()
	subj := &fakeSubject{}
	r := &Runner{Pack: packOf(controlScenario("c-1")), Subject: subj, Arena: arena, Injector: inj}

	runs, err := r.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := subj.asked(); len(got) != 1 {
		t.Fatalf("the subject was asked %v, want exactly one question", got)
	}
	if runs[0].Report == nil {
		t.Error("the control produced no report")
	}
	if !runs[0].InjectedAt.IsZero() {
		t.Error("InjectedAt was stamped on a control; nothing was injected")
	}
	if got := inj.appliedCount(); got != 0 {
		t.Errorf("applied %d faults for a control", got)
	}
	if len(arena.setup) != 0 {
		t.Errorf("provisioned %v for a control that names no namespace", arena.setup)
	}
}

// The disappearance of a fault while the subject is working is the subject
// having fixed it, and that is a measurement. The timestamp is an upper bound
// with the poll interval as its resolution, which is why the interval is a flag.
func TestRemediationIsObservedWhileTheSubjectWorks(t *testing.T) {
	inj := newFakeInjector()
	subj := &fakeSubject{fn: func(context.Context, string) (eval.Report, error) {
		inj.remove("f-001")
		time.Sleep(80 * time.Millisecond)
		return eval.Report{Findings: []scenario.Finding{}}, nil
	}}
	r := &Runner{
		Pack:            packOf(scenarioIn("s-1", "ns-a", 1)),
		Subject:         subj,
		Arena:           newFakeArena(),
		Injector:        inj,
		RemediationPoll: 5 * time.Millisecond,
	}
	runs, err := r.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runs[0].ClearedAt.IsZero() {
		t.Fatal("ClearedAt is zero; the fault went away while the subject worked")
	}
	if runs[0].ClearedAt.After(runs[0].DetectedAt) {
		t.Errorf("ClearedAt %s is after DetectedAt %s", runs[0].ClearedAt, runs[0].DetectedAt)
	}
}

// A fault that is still there is not remediation, and neither is a cluster
// that will not answer. Guessing "gone" from an API error hands the subject a
// time-to-remediate it did not earn.
func TestRemediationIsNotInferredFromSilence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*fakeInjector)
	}{
		{"the fault is still there", func(*fakeInjector) {}},
		{"the cluster will not answer", func(f *fakeInjector) { f.listErr = errBoom }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inj := newFakeInjector()
			tc.setup(inj)
			subj := &fakeSubject{fn: func(context.Context, string) (eval.Report, error) {
				time.Sleep(40 * time.Millisecond)
				return eval.Report{Findings: []scenario.Finding{}}, nil
			}}
			r := &Runner{
				Pack:            packOf(scenarioIn("s-1", "ns-a", 1)),
				Subject:         subj,
				Arena:           newFakeArena(),
				Injector:        inj,
				RemediationPoll: 5 * time.Millisecond,
			}
			runs, err := r.Run(t.Context())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !runs[0].ClearedAt.IsZero() {
				t.Errorf("ClearedAt = %s, want zero", runs[0].ClearedAt)
			}
		})
	}
}

func TestTheRemediationWatchIsOffWhenThePollIsZero(t *testing.T) {
	inj := newFakeInjector()
	subj := &fakeSubject{fn: func(context.Context, string) (eval.Report, error) {
		inj.remove("f-001")
		time.Sleep(20 * time.Millisecond)
		return eval.Report{Findings: []scenario.Finding{}}, nil
	}}
	r := &Runner{
		Pack:            packOf(scenarioIn("s-1", "ns-a", 1)),
		Subject:         subj,
		Arena:           newFakeArena(),
		Injector:        inj,
		RemediationPoll: 0,
	}
	runs, err := r.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !runs[0].ClearedAt.IsZero() {
		t.Errorf("ClearedAt = %s with the watch disabled", runs[0].ClearedAt)
	}
}

// Every audit event the executor, drivers and probes emit below runScenario
// carries the scenario ID, because it is stamped onto the context once rather
// than threaded through every call site. It is the key the offline scorer
// joins on, and a missing one is invisible until somebody tries to score.
func TestTheScenarioIDIsStampedOntoTheContext(t *testing.T) {
	var seen []string
	inj := newFakeInjector()
	r := &Runner{
		Pack:     packOf(scenarioIn("s-1", "ns-a", 1), scenarioIn("s-2", "ns-b", 1)),
		Subject:  &fakeSubject{},
		Arena:    newFakeArena(),
		Injector: inj,
	}
	subj := &fakeSubject{fn: func(ctx context.Context, _ string) (eval.Report, error) {
		seen = append(seen, audit.ScenarioIDFrom(ctx))
		return eval.Report{Findings: []scenario.Finding{}}, nil
	}}
	r.Subject = subj

	if _, err := r.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	slices.Sort(seen)
	if want := []string{"s-1", "s-2"}; !slices.Equal(seen, want) {
		t.Errorf("scenario IDs on the context were %v, want %v", seen, want)
	}
}

// A scenario whose arena never came up emits no fault event at all. Without a
// line naming it, the audit log has no record of a scenario the harness
// definitely attempted, and the offline join reports a corrupt pair of
// artifacts instead of a harness failure.
func TestEveryAttemptedScenarioLeavesAnAuditTrace(t *testing.T) {
	arena := newFakeArena()
	arena.fail["ns-a"] = errBoom
	aud := &recordingAuditor{}

	r := &Runner{
		Pack:     packOf(scenarioIn("s-1", "ns-a", 1)),
		Subject:  &fakeSubject{},
		Arena:    arena,
		Injector: newFakeInjector(),
		Auditor:  aud,
	}
	if _, err := r.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	started := aud.named(audit.EventEvalScenarioStarted)
	if len(started) != 1 || started[0].ScenarioID != "s-1" {
		t.Fatalf("started events = %+v, want one naming s-1", started)
	}
	done := aud.named(audit.EventEvalScenarioCompleted)
	if len(done) != 1 || done[0].ScenarioID != "s-1" {
		t.Fatalf("completed events = %+v, want one naming s-1", done)
	}
	if done[0].Reason == "" {
		t.Error("the completed event gives no reason for a scenario whose arena failed")
	}
}

// A scenario can fail on either side, and the audit line has to say which. A
// subject that crashed and a fault that never landed are the same shape in the
// log and opposite facts about who to blame.
func TestTheCompletedLineNamesTheSubjectsFailureToo(t *testing.T) {
	aud := &recordingAuditor{}
	r := &Runner{
		Pack:     packOf(scenarioIn("s-1", "ns-a", 1)),
		Subject:  &fakeSubject{fn: func(context.Context, string) (eval.Report, error) { return eval.Report{}, errBoom }},
		Arena:    newFakeArena(),
		Injector: newFakeInjector(),
		Auditor:  aud,
	}
	if _, err := r.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	done := aud.named(audit.EventEvalScenarioCompleted)
	if len(done) != 1 {
		t.Fatalf("completed events = %+v, want one", done)
	}
	if !strings.Contains(done[0].Reason, errBoom.Error()) {
		t.Errorf("reason = %q, want the subject's failure; injection succeeded here", done[0].Reason)
	}
}

func TestARunnerWithNoAuditorStillRuns(t *testing.T) {
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

func TestDescribeNamesThePackAndTheSelection(t *testing.T) {
	pack := packOf(scenarioIn("s-1", "ns-a", 1), scenarioIn("s-2", "ns-b", 1), scenarioIn("s-3", "ns-c", 1))
	got := Describe(pack, pack.Scenarios[:2])
	for _, want := range []string{"test", "2 of 3", "s-1", "s-2"} {
		if !strings.Contains(got, want) {
			t.Errorf("Describe = %q, want it to mention %q", got, want)
		}
	}
}

// Overlapping namespace sets are taken in sorted order, which is what makes
// two scenarios that share one of two namespaces safe to start at once. Taken
// in the order they are named, {a,b} and {b,a} deadlock.
func TestOverlappingNamespaceSetsDoNotDeadlock(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		locks := newNamespaceLocks()
		var wg sync.WaitGroup
		for range 20 {
			wg.Add(2)
			go func() { defer wg.Done(); locks.acquire([]string{"a", "b"})() }()
			go func() { defer wg.Done(); locks.acquire([]string{"b", "c"})() }()
		}
		wg.Wait()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("namespace locks deadlocked")
	}
}

func TestTheConcurrencyCeilingIsHonoured(t *testing.T) {
	var (
		mu   sync.Mutex
		now  int
		peak int
	)
	subj := &fakeSubject{fn: func(context.Context, string) (eval.Report, error) {
		mu.Lock()
		now++
		peak = max(peak, now)
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		now--
		mu.Unlock()
		return eval.Report{Findings: []scenario.Finding{}}, nil
	}}

	var ss []scenario.Scenario
	for i := range 8 {
		ss = append(ss, scenarioIn(string(rune('a'+i)), "ns-"+string(rune('a'+i)), 1))
	}
	r := &Runner{Pack: packOf(ss...), Subject: subj, Arena: newFakeArena(), Injector: newFakeInjector(), Concurrency: 2}
	if _, err := r.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if peak > 2 {
		t.Errorf("%d scenarios were in flight at once, want at most 2", peak)
	}
}

func ids(ss []scenario.Scenario) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.ID)
	}
	return out
}
