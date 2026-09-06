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

// Package harness runs a scenario pack against a subject on a real cluster.
//
// # It has no back door
//
// Faults go in through the same executor an operator's `simian chaos` call
// goes through: same schema validation, same safety stages, same leases, same
// audit events. A harness with a privileged shortcut stops measuring the
// product and starts measuring the shortcut, and the first thing it would stop
// catching is the executor rejecting something it should have applied.
//
// # It produces the artifacts the offline scorer reads
//
// The audit log written during a run and the run file written at the end of it
// are exactly what `simian evaluate` consumes. That is not a convenience: it
// means the live numbers and the offline numbers come from one code path, and
// a live scorecard can be reproduced months later by anyone holding the two
// files.
//
// # Scoring lives elsewhere
//
// This package decides what happened. pkg/eval decides what it was worth.
// Keeping the impure half here is what lets the scoring half promise that the
// same inputs always produce the same scores.
package harness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/eval"
	"github.com/go-steer/simian-agent/pkg/scenario"
	"github.com/go-steer/simian-agent/pkg/simian"
)

// Arena is the per-scenario cluster lifecycle: make a namespace a place chaos
// is allowed, and put it back afterwards.
type Arena interface {
	Setup(ctx context.Context, namespace string) error
	Teardown(ctx context.Context, namespace string) error
}

// Substrates stands a scenario's named SUT up in a namespace and takes it
// down again. Only scenarios that set Scenario.Substrate need one.
//
// Known is separate from Deploy so that a pack naming a SUT nobody registered
// is refused before the run starts, rather than in the middle of scenario
// seven with six arenas already spent.
type Substrates interface {
	Known(name string) bool
	Deploy(ctx context.Context, namespace, name string) error
	Destroy(ctx context.Context, namespace, name string) error
}

// Injector is the executor, narrowed to what a run needs. *executor.Executor
// satisfies it as-is, which is the point: the harness cannot reach past this
// interface into a privileged path the product does not have.
type Injector interface {
	Apply(ctx context.Context, m simian.FaultManifest) (string, error)
	Clear(ctx context.Context, faultUID string) error
	ListActive(ctx context.Context, namespace string) ([]simian.ActiveFault, error)
}

// The collaborators a Runner cannot invent for itself. Reported before the
// first namespace is touched: a suite that dies on a nil pointer four
// scenarios in has already changed a cluster and has nothing to show for it.
var (
	ErrNoSubject  = errors.New("harness: no subject configured")
	ErrNoArena    = errors.New("harness: no arena configured")
	ErrNoInjector = errors.New("harness: no injector configured")
)

// Defaults for the knobs a caller usually leaves alone.
const (
	DefaultConcurrency     = 1
	DefaultSubjectTimeout  = 10 * time.Minute
	DefaultRemediationPoll = 5 * time.Second
	DefaultTeardownTimeout = 2 * time.Minute
)

// Runner executes a pack against a subject.
type Runner struct {
	Pack     scenario.Pack
	Subject  eval.Subject
	Arena    Arena
	Injector Injector

	// Substrates is consulted only by scenarios that set a Substrate. A pack
	// with none never touches it, which is why it is not in validate().
	Substrates Substrates

	// Auditor is the same auditor the executor writes through. The harness
	// uses it for one thing: a line per scenario it attempted, so that a
	// scenario which failed before any fault event existed is still in the
	// log the scorer joins against. Optional, but a run whose artifacts are
	// meant to be scored wants it.
	Auditor simian.Auditor

	// Only restricts the run to these scenario IDs. Empty runs the pack.
	Only []string

	// Concurrency bounds how many scenarios are in flight. It is a ceiling,
	// not a target: scenarios that would share a namespace are serialised
	// regardless (see Run).
	Concurrency int

	// RemediationPoll is how often the cluster is asked whether the fault is
	// still there while the subject works. Zero disables the watch.
	RemediationPoll time.Duration

	// TeardownTimeout bounds cleanup, which runs on a context that ignores
	// cancellation. Without a bound, a Ctrl-C into a wedged API server would
	// hang the harness on the way out.
	TeardownTimeout time.Duration

	// Now is the clock, injectable for tests.
	Now func() time.Time

	Logger *slog.Logger
}

