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

package eval

import (
	"testing"

	"github.com/go-steer/simian-agent/pkg/scenario"
)

// The dataplane pack's other two faults, by ID.
const (
	abort503  = "dataplane-abort-503-not-a-bug"
	partition = "dataplane-partition-one-way"
)

// oneFinding is a report that names a single object and a single reason. Most
// of the claims below are about which object a subject blames, so the reports
// are deliberately minimal: anything else in them would score somewhere and
// blur the thing being asserted.
func oneFinding(ns, kind, name, reason string) Run {
	return Run{
		ScenarioID: "dataplane",
		Manifested: true,
		Report: &Report{
			Findings: []scenario.Finding{{
				Kind:         kind,
				ResourceName: name,
				Reason:       reason,
				Severity:     scenario.SeverityCritical,
				Namespace:    ns,
			}},
			OverallSeverity: scenario.SeverityCritical,
		},
	}
}

// abort-503-not-a-bug is graded on attribution, and this is that claim.
//
// Two subjects observe exactly the same thing and write exactly the same word.
// One says the caller is returning 503s, which is true and is the trap; the
// other says the callee is, which is also true and is the answer. Nothing in
// the vocabulary separates them — a status code names no cause — so if the
// scenario measures anything at all, it measures the object.
func TestTheFiveHundredThreeScenarioIsGradedOnWhichServiceIsBlamed(t *testing.T) {
	s := dataplaneScenario(t, abort503)
	ns := s.Namespaces()[0]

	blamedCaller := oneFinding(ns, "Deployment", "edge", "HTTP503")
	blamedCallee := oneFinding(ns, "Pod", "upstream-7f9c4d", "HTTP503")

	if got := scoreOf(t, s, blamedCaller, MeasureRootCause); got != 0 {
		t.Errorf("blaming the caller scored %v on %s; the caller is the symptom, and calling it the root is the whole trap", got, MeasureRootCause)
	}
	if got := scoreOf(t, s, blamedCallee, MeasureRootCause); got != 1 {
		t.Errorf("blaming the callee scored %v on %s; it is the object that changed", got, MeasureRootCause)
	}

	// And neither is charged. The 5xx words are observations, so a subject that
	// got the attribution wrong loses the root and keeps its honesty score —
	// which is right, because it invented nothing. See genericReasons.
	for name, run := range map[string]Run{"caller": blamedCaller, "callee": blamedCallee} {
		if got := scoreOf(t, s, run, MeasureHallucination); got != 1 {
			t.Errorf("blaming the %s scored %v on %s; an HTTP status names no cause and cannot be an invention", name, got, MeasureHallucination)
		}
	}
}

// A subject that arrives at the 5xx scenario carrying the matched pair's
// answers is charged for them. Without this the pack's three critical
// scenarios could be passed with one guess.
func TestTheFiveHundredThreeScenarioChargesThePairsAnswers(t *testing.T) {
	s := dataplaneScenario(t, abort503)
	ns := s.Namespaces()[0]

	for _, reason := range []string{"CPUSaturation", "NetworkLatency", "NetworkPartition"} {
		run := oneFinding(ns, "Pod", "upstream-7f9c4d", reason)
		if got := scoreOf(t, s, run, MeasureHallucination); got != 0 {
			t.Errorf("reason %q scored %v on %s; nothing in this scenario is slow, saturated or severed", reason, got, MeasureHallucination)
		}
	}
}

// partition-one-way is graded on the vocabulary, and the interesting cases are
// the three shades of "something is wrong with the network".
func TestThePartitionSeparatesSeveredFromSlow(t *testing.T) {
	s := dataplaneScenario(t, partition)
	ns := s.Namespaces()[0]

	for _, tc := range []struct {
		reason               string
		wantRoot, wantHonest float64
		why                  string
	}{{
		reason: "NetworkPartition", wantRoot: 1, wantHonest: 1,
		why: "the answer",
	}, {
		// The closest wrong answer, and the one the scenario's also_true was
		// written to keep chargeable. A partition is total loss, so this is a
		// defensible thing to say — but also_true exempts by family, and
		// exempting it would license "the link is congested" against a link
		// carrying nothing. See the comment on 05's also_true.
		reason: "PacketLoss", wantRoot: 0, wantHonest: 0,
		why: "defensible, and still not what a severed link is",
	}, {
		// Slowness is generic everywhere in this pack, including here.
		reason: "HighLatency", wantRoot: 0, wantHonest: 1,
		why: "an observation, and not one this scenario credits",
	}, {
		// The link is not congested. It is carrying nothing.
		reason: "NetworkCongestion", wantRoot: 0, wantHonest: 0,
		why: "network-degradation is not injected here and is not exempted by name",
	}, {
		reason: "CPUSaturation", wantRoot: 0, wantHonest: 0,
		why: "the pair's other answer, and wrong here too",
	}} {
		t.Run(tc.reason, func(t *testing.T) {
			run := oneFinding(ns, "Service", "upstream", tc.reason)
			approx(t, scoreOf(t, s, run, MeasureRootCause), tc.wantRoot)
			approx(t, scoreOf(t, s, run, MeasureHallucination), tc.wantHonest)
		})
	}
}

// The pack ships two NetworkChaos scenarios, and they are only two scenarios
// if a correct answer to one is a wrong answer to the other.
//
// A slow link and a severed one are the easiest pair in the pack to collapse
// by accident, because every honest word about either of them starts with
// "network". The guard is that neither file credits or exempts the other's
// family — and since also_true works by family, one plausible-looking
// exemption is all it would take.
func TestTheTwoNetworkScenariosDoNotLicenseEachOthersDiagnosis(t *testing.T) {
	for _, tc := range []struct {
		id, mustNotAccept string
	}{
		{pairNetwork, "network-partition"},
		{partition, "network-degradation"},
	} {
		s := dataplaneScenario(t, tc.id)
		for _, r := range append(append([]string{}, s.AlsoTrue...), reasonsOf(s)...) {
			if familyOf(r) == tc.mustNotAccept {
				t.Errorf("scenario %q accepts %q, which is family %q — the other network scenario's diagnosis", s.ID, r, tc.mustNotAccept)
			}
		}
	}
}

func reasonsOf(s scenario.Scenario) []string {
	var out []string
	for _, e := range s.Expect {
		out = append(out, e.Reasons...)
	}
	return out
}
