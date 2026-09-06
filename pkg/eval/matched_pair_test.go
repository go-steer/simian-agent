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

// The dataplane pack's matched pair, by ID.
const (
	pairNetwork = "dataplane-latency-not-saturation"
	pairCPU     = "dataplane-stress-real"
)

func dataplaneScenario(t *testing.T, id string) scenario.Scenario {
	t.Helper()
	pack := scenario.MustBuiltin(scenario.PackDataplane)
	s, ok := pack.ByID(id)
	if !ok {
		t.Fatalf("the dataplane pack has no scenario %q", id)
	}
	return s
}

// pairReport is one subject's answer, expressed as the report it would file
// about whichever half of the pair it was shown. Both halves have the same
// symptom, so a subject's answer is the same shape in both and only the
// namespace changes.
func pairReport(ns, calleeReason string, extra ...scenario.Finding) Run {
	findings := []scenario.Finding{{
		Kind:         "Pod",
		ResourceName: "upstream-7f9c4d",
		Reason:       calleeReason,
		Severity:     scenario.SeverityCritical,
		Namespace:    ns,
	}, {
		// Every subject in these tests finds the symptom. It is in object
		// status, it is identical across the pair, and it is not what is being
		// measured here.
		Kind:         "Deployment",
		ResourceName: "edge",
		Reason:       "ReadinessProbeFailed",
		Severity:     scenario.SeverityCritical,
		Namespace:    ns,
	}}
	return Run{
		ScenarioID: "pair",
		Manifested: true,
		Report:     &Report{Findings: append(findings, extra...), OverallSeverity: scenario.SeverityCritical},
	}
}

func scoreOf(t *testing.T, s scenario.Scenario, run Run, measure string) float64 {
	t.Helper()
	for _, sc := range ScoreRun(s, run) {
		if sc.Name == measure {
			return sc.Value
		}
	}
	t.Fatalf("no %q score in the run", measure)
	return 0
}

// This is the claim the dataplane pack is built on, and the reason its two
// halves ship together.
//
// Both scenarios present the same symptom — a caller with no ready replicas in
// front of a callee that is Ready and slow — and every field the API server
// records is identical between them. A subject that answers "the callee is
// saturated, scale it up" is right in one and wrong in the other, and a fixture
// set containing only one of them cannot tell that subject apart from one that
// measured before it answered.
//
// So the pack is only worth shipping if the two scenarios actually score
// differently for the same answer. That is asserted here rather than asserted
// in a README, because the mechanism it depends on is subtle: the two families
// are separate, "HighLatency" is in neither, and any of those could be undone
// by a plausible-looking edit to the family table.
func TestTheMatchedPairSeparatesSubjectsThatGuessFromSubjectsThatMeasured(t *testing.T) {
	network := dataplaneScenario(t, pairNetwork)
	cpu := dataplaneScenario(t, pairCPU)
	networkNS := network.Namespaces()[0]
	cpuNS := cpu.Namespaces()[0]

	// Every subject answers the same way in both halves except the last one,
	// which is the only one that looked at utilisation before answering.
	cases := []struct {
		subject string
		// what it says about the callee in each half
		onNetwork, onCPU string
		// recall it should score in each half
		wantRecallNetwork, wantRecallCPU float64
		// hallucination score: 1 means nothing it claimed was invented
		wantHonestNetwork, wantHonestCPU float64
	}{{
		// The answer #67 names. Right about the CPU scenario, wrong about the
		// network one, and charged for the invention there.
		subject:           "scale it up",
		onNetwork:         "CPUSaturation",
		onCPU:             "CPUSaturation",
		wantRecallNetwork: 0.5, // the symptom only
		wantRecallCPU:     1,
		wantHonestNetwork: 0, // the one concrete claim it made was invented
		wantHonestCPU:     1,
	}, {
		// The mirror image. A subject that blames the network everywhere.
		subject:           "blame the network",
		onNetwork:         "NetworkLatency",
		onCPU:             "NetworkLatency",
		wantRecallNetwork: 1,
		wantRecallCPU:     0.5,
		wantHonestNetwork: 1,
		wantHonestCPU:     0,
	}, {
		// The honest under-answer: reports the slowness and attributes nothing.
		// It should score the symptom in both and be charged in neither —
		// "HighLatency" is true of both clusters and is a claim about neither
		// cause.
		subject:           "it is slow",
		onNetwork:         "HighLatency",
		onCPU:             "HighLatency",
		wantRecallNetwork: 0.5,
		wantRecallCPU:     0.5,
		wantHonestNetwork: 1,
		wantHonestCPU:     1,
	}, {
		// The subject that measured.
		subject:           "measured before answering",
		onNetwork:         "NetworkLatency",
		onCPU:             "CPUSaturation",
		wantRecallNetwork: 1,
		wantRecallCPU:     1,
		wantHonestNetwork: 1,
		wantHonestCPU:     1,
	}}

	for _, tc := range cases {
		t.Run(tc.subject, func(t *testing.T) {
			gotRecallNetwork := scoreOf(t, network, pairReport(networkNS, tc.onNetwork), MeasureRecall)
			gotRecallCPU := scoreOf(t, cpu, pairReport(cpuNS, tc.onCPU), MeasureRecall)
			gotHonestNetwork := scoreOf(t, network, pairReport(networkNS, tc.onNetwork), MeasureHallucination)
			gotHonestCPU := scoreOf(t, cpu, pairReport(cpuNS, tc.onCPU), MeasureHallucination)

			approx(t, gotRecallNetwork, tc.wantRecallNetwork)
			approx(t, gotRecallCPU, tc.wantRecallCPU)
			approx(t, gotHonestNetwork, tc.wantHonestNetwork)
			approx(t, gotHonestCPU, tc.wantHonestCPU)
		})
	}
}

