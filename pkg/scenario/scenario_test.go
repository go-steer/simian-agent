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

import "testing"

// TestExpectedFindingMatchingIsParityWithTheAgentRig is the load-bearing test
// of this package.
//
// This table is ported case-for-case from the agent-side live eval tier
// (core-sre-agent/internal/faults/inject_test.go, TestWantMatching). The two
// rigs exist to produce comparable numbers; if this matcher drifts from that
// one, a recall score from Simian and a recall score from the agent's own
// suite stop meaning the same thing, and nobody finds out from the numbers.
//
// If a case here is changed, the corresponding case there must change too.
func TestExpectedFindingMatchingIsParityWithTheAgentRig(t *testing.T) {
	e := ExpectedFinding{
		Kind:            "Pod",
		Name:            "checkout-api",
		AlsoAcceptKinds: []string{"Deployment"},
		Reasons:         []string{"ImagePullBackOff", "ErrImagePull"},
		MinSeverity:     SeverityWarning,
	}

	for _, tc := range []struct {
		name string
		f    Finding
		want bool
	}{
		{"exact", Finding{Kind: "Pod", ResourceName: "checkout-api", Reason: "ImagePullBackOff", Severity: SeverityCritical}, true},
		{"generated pod suffix", Finding{Kind: "Pod", ResourceName: "checkout-api-7d9f4b6c8-x2k9", Reason: "ErrImagePull", Severity: SeverityWarning}, true},
		{"controller instead of pod", Finding{Kind: "Deployment", ResourceName: "checkout-api", Reason: "ImagePullBackOff", Severity: SeverityWarning}, true},
		{"lowercase kind", Finding{Kind: "pod", ResourceName: "checkout-api", Reason: "imagepullbackoff", Severity: SeverityWarning}, true},
		{"snake reason", Finding{Kind: "Pod", ResourceName: "checkout-api", Reason: "image_pull_back_off", Severity: SeverityWarning}, true},

		{"wrong workload", Finding{Kind: "Pod", ResourceName: "payments-worker", Reason: "ImagePullBackOff", Severity: SeverityCritical}, false},
		{"wrong failure mode", Finding{Kind: "Pod", ResourceName: "checkout-api", Reason: "CrashLoopBackOff", Severity: SeverityCritical}, false},
		{"unrelated kind", Finding{Kind: "Node", ResourceName: "checkout-api", Reason: "ImagePullBackOff", Severity: SeverityCritical}, false},
		{"too mild", Finding{Kind: "Pod", ResourceName: "checkout-api", Reason: "ImagePullBackOff", Severity: SeverityInfo}, false},
		{"no resource named", Finding{Kind: "Pod", Reason: "ImagePullBackOff", Severity: SeverityCritical}, false},

		// A prefix match must not run the other way. "checkout" is a
		// different, shorter name, and crediting it would let a subject score
		// by naming the namespace's common prefix instead of the workload.
		{"prefix in reverse", Finding{Kind: "Pod", ResourceName: "checkout", Reason: "ImagePullBackOff", Severity: SeverityCritical}, false},
		// A sibling that merely shares a prefix is a different object.
		{"sibling workload", Finding{Kind: "Pod", ResourceName: "checkout-apiserver", Reason: "ImagePullBackOff", Severity: SeverityCritical}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := e.Matches(tc.f); got != tc.want {
				t.Errorf("Matches(%+v) = %v, want %v", tc.f, got, tc.want)
			}
		})
	}
}

func TestEmptyReasonsMeansTheReasonIsNotGraded(t *testing.T) {
	e := ExpectedFinding{Kind: "Pod", Name: "web"}
	for _, reason := range []string{"anything", "", "CrashLoopBackOff"} {
		if !e.Matches(Finding{Kind: "Pod", ResourceName: "web", Reason: reason}) {
			t.Errorf("reason %q should be ungraded when Reasons is empty", reason)
		}
	}
}

func TestEmptyMinSeverityMeansSeverityIsNotGraded(t *testing.T) {
	e := ExpectedFinding{Kind: "Pod", Name: "web"}
	if !e.Matches(Finding{Kind: "Pod", ResourceName: "web", Severity: SeverityInfo}) {
		t.Error("info severity should match when MinSeverity is empty")
	}
	// Even a severity we cannot parse must not fail an ungraded expectation.
	if !e.Matches(Finding{Kind: "Pod", ResourceName: "web", Severity: "banana"}) {
		t.Error("unparseable severity should match when MinSeverity is empty")
	}
}

// An unrecognised severity must never satisfy a MinSeverity gate. Ranking it
// at zero is what makes that true; if unknown severities sorted high, a
// subject could clear every severity gate by emitting garbage.
func TestUnknownSeverityNeverSatisfiesAGate(t *testing.T) {
	e := ExpectedFinding{Kind: "Pod", Name: "web", MinSeverity: SeverityInfo}
	if e.Matches(Finding{Kind: "Pod", ResourceName: "web", Severity: "banana"}) {
		t.Error("an unparseable severity satisfied a MinSeverity gate")
	}
}

func TestSeverityOrdering(t *testing.T) {
	if !SeverityCritical.AtLeast(SeverityWarning) {
		t.Error("critical should be at least warning")
	}
	if SeverityInfo.AtLeast(SeverityWarning) {
		t.Error("info should not be at least warning")
	}
	if !SeverityWarning.AtLeast(SeverityWarning) {
		t.Error("AtLeast should be inclusive")
	}
	if !SeverityOK.AtLeast(SeverityOK) {
		t.Error("ok should be at least ok")
	}
}

func TestParseSeverity(t *testing.T) {
	// A slice rather than a map because one case is deliberately surrounded by
	// whitespace — reports arrive from more than one producer and not all of
	// them trim.
	for _, tc := range []struct {
		in   string
		want Severity
	}{
		{"critical", SeverityCritical},
		{"CRITICAL", SeverityCritical},
		{"[CRITICAL]", SeverityCritical},
		{"CRITICAL:", SeverityCritical},
		{"  warning ", SeverityWarning},
		{"ok", SeverityOK},
	} {
		in, want := tc.in, tc.want
		got, ok := ParseSeverity(in)
		if !ok || got != want {
			t.Errorf("ParseSeverity(%q) = %q, %v; want %q, true", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "fatal", "sev1", "[]"} {
		if got, ok := ParseSeverity(in); ok {
			t.Errorf("ParseSeverity(%q) = %q, true; want not ok", in, got)
		}
	}
}

func TestRootsAndControls(t *testing.T) {
	s := Scenario{Expect: []ExpectedFinding{
		{Kind: "Pod", Name: "db", Root: true},
		{Kind: "Pod", Name: "api"},
	}}
	roots := s.Roots()
	if len(roots) != 1 || roots[0].Name != "db" {
		t.Errorf("Roots() = %+v, want just db", roots)
	}
	if s.IsControl() {
		t.Error("a scenario with expectations is not a control")
	}
	if !(Scenario{}).IsControl() {
		t.Error("a scenario with no expectations is a control")
	}
}