// Run executes the selected scenarios and returns one eval.Run each, in pack
// order regardless of the order they finished in.
//
// # Two scenarios never share a namespace at the same time
//
// Concurrency is bounded by the Concurrency field and then bounded again by
// the cluster: a scenario holds every namespace it touches for its whole
// lifetime. Running two scenarios in one namespace concurrently would let the
// first one's fault be live while the second one's subject is investigating,
// and the second one's ground truth — which says nothing about the first
// fault — would charge the subject for reporting something that was really
// there.
//
// A scenario that names no namespace at all is a control, and there is nothing
// to fence it against. It takes the whole cluster instead. That looks
// heavy-handed until you notice the alternative: a control run beside a live
// fault sees real breakage, reports it correctly, and is scored as having
// hallucinated it.
func (r *Runner) Run(ctx context.Context) ([]eval.Run, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	selected, err := r.Selection()
	if err != nil {
		return nil, err
	}
	if err := r.checkSubstrates(selected); err != nil {
		return nil, err
	}

	locks := newNamespaceLocks()
	sem := make(chan struct{}, max(r.concurrency(), 1))
	runs := make([]eval.Run, len(selected))

	var wg sync.WaitGroup
	for i, s := range selected {
		wg.Add(1)
		go func() {
			defer wg.Done()

			sem <- struct{}{}
			defer func() { <-sem }()

			release := locks.acquire(s.Namespaces())
			defer release()

			runs[i] = r.runScenario(ctx, s)
		}()
	}
	wg.Wait()

	return runs, nil
}

func (r *Runner) validate() error {
	switch {
	case r.Subject == nil:
		return ErrNoSubject
	case r.Arena == nil:
		return ErrNoArena
	case r.Injector == nil:
		return ErrNoInjector
	default:
		return nil
	}
}

// checkSubstrates refuses a selection naming a SUT this run cannot deploy.
//
// Before the first namespace, for the same reason --only is resolved early: a
// name that will never resolve should cost nothing to discover, and a suite
// that dies partway through has already spent cluster time on scenarios whose
// scorecard it will not produce.
func (r *Runner) checkSubstrates(selected []scenario.Scenario) error {
	var errs []error
	for _, s := range selected {
		if s.Substrate == "" {
			continue
		}
		if r.Substrates == nil {
			errs = append(errs, fmt.Errorf("harness: scenario %q needs substrate %q but this run has no substrate registry", s.ID, s.Substrate))
			continue
		}
		if !r.Substrates.Known(s.Substrate) {
			errs = append(errs, fmt.Errorf("harness: scenario %q names substrate %q, which is not registered", s.ID, s.Substrate))
		}
	}
	return errors.Join(errs...)
}

// Selection resolves --only against the pack.
func (r *Runner) Selection() ([]scenario.Scenario, error) { return Select(r.Pack, r.Only) }

// Select resolves a set of --only IDs against a pack.
//
// An ID that is not in the pack is an error rather than an empty run. A typo
// in a scenario ID that silently grades nothing is how a suite comes back
// green having measured nothing at all.
//
// Exported and free-standing because the answer is needed before there is a
// Runner to ask. Anything that validates the run against what it will actually
// do — rather than against everything the pack happens to contain — has to
// resolve the selection first, and doing that twice with two implementations
// is how the two answers drift.
func Select(pack scenario.Pack, only []string) ([]scenario.Scenario, error) {
	if len(only) == 0 {
		return pack.Scenarios, nil
	}

	want := map[string]bool{}
	for _, id := range only {
		if _, ok := pack.ByID(id); !ok {
			return nil, fmt.Errorf("harness: --only %q is not a scenario in pack %q", id, pack.Name)
		}
		want[id] = true
	}

	// Pack order, not flag order: a scorecard's rows should not depend on how
	// the operator typed the flag.
	var out []scenario.Scenario
	for _, s := range pack.Scenarios {
		if want[s.ID] {
			out = append(out, s)
		}
	}
	return out, nil
}