// The other half of the same claim: the pair has to be *hard* in the same way,
// or the difference in scores is a difference in difficulty.
//
// A subject that reads object status perfectly and stops there scores exactly
// the same in both halves. Everything above that line is what the pair
// measures; everything below it is shared.
func TestTheSymptomIsWorthTheSameInBothHalvesOfThePair(t *testing.T) {
	network := dataplaneScenario(t, pairNetwork)
	cpu := dataplaneScenario(t, pairCPU)

	statusOnly := func(ns string) Run {
		return Run{
			ScenarioID: "pair",
			Manifested: true,
			Report: &Report{
				Findings: []scenario.Finding{{
					Kind:         "Deployment",
					ResourceName: "edge",
					Reason:       "NotReady",
					Severity:     scenario.SeverityCritical,
					Namespace:    ns,
				}},
				OverallSeverity: scenario.SeverityCritical,
			},
		}
	}

	for _, m := range []string{MeasureRecall, MeasureRootCause, MeasureHallucination, MeasureSeverity} {
		gotNetwork := scoreOf(t, network, statusOnly(network.Namespaces()[0]), m)
		gotCPU := scoreOf(t, cpu, statusOnly(cpu.Namespaces()[0]), m)
		if gotNetwork != gotCPU {
			t.Errorf("%s: a status-only report scores %v on %s and %v on %s; the shared symptom must be worth the same in both",
				m, gotNetwork, network.Name, gotCPU, cpu.Name)
		}
	}
}

// The pair only measures a cause if neither half licenses the other's answer.
//
// AlsoTrue suppresses the hallucination charge, so a network token exempted in
// the CPU scenario — or the reverse — would turn the discrimination off without
// changing a single score in the tests above until a subject happened to
// exercise it.
func TestNeitherHalfOfThePairExemptsTheOthersDiagnosis(t *testing.T) {
	for _, tc := range []struct {
		id, mustNotExempt string
	}{
		{pairNetwork, "cpu-saturation"},
		{pairCPU, "network-degradation"},
	} {
		s := dataplaneScenario(t, tc.id)
		for _, r := range s.AlsoTrue {
			if familyOf(r) == tc.mustNotExempt {
				t.Errorf("scenario %q exempts %q, which is family %q — the other half's diagnosis", s.ID, r, tc.mustNotExempt)
			}
		}
		for _, e := range s.Expect {
			for _, r := range e.Reasons {
				if familyOf(r) == tc.mustNotExempt {
					t.Errorf("scenario %q credits %q, which is family %q — the other half's diagnosis", s.ID, r, tc.mustNotExempt)
				}
			}
		}
	}
}
