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
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/eval"
	"github.com/go-steer/simian-agent/pkg/scenario"
	"github.com/go-steer/simian-agent/pkg/simian"
)

// auditingInjector emits the events the executor emits — driver.applied and a
// fault.efficacy record per fault — so a round-trip test exercises the real
// audit-log shape rather than one invented for the test.
type auditingInjector struct {
	*fakeInjector
	auditor simian.Auditor

	// gateFails names namespaces whose faults should record a failed efficacy
	// probe: the cluster accepted the object and nothing actually broke.
	// Keyed by namespace rather than by fault UID because scenarios run
	// concurrently and the UID a given scenario draws is a race.
	gateFails map[string]bool
}

func (a *auditingInjector) Apply(ctx context.Context, m simian.FaultManifest) (string, error) {
	uid, err := a.fakeInjector.Apply(ctx, m)
	if err != nil {
		return "", err
	}
	a.auditor.Emit(ctx, simian.AuditEvent{Event: audit.EventDriverApplied, FaultUID: uid})

	passed := true
	for _, t := range m.Targets {
		if a.gateFails[t.Namespace] {
			passed = false
		}
	}
	payload := map[string]any{"probe": "pod-not-ready", "passed": passed}
	if !passed {
		payload["expected"] = "unschedulable"
		payload["observed"] = "Running"
	}
	a.auditor.Emit(ctx, simian.AuditEvent{Event: audit.EventFaultEfficacy, FaultUID: uid, Payload: payload})
	return uid, nil
}

// jsonlAuditor writes the JSONL an operator's `simian-eval` writes, so the
// bytes under test are the bytes that land in audit.log.
func jsonlAuditor(w *bytes.Buffer) simian.Auditor {
	return audit.New(slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})))
}

// The live run and the offline score come from one set of facts. If the two
// artifacts did not join, a scorecard could only ever be read at the moment it
// was produced — and the whole point of writing them is that somebody can
// reproduce the number months later from the files alone.
func TestTheArtifactsAHarnessWritesAreTheOnesTheScorerReads(t *testing.T) {
	var auditLog bytes.Buffer
	auditor := jsonlAuditor(&auditLog)

	inj := &auditingInjector{fakeInjector: newFakeInjector(), auditor: auditor}
	pack := packOf(
		scenarioIn("s-1", "ns-a", 1),
		scenarioIn("s-2", "ns-b", 2),
		controlScenario("c-1"),
	)
	subj := &fakeSubject{name: "agent", fn: func(_ context.Context, prompt string) (eval.Report, error) {
		if strings.Contains(prompt, "ns-a") {
			// The right answer for s-1: the expected root finding.
			return eval.Report{Findings: []scenario.Finding{
				{Kind: "Pod", ResourceName: "api", Severity: scenario.SeverityCritical},
			}}, nil
		}
		return eval.Report{Findings: []scenario.Finding{}}, nil
	}}

	r := &Runner{Pack: pack, Subject: subj, Arena: newFakeArena(), Injector: inj, Auditor: auditor}
	runs, err := r.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	facts, err := eval.ReadAudit(bytes.NewReader(auditLog.Bytes()))
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}

	var runBuf bytes.Buffer
	if err := WriteRunFile(&runBuf, RunFile(subj.Name(), runs)); err != nil {
		t.Fatalf("WriteRunFile: %v", err)
	}
	runFile, err := eval.ReadRunFile(bytes.NewReader(runBuf.Bytes()))
	if err != nil {
		t.Fatalf("ReadRunFile: %v", err)
	}

	joined, err := eval.Join(pack, facts, runFile)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if len(joined) != 3 {
		t.Fatalf("joined %d runs, want 3", len(joined))
	}
	for _, run := range joined {
		if !run.Manifested {
			t.Errorf("%s: the offline read says it did not manifest: %s", run.ScenarioID, run.InjectError)
		}
	}

	summary, err := eval.Summarize(runFile.Subject, pack, joined)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if summary.Subject != "agent" {
		t.Errorf("Subject = %q, want agent", summary.Subject)
	}
	if summary.Scenarios != 3 || summary.Manifested != 3 || summary.InjectFailures != 0 {
		t.Errorf("summary counts = %d/%d/%d, want 3/3/0", summary.Scenarios, summary.Manifested, summary.InjectFailures)
	}
	if summary.EfficacyRate != 1 {
		t.Errorf("EfficacyRate = %v, want 1", summary.EfficacyRate)
	}
	if got := summary.Means[eval.MeasureRecall]; got <= 0 {
		t.Errorf("recall = %v; the subject answered s-1 correctly", got)
	}
}