// runScenario is the whole flow for one scenario: provision, inject, gate, ask,
// collect, clean up.
func (r *Runner) runScenario(ctx context.Context, s scenario.Scenario) eval.Run {
	// Stamped once, here. Every audit event the executor, the drivers and the
	// probes emit below this line carries the scenario ID, which is the key
	// the offline scorer joins on. Threading it as an argument instead would
	// mean the next new call site forgets it, and a missing join key is
	// invisible until someone tries to score the run.
	ctx = audit.WithScenarioID(ctx, s.ID)

	run := eval.Run{ScenarioID: s.ID, Subject: r.Subject.Name()}
	log := r.logger().With(slog.String("scenario", s.ID))

	namespaces := s.Namespaces()
	r.emit(ctx, simian.AuditEvent{
		Event:      audit.EventEvalScenarioStarted,
		ScenarioID: s.ID,
		Payload: map[string]any{
			"subject":    run.Subject,
			"pack":       r.Pack.Name,
			"faults":     len(s.Faults),
			"namespaces": namespaces,
		},
	})
	defer func() {
		r.emit(ctx, simian.AuditEvent{
			Event:      audit.EventEvalScenarioCompleted,
			ScenarioID: s.ID,
			Reason:     firstNonEmpty(run.InjectError, run.SubjectError),
		})
	}()

	var applied, substrated []string
	provisioned, err := r.setup(ctx, namespaces)
	defer func() { r.teardown(ctx, log, provisioned, substrated, s.Substrate, applied) }()
	if err != nil {
		run.InjectError = err.Error()
		log.Error("harness: arena setup failed", slog.String("error", err.Error()))
		return run
	}

	// Before the faults, because the faults are aimed at it. A dataplane
	// scenario's fault selects the substrate's pods by name or by label, and
	// a NetworkChaos that matches nothing is applied, gated and cleared
	// without ever having perturbed anything.
	substrated, err = r.deploySubstrate(ctx, log, s, provisioned)
	if err != nil {
		run.InjectError = err.Error()
		log.Error("harness: substrate deploy failed", slog.String("error", err.Error()))
		return run
	}

	applied, err = r.inject(ctx, s)
	if err != nil {
		// The harness's own failure, never the subject's. Asking a subject to
		// investigate a cluster that was never broken produces a report that
		// cannot be scored either way, so it is not asked.
		run.InjectError = err.Error()
		log.Error("harness: injection failed", slog.String("error", err.Error()))
		return run
	}
	run.Manifested = true
	if len(s.Faults) > 0 {
		// Apply returns only once the fault's Settle probes have passed, so
		// this is the first moment the cluster was observably broken — the
		// earliest a subject could honestly have detected anything.
		run.InjectedAt = r.now()
	}

	stopWatch := r.watchRemediation(ctx, namespaces, applied, &run)
	report, subjectErr := r.Subject.Investigate(ctx, s.Prompt)
	run.DetectedAt = r.now()
	stopWatch()

	if subjectErr != nil {
		// Scored as a zero, not skipped. A subject that crashes must not be
		// able to improve its mean by crashing.
		run.SubjectError = subjectErr.Error()
		log.Warn("harness: subject failed", slog.String("error", subjectErr.Error()))
		return run
	}
	run.Report = &report
	log.Info("harness: scenario complete", slog.Int("findings", len(report.Findings)))
	return run
}

// setup provisions arenas, returning the ones it created so a partial failure
// still tears down what it made.
func (r *Runner) setup(ctx context.Context, namespaces []string) ([]string, error) {
	var made []string
	for _, ns := range namespaces {
		if err := r.Arena.Setup(ctx, ns); err != nil {
			return made, fmt.Errorf("arena %s: %w", ns, err)
		}
		made = append(made, ns)
	}
	return made, nil
}

// deploySubstrate stands the scenario's SUT up in every namespace it was
// given, returning the ones that took it so a partial failure still cleans up
// what it made.
func (r *Runner) deploySubstrate(ctx context.Context, log *slog.Logger, s scenario.Scenario, namespaces []string) ([]string, error) {
	if s.Substrate == "" {
		return nil, nil
	}
	var made []string
	for _, ns := range namespaces {
		log.Info("harness: deploying substrate", slog.String("substrate", s.Substrate), slog.String("namespace", ns))
		if err := r.Substrates.Deploy(ctx, ns, s.Substrate); err != nil {
			return made, fmt.Errorf("substrate %s in %s: %w", s.Substrate, ns, err)
		}
		made = append(made, ns)
	}
	return made, nil
}

// inject applies every fault through the executor, in order.
//
// The first failure stops the run. A cascade is one incident, and the half of
// it that landed is not the incident the expectations describe.
func (r *Runner) inject(ctx context.Context, s scenario.Scenario) ([]string, error) {
	var uids []string
	for i, f := range s.Faults {
		uid, err := r.Injector.Apply(ctx, f)
		if err != nil {
			return uids, fmt.Errorf("fault %d (%s/%s): %w", i, f.Engine, f.ResourceKind, err)
		}
		uids = append(uids, uid)
	}
	return uids, nil
}

// watchRemediation polls the cluster while the subject works and stamps the
// first moment none of the scenario's faults are active any more.
//
// The lease reaper was built to stop Simian leaking faults into a cluster. It
// becomes a measuring instrument the moment the subject is allowed to write:
// a fault that is already gone when we look is not an error, it is the subject
// having fixed it. The timestamp is an upper bound with the poll interval as
// its resolution, which is honest and is why the interval is a flag.
func (r *Runner) watchRemediation(ctx context.Context, namespaces, uids []string, run *eval.Run) (stop func()) {
	if r.RemediationPoll <= 0 || len(uids) == 0 || len(namespaces) == 0 {
		return func() {}
	}

	watchCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	var mu sync.Mutex

	go func() {
		defer close(done)
		ticker := time.NewTicker(r.RemediationPoll)
		defer ticker.Stop()
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-ticker.C:
				if !r.allCleared(watchCtx, namespaces, uids) {
					continue
				}
				mu.Lock()
				run.ClearedAt = r.now()
				mu.Unlock()
				return
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}
}

