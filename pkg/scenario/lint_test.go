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

package scenario

import (
	"strings"
	"testing"
	"time"

	"github.com/go-steer/simian-agent/pkg/simian"
)

func promptScenario(prompt string) Scenario {
	return Scenario{
		ID:     "s-1",
		Name:   "test",
		Prompt: prompt,
		Source: SourcePackParity,
		Faults: []simian.FaultManifest{{
			Engine:       simian.EngineKubeState,
			APIVersion:   "apps/v1",
			ResourceKind: "ImageUnresolvable",
			Duration:     5 * time.Minute,
			Targets:      []simian.TargetRef{{Namespace: "shop", Name: "checkout-api"}},
		}},
		Expect: []ExpectedFinding{{Kind: "Pod", Name: "checkout-api"}},
	}
}

// A prompt that names the failure mode turns a diagnosis task into a
// paraphrase task: the subject no longer has to look at the cluster to be
// right. This is the rule the whole live tier rests on, so it is enforced by
// a test rather than by review.
func TestLintPromptRejectsALeakedFailureMode(t *testing.T) {
	for _, prompt := range []string{
		"The checkout flow is broken because a pod is in CrashLoopBackOff.",
		"A container keeps crash-looping in namespace shop.",
		"Investigate the ImagePullBackOff in namespace shop.",
		"Something got OOM-killed, take a look at namespace shop.",
		"A pod is Pending in namespace shop.",
		"There is a pod stuck unschedulable, please look at namespace shop.",
		"The service ran out of memory. Check namespace shop.",
		"Latency is up in namespace shop.",
		"A workload is restarting in namespace shop.",
		"Something is degraded in namespace shop.",
	} {
		s := promptScenario(prompt)
		// Use a target name that cannot itself trip the workload half, so
		// this test only measures the failure-mode half.
		s.Faults[0].Targets[0].Name = "zzz"
		s.Expect[0].Name = "zzz"
		if err := LintPrompt(s); err == nil {
			t.Errorf("prompt %q was accepted; it names a failure mode", prompt)
		} else if !strings.Contains(err.Error(), "failure mode") {
			t.Errorf("prompt %q: error %q does not mention the failure mode", prompt, err)
		}
	}
}

// The other half: naming the workload tells the subject where the fault is,
// which is most of the diagnosis on a namespace with a dozen services.
func TestLintPromptRejectsALeakedWorkloadName(t *testing.T) {
	for _, prompt := range []string{
		"Take a look at checkout-api in namespace shop.",
		"Is checkout-api behaving?",
		"Something is wrong with CHECKOUT-API.",
	} {
		if err := LintPrompt(promptScenario(prompt)); err == nil {
			t.Errorf("prompt %q was accepted; it names the target workload", prompt)
		} else if !strings.Contains(err.Error(), "target workload") {
			t.Errorf("prompt %q: error %q does not mention the workload", prompt, err)
		}
	}
}

// The workload vocabulary is derived from the scenario, so a name that
// appears only in an expectation — never in a target — is still a leak.
func TestLintPromptCatchesANameThatOnlyAppearsInAnExpectation(t *testing.T) {
	s := promptScenario("Check on payments-worker, please.")
	s.Faults[0].Targets[0].Name = "" // selected by label, not by name
	s.Expect = []ExpectedFinding{{Kind: "Pod", Name: "payments-worker"}}
	if err := LintPrompt(s); err == nil {
		t.Fatal("a workload named only in an expectation was not caught")
	}
}

// A label selector is how most faults choose their victim, so the values in
// one are workload names for linting purposes.
func TestLintPromptCatchesANameFromALabelSelector(t *testing.T) {
	s := promptScenario("Have a look at frontend.")
	s.Faults[0].Targets[0].Name = ""
	s.Faults[0].Targets[0].Labels = map[string]string{"app": "frontend"}
	s.Expect[0].Name = "zzz"
	if err := LintPrompt(s); err == nil {
		t.Fatal("a workload named by label selector was not caught")
	}
}

// Naming the namespace is the task, not the answer. If this were rejected
// there would be no legal prompt at all.
func TestLintPromptAcceptsTheTaskFraming(t *testing.T) {
	for _, prompt := range []string{
		"Check the health of namespace shop and report what you find.",
		"An alert fired for namespace shop. Investigate and report.",
		"Something is wrong in namespace shop. Find it.",
		"Triage namespace shop.",
	} {
		s := promptScenario(prompt)
		if err := LintPrompt(s); err != nil {
			t.Errorf("prompt %q was rejected: %v", prompt, err)
		}
	}
}

// The failure-mode list is matched two different ways for a reason: short
// common words are matched whole, long distinctive phrases as substrings. Get
// that wrong and "room" reads as "OOM", which would reject good prompts and
// teach everyone to ignore the lint.
func TestLintPromptDoesNotFireOnOrdinaryEnglish(t *testing.T) {
	for _, prompt := range []string{
		"Check the war room dashboard, then triage namespace shop.",
		"Depending on what you find in namespace shop, escalate.",
		"Spending on namespace shop is up; check its health.",
		"Report on the state of namespace shop.",
		"The on-call is impending; review namespace shop.",
	} {
		s := promptScenario(prompt)
		s.Faults[0].Targets[0].Name = "zzz"
		s.Expect[0].Name = "zzz"
		if err := LintPrompt(s); err != nil {
			t.Errorf("prompt %q was rejected as a leak, but it is ordinary English: %v", prompt, err)
		}
	}
}