// A fault the cluster accepted but silently dropped is not a fault. The
// scenario is NOT SCORED rather than a miss, and the two artifacts agree on
// that without anyone having to write it down twice.
func TestAFailedEfficacyGateSurvivesTheRoundTripAsAnInjectFailure(t *testing.T) {
	var auditLog bytes.Buffer
	auditor := jsonlAuditor(&auditLog)

	inj := &auditingInjector{
		fakeInjector: newFakeInjector(),
		auditor:      auditor,
		gateFails:    map[string]bool{"ns-a": true},
	}
	pack := packOf(scenarioIn("s-1", "ns-a", 1), scenarioIn("s-2", "ns-b", 1))
	r := &Runner{Pack: pack, Subject: &fakeSubject{name: "agent"}, Arena: newFakeArena(), Injector: inj, Auditor: auditor}

	runs, err := r.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	facts, err := eval.ReadAudit(bytes.NewReader(auditLog.Bytes()))
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	joined, err := eval.Join(pack, facts, RunFile("agent", runs))
	if err != nil {
		t.Fatalf("Join: %v", err)
	}

	byID := map[string]eval.Run{}
	for _, run := range joined {
		byID[run.ScenarioID] = run
	}
	if got := byID["s-1"]; got.Manifested || got.InjectError == "" {
		t.Errorf("s-1 = %+v, want it reported as not having manifested", got)
	}
	if !strings.Contains(byID["s-1"].InjectError, "pod-not-ready") {
		t.Errorf("s-1 InjectError = %q, want it to name the gate that failed", byID["s-1"].InjectError)
	}
	if got := byID["s-2"]; !got.Manifested {
		t.Errorf("s-2 = %+v, want it to have manifested; only s-1's gate failed", got)
	}

	// The harness's own view agreed at the time: it does not know about the
	// gate, so it says the apply succeeded, and the audit log is what corrects
	// it. That is the point of not letting the harness certify itself.
	summary, err := eval.Summarize("agent", pack, joined)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if summary.InjectFailures != 1 {
		t.Errorf("InjectFailures = %d, want 1", summary.InjectFailures)
	}
	if summary.EfficacyRate != 0.5 {
		t.Errorf("EfficacyRate = %v, want 0.5", summary.EfficacyRate)
	}
}

// A scenario whose arena never came up produces no fault events at all. The
// eval.scenario_started line is the only reason the offline join can see it,
// and seeing it is what turns "the artifacts are corrupt" into "the harness
// failed on this scenario, do not score it".
func TestAScenarioThatFailedBeforeAnyFaultStillJoins(t *testing.T) {
	var auditLog bytes.Buffer
	auditor := jsonlAuditor(&auditLog)

	arena := newFakeArena()
	arena.fail["ns-a"] = errBoom
	inj := &auditingInjector{fakeInjector: newFakeInjector(), auditor: auditor}

	pack := packOf(scenarioIn("s-1", "ns-a", 1), scenarioIn("s-2", "ns-b", 1))
	r := &Runner{Pack: pack, Subject: &fakeSubject{name: "agent"}, Arena: arena, Injector: inj, Auditor: auditor}

	runs, err := r.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	facts, err := eval.ReadAudit(bytes.NewReader(auditLog.Bytes()))
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	joined, err := eval.Join(pack, facts, RunFile("agent", runs))
	if err != nil {
		t.Fatalf("Join: %v — a scenario the harness attempted was missing from the audit log", err)
	}
	for _, run := range joined {
		if run.ScenarioID == "s-1" && run.Manifested {
			t.Error("s-1 manifested, but its arena never came up")
		}
		if run.ScenarioID == "s-2" && !run.Manifested {
			t.Errorf("s-2 did not manifest: %s", run.InjectError)
		}
	}
}