// allCleared reports whether none of uids is still leased. An error asking is
// not remediation: a cluster that will not answer says nothing about whether
// the fault is still there, and guessing "gone" would hand the subject a
// time-to-remediate it did not earn.
func (r *Runner) allCleared(ctx context.Context, namespaces, uids []string) bool {
	live := map[string]bool{}
	for _, ns := range namespaces {
		active, err := r.Injector.ListActive(ctx, ns)
		if err != nil {
			return false
		}
		for _, af := range active {
			live[af.FaultUID] = true
		}
	}
	for _, uid := range uids {
		if live[uid] {
			return false
		}
	}
	return true
}

// teardown clears whatever is still leased and destroys the arenas it made.
//
// It runs on a context that ignores cancellation, with its own deadline. A
// Ctrl-C in the middle of a suite must still take the chaos out of the
// cluster: the alternative is a partition left behind in someone's namespace
// because they changed their mind about the run.
func (r *Runner) teardown(ctx context.Context, log *slog.Logger, namespaces, substrated []string, substrate string, faultUIDs []string) {
	if len(namespaces) == 0 && len(substrated) == 0 && len(faultUIDs) == 0 {
		return
	}

	timeout := r.TeardownTimeout
	if timeout <= 0 {
		timeout = DefaultTeardownTimeout
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()

	// Faults first: an arena with chaos still leased in it refuses to be
	// destroyed, and rightly so.
	for _, uid := range faultUIDs {
		if err := r.Injector.Clear(ctx, uid); err != nil {
			log.Warn("harness: clearing fault failed", slog.String("fault_uid", uid), slog.String("error", err.Error()))
		}
	}
	// Then the substrate, before the namespace that holds it. Destroying it
	// is nearly always redundant — the arena teardown deletes the whole
	// namespace a moment later — but not under --keep-arenas, which is the
	// mode somebody uses to look at the wreckage, and a substrate left
	// running there is a workload nobody remembers deploying.
	for _, ns := range substrated {
		if err := r.Substrates.Destroy(ctx, ns, substrate); err != nil {
			log.Warn("harness: substrate teardown failed",
				slog.String("substrate", substrate), slog.String("namespace", ns), slog.String("error", err.Error()))
		}
	}
	for _, ns := range namespaces {
		if err := r.Arena.Teardown(ctx, ns); err != nil {
			log.Warn("harness: arena teardown failed", slog.String("namespace", ns), slog.String("error", err.Error()))
		}
	}
}

func (r *Runner) emit(ctx context.Context, ev simian.AuditEvent) {
	if r.Auditor == nil {
		return
	}
	r.Auditor.Emit(ctx, ev)
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func (r *Runner) concurrency() int {
	if r.Concurrency <= 0 {
		return DefaultConcurrency
	}
	return r.Concurrency
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now().UTC()
}

func (r *Runner) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// namespaceLocks serialises scenarios that would otherwise share a namespace.
//
// The global lock is a read-write lock read backwards from the usual: a
// scenario with namespaces is a *reader* — several can hold the cluster at
// once, kept apart by their per-namespace locks — and a scenario with none is
// a *writer*, which is how a control gets the cluster to itself.
type namespaceLocks struct {
	global sync.RWMutex

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func newNamespaceLocks() *namespaceLocks {
	return &namespaceLocks{locks: map[string]*sync.Mutex{}}
}

// acquire takes every named namespace and returns the release. Locks are taken
// in sorted order, which is what makes two scenarios with overlapping
// namespace sets safe to start at the same time.
//
// No namespaces means the whole cluster: see Run.
func (n *namespaceLocks) acquire(namespaces []string) func() {
	if len(namespaces) == 0 {
		n.global.Lock()
		return n.global.Unlock
	}

	n.global.RLock()
	held := make([]*sync.Mutex, 0, len(namespaces))
	for _, ns := range namespaces {
		held = append(held, n.lockFor(ns))
	}
	for _, m := range held {
		m.Lock()
	}
	return func() {
		for i := len(held) - 1; i >= 0; i-- {
			held[i].Unlock()
		}
		n.global.RUnlock()
	}
}

func (n *namespaceLocks) lockFor(ns string) *sync.Mutex {
	n.mu.Lock()
	defer n.mu.Unlock()
	m, ok := n.locks[ns]
	if !ok {
		m = &sync.Mutex{}
		n.locks[ns] = m
	}
	return m
}

// Describe renders a one-line summary of what a run will do, for the log line
// that goes out before the cluster is touched.
func Describe(pack scenario.Pack, selected []scenario.Scenario) string {
	ids := make([]string, 0, len(selected))
	for _, s := range selected {
		ids = append(ids, s.ID)
	}
	return fmt.Sprintf("pack %s: %d of %d scenarios (%s)", pack.Name, len(selected), pack.Len(), strings.Join(ids, ", "))
}