func TestLintPromptRejectsAnEmptyPrompt(t *testing.T) {
	s := promptScenario("   ")
	if err := LintPrompt(s); err == nil {
		t.Fatal("an empty prompt was accepted")
	}
}

func TestValidateRequiresAnID(t *testing.T) {
	s := promptScenario("Check namespace shop.")
	s.ID = ""
	err := s.Validate()
	if err == nil || !strings.Contains(err.Error(), "join key") {
		t.Fatalf("Validate() = %v; want a complaint about the missing ID", err)
	}
}

func TestValidateAcceptsAWellFormedScenario(t *testing.T) {
	if err := promptScenario("Check namespace shop and report.").Validate(); err != nil {
		t.Fatalf("Validate() = %v; want nil", err)
	}
}

// A fault with no way to prove it landed would let a subject be graded on a
// cluster that was never broken — a zero that means "nothing to find" is
// indistinguishable from a zero that means "missed it".
func TestValidateRejectsAFaultWithNoSettleGate(t *testing.T) {
	s := promptScenario("Check namespace shop.")
	s.Faults[0].ResourceKind = "NoSuchKind"
	err := s.Validate()
	if err == nil || !strings.Contains(err.Error(), "Settle probe") {
		t.Fatalf("Validate() = %v; want a complaint about the missing gate", err)
	}
}

func TestValidateAcceptsAnExplicitProbeInPlaceOfADefaultGate(t *testing.T) {
	s := promptScenario("Check namespace shop.")
	s.Faults[0].ResourceKind = "NoSuchKind"
	s.Faults[0].Probes = []simian.ProbeSpec{{Name: "custom", Mode: simian.ProbeModeSettle}}
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() = %v; want nil when an explicit Settle probe is supplied", err)
	}
}

func TestValidateRejectsExpectationsWithNoFaults(t *testing.T) {
	s := promptScenario("Check namespace shop.")
	s.Faults = nil
	err := s.Validate()
	if err == nil || !strings.Contains(err.Error(), "nothing would break") {
		t.Fatalf("Validate() = %v; want a complaint about expectations without faults", err)
	}
}

// A control scenario legitimately has neither faults nor expectations. It is
// how the suite measures whether a subject invents problems, so validation
// must not require a fault.
func TestValidateAcceptsAControlWithNoFaults(t *testing.T) {
	s := Scenario{
		ID:     "s-control",
		Name:   "healthy",
		Prompt: "Check namespace shop and report what you find.",
		Source: SourcePackParity,
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() = %v; want nil for a healthy control", err)
	}
}

func TestValidateRejectsAnUnknownSource(t *testing.T) {
	s := promptScenario("Check namespace shop.")
	s.Source = "pack:invented"
	err := s.Validate()
	if err == nil || !strings.Contains(err.Error(), "known source") {
		t.Fatalf("Validate() = %v; want a complaint about the source", err)
	}
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	s := Scenario{
		ID:     "s-bad",
		Source: "nope",
		Expect: []ExpectedFinding{{}},
	}
	err := s.Validate()
	if err == nil {
		t.Fatal("Validate() = nil; want errors")
	}
	// Name, Source, no-faults, empty prompt, expectation Kind, expectation
	// Name: a loader that surfaced one at a time would take six runs to fix.
	for _, want := range []string{"Name is required", "known source", "nothing would break", "prompt", "has no Kind", "has no Name"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() = %q; missing %q", err, want)
		}
	}
}

func TestAControlMayNotExemptAnything(t *testing.T) {
	s := Scenario{
		ID: "c", Name: "healthy", Source: SourcePackParity,
		Prompt:   "Assess the health of the \"x\" namespace and report what you find.",
		AlsoTrue: []string{"CrashLoopBackOff"},
	}
	err := s.Validate()
	if err == nil || !strings.Contains(err.Error(), "control has AlsoTrue") {
		t.Errorf("Validate() = %v, want a complaint about a control with an exemption", err)
	}
}

func TestAnEmptyExemptionIsRejected(t *testing.T) {
	s := Scenario{
		ID: "s", Name: "x", Source: SourcePackParity,
		Prompt:   "Assess the health of the \"x\" namespace and report what you find.",
		Expect:   []ExpectedFinding{{Kind: "Pod", Name: "web"}},
		Faults:   []simian.FaultManifest{{Engine: "kube-state", ResourceKind: "ContainerExitLoop", Targets: []simian.TargetRef{{Namespace: "x"}}}},
		AlsoTrue: []string{"  "},
	}
	err := s.Validate()
	if err == nil || !strings.Contains(err.Error(), "AlsoTrue 0 is empty") {
		t.Errorf("Validate() = %v, want a complaint about the empty entry", err)
	}
}
