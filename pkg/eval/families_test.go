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
	"slices"
	"strings"
	"testing"

	"github.com/go-steer/simian-agent/pkg/driver/kubestate"
	"github.com/go-steer/simian-agent/pkg/scenario"
)

// kindVocabulary is what a correct report about one fault kind may say.
//
// reasons are the tokens a subject could honestly write about a pod broken
// that way — the ones the control plane puts on the object, plus the ones an
// operator would use for the states it passes through. families are the
// failure families those tokens resolve into, and therefore the families a
// scenario using this kind has to make reachable from its own expectation
// Reasons.
type kindVocabulary struct {
	families []string
	reasons  []string
}

// faultKindReports is the checklist behind TestEveryFaultKindNamesItsOwnFamily.
//
// Kube-state kinds are the ones the first loop below *requires* an entry for,
// because the engine enumerates them and a new one must be classified before
// it can be scored. Chaos Mesh has no such enumeration here, and for a long
// time this table said dataplane faults needed no entry at all: they manifest
// as latency rather than as a reason token on an object, so there was nothing
// for familyOf to resolve and nothing to charge.
//
// That stopped being true with the dataplane pack. Its whole premise is that
// "the path is slow" and "the callee is out of CPU" are distinguishable
// claims, which means both had to become resolvable families, which means both
// can now be charged — including against a subject that got it right. The two
// kinds are listed for the same reason every other entry is.
//
// The entries with more than one family are the interesting ones, and they are
// not redundancy. A memory-squeezed container really does enter
// CrashLoopBackOff between OOM kills, and an unschedulable pod really is
// rejected for insufficient memory, so both readings are correct reports about
// one fault. A scenario for those kinds must list both spellings or a subject
// gets charged for the true one.
var faultKindReports = map[string]kindVocabulary{
	kubestate.KindImageUnresolvable: {
		families: []string{"image-pull"},
		reasons:  []string{"ImagePullBackOff", "ErrImagePull", "InvalidImageName", "Pending", "Failed"},
	},
	kubestate.KindContainerExitLoop: {
		families: []string{"crash-loop"},
		// "Error" is the token the gate asserts on — the kubelet's reason for
		// a non-zero exit. It is generic, deliberately: it says a container
		// died and not why.
		reasons: []string{"CrashLoopBackOff", "CrashLooping", "Error", "Failed"},
	},
	kubestate.KindMemoryLimitSqueeze: {
		families: []string{"oom", "crash-loop"},
		reasons:  []string{"OOMKilled", "OutOfMemory", "CrashLoopBackOff", "Error"},
	},
	kubestate.KindUnschedulable: {
		families: []string{"resource-pressure"},
		reasons:  []string{"Unschedulable", "FailedScheduling", "InsufficientMemory", "InsufficientCPU", "Pending"},
	},
	kubestate.KindJobFailure: {
		families: []string{"job-failed"},
		// "Error" is what the kubelet writes on each failed attempt, and
		// "Failed" is the Job's own condition type. Both are generic, and both
		// are what an honest report about this fault reaches for first.
		reasons: []string{"BackoffLimitExceeded", "JobFailed", "Error", "Failed"},
	},
	kubestate.KindSelectorDrift: {
		families: []string{"no-endpoints"},
		// No pod-level token here, and that is the fault: every pod is Running
		// and Ready. A subject with nothing to say beyond the pods has not
		// found this one.
		reasons: []string{"NoEndpoints", "SelectorMismatch", "NoMatchingPods", "EmptyEndpoints", "ServiceHasNoEndpoints"},
	},
	kubestate.KindBackendCrashLoop: {
		families: []string{"crash-loop", "no-endpoints"},
		// Two families, and unlike RolloutStuck's pair neither is the whole
		// answer on its own. The pods really are crash-looping and the Service
		// really has no ready backend; a report naming only the Service has
		// found what a user noticed, and a report naming only the pods has not
		// said what broke. Both spellings have to be listed or the true half a
		// subject did report gets charged as an invention.
		reasons: []string{
			"CrashLoopBackOff", "CrashLooping", "Error", "Failed",
			"NoReadyEndpoints", "NoHealthyBackends", "NoEndpoints", "ServiceHasNoEndpoints",
		},
	},
	kubestate.KindUnboundClaim: {
		families: []string{"volume-binding"},
		// The scheduler's own tokens for a pod blocked behind a claim are
		// generic, deliberately — see genericReasons. Only a report that names
		// the binding is making a claim that could be wrong.
		reasons: []string{
			"Unbound", "UnboundImmediatePersistentVolumeClaims", "VolumeBindingFailed",
			"StorageClassNotFound", "MissingStorageClass",
			"Pending", "Unschedulable", "FailedScheduling",
		},
	},
	kubestate.KindDependencyStall: {
		families: []string{"dependency"},
		// Nothing the control plane wrote is in this list, because the control
		// plane wrote nothing: the pods are Ready, the Service is endpointed,
		// no event fired. Every token here is one a subject can only produce by
		// having read the log, which is exactly the discrimination this kind
		// exists to make.
		reasons: []string{
			"UpstreamTimeout", "UpstreamUnavailable", "DependencyFailure",
			"ConnectionRefused", "ContextDeadlineExceeded",
		},
	},
	kubestate.KindPDBGridlock: {
		families: []string{"disruption-budget"},
		// Like SelectorDrift, no pod-level token: every pod is Running and
		// Ready and the Deployment is Available. Unlike SelectorDrift, there is
		// no object in a failed state at all — the budget is doing exactly what
		// it says. A report has to name the budget as the obstruction, which is
		// what every token here does.
		reasons: []string{
			"PDBGridlock", "PDBBlocked", "DisruptionsNotAllowed",
			"DisruptionBudgetBlocked", "DrainBlocked",
		},
	},
	kubestate.KindRolloutStuck: {
		families: []string{"rollout", "crash-loop"},
		// Two families, and both are correct readings. The surge pod really is
		// in CrashLoopBackOff, so a subject that reports one has not invented
		// anything — but a subject that reports *only* that has found the
		// symptom and missed that a deploy never landed.
		reasons: []string{
			"ProgressDeadlineExceeded", "RolloutStuck", "FailedRollout",
			"CrashLoopBackOff", "Error", "Failed",
		},
	},
	kubestate.KindCertExpiry: {
		families: []string{"cert-expiry"},
		// Nothing the control plane wrote, for the same reason as
		// DependencyStall: it wrote nothing. Kubernetes has no opinion about
		// the contents of a Secret, so every token here is one the subject can
		// only produce by having decoded the certificate and looked at the
		// date.
		reasons: []string{
			"CertificateExpired", "CertificateExpiring", "CertExpired",
			"ExpiredCertificate", "TLSCertificateExpiry",
		},
	},
	// The control, and the only entry that is empty on purpose. There is
	// nothing wrong in the namespace, so there is no token a correct report
	// contains — a report about NoOp is the empty one, which is what
	// HallucinatedFault grades and what recall has nothing to ask for.
	kubestate.KindNoOp: {},

	// The dataplane pair. Both vocabularies include the generic slowness words
	// on purpose: they are what a subject writes when it has observed the
	// symptom and attributed nothing, they resolve to no family, and the test
	// below therefore asserts that writing one is never charged — in either
	// scenario. That is the property the pair depends on.
	//
	// NetworkChaos names two families because the kind covers two incidents.
	// A delay degrades the path and a partition severs it, and the pack ships
	// both; a scenario using one action still has to list only its own
	// spellings, which is why 01 and 05 have disjoint expectation Reasons and
	// why 05 exempts PacketLoss by hand.
	"NetworkChaos": {
		families: []string{"network-degradation", "network-partition"},
		reasons: []string{
			"NetworkLatency", "NetworkDelay", "NetworkDegradation", "PacketDelay",
			"NetworkPartition", "NetworkUnreachable", "ConnectivityLoss", "Partitioned",
			"HighLatency", "Latency",
		},
	},
	"StressChaos": {
		families: []string{"cpu-saturation"},
		reasons: []string{
			"CPUSaturation", "CPUThrottling", "CPUThrottled", "CPUExhaustion",
			"CPUStarvation", "HighCPU",
			"HighLatency", "Latency",
		},
	},
	// The 5xx scenario, and the one entry whose interesting tokens are all
	// generic. A status code says what the caller saw and not why, so the only
	// family a correct report about a synthesized 503 resolves to is the one
	// the *caller* is entitled to name — its dependency is returning errors.
	// Everything else here is creditable and uncharged everywhere, which is
	// deliberate: that scenario is graded on which object it calls the root.
	"HTTPChaos": {
		families: []string{"dependency"},
		reasons: []string{
			"UpstreamError", "UpstreamUnavailable",
			"HTTP503", "ServiceUnavailable", "Error5xx", "ServerError",
		},
	},
}

// TestEveryFaultKindNamesItsOwnFamily is the guard familyOf's doc comment
// names.
//
// HallucinatedFault resolves reasons in two directions: over a scenario's
// expectations, to learn which families were injected, and over the subject's
// findings, to see what it claimed. The two have to agree. If a token drops
// out of failureFamilies, or is moved, or a new fault kind arrives whose
// reason resolves nowhere, the family falls out of `injected` and a correct
// report about Simian's own fault is charged as an invention. That is the
// worst failure this package has: a confidently wrong zero against a subject
// that got the answer right.
func TestEveryFaultKindNamesItsOwnFamily(t *testing.T) {
	// A new fault kind has to be classified here before it can be scored.
	for _, kind := range kubestate.Kinds() {
		if _, ok := faultKindReports[kind]; !ok {
			t.Errorf("fault kind %q has no entry in faultKindReports; what does a correct report about it say?", kind)
		}
	}

	for kind, vocab := range faultKindReports {
		t.Run(kind, func(t *testing.T) {
			for _, fam := range vocab.families {
				if _, ok := failureFamilies[fam]; !ok {
					t.Errorf("family %q is not in failureFamilies", fam)
				}
			}

			seen := map[string]bool{}
			for _, r := range vocab.reasons {
				fam := familyOf(r)
				if fam == "" {
					// Generic, so unresolvable in both directions and safe:
					// it can neither be credited nor charged.
					continue
				}
				if !slices.Contains(vocab.families, fam) {
					t.Errorf("reason %q resolves to family %q, which a correct report about %s does not name", r, fam, kind)
				}
				seen[fam] = true
			}

			for _, fam := range vocab.families {
				if !seen[fam] {
					t.Errorf("family %q is declared for %s but no listed reason resolves to it", fam, kind)
				}
			}
		})
	}
}

// The end-to-end version of the same invariant, through the measure that
// depends on it: a scenario that lists a kind's vocabulary in its expectations
// charges nothing for a report using any token from it.
func TestACorrectReportAboutAnInjectedKindIsNeverCharged(t *testing.T) {
	for kind, vocab := range faultKindReports {
		t.Run(kind, func(t *testing.T) {
			s := testScenario(scenario.ExpectedFinding{
				Kind:    "Pod",
				Name:    "workload",
				Reasons: vocab.reasons,
			})
			for _, r := range vocab.reasons {
				got := HallucinatedFault{}.Score(s, runWith(
					finding("Pod", "workload-abc", r, scenario.SeverityCritical),
				))
				if got.Value != 1 {
					t.Errorf("reason %q scored %v: %s", r, got.Value, got.Comment)
				}
			}
		})
	}
}

func TestFamilyOfMatchesTheExactToken(t *testing.T) {
	for _, tc := range []struct {
		reason string
		want   string
		why    string
	}{
		{"OOMKilled", "oom", "exact"},
		{"oomkilled", "oom", "case folded"},
		{"OOM_KILLED", "oom", "underscores folded"},
		{"oom-killed", "oom", "hyphens folded"},
		{"  OOMKilled  ", "oom", "trimmed"},

		// The four that were each paid for with a live run scored wrong.
		{"FailedMount", "disk", "a full member, not the bare word"},
		{"Failed", "", "generic; must not annex FailedMount"},
		{"ImagePullError", "image-pull", "a full member"},
		{"Error", "", "generic; must not annex ImagePullError"},
		{"PodsNotReady", "", "not a member, and NotReady must not annex it"},
		{"ProgressDeadlineExceeded", "rollout", "a stalled rollout is not a failed Job"},
		{"DeadlineExceeded", "", "generic; must not annex ProgressDeadlineExceeded"},

		{"Unschedulable", "", "the scheduler writes it for four unrelated causes"},
		{"FailedScheduling", "", "says scheduling failed, not why"},
		{"InsufficientMemory", "resource-pressure", "names a cause that can be false"},

		{"", "", "empty"},
		{"NoSuchReason", "", "unknown"},
		{"CrashLoop", "", "a prefix of a member is not the member"},
	} {
		if got := familyOf(tc.reason); got != tc.want {
			t.Errorf("familyOf(%q) = %q, want %q (%s)", tc.reason, got, tc.want, tc.why)
		}
	}
}

// Every generic token has to stay out of every family, or it starts making
// claims on the family's behalf. The real table is built at init, so this
// passing means the package loaded without panicking; the case below is what
// proves the guard is armed.
func TestGenericReasonsAreInNoFamily(t *testing.T) {
	for generic := range genericReasons {
		if fam, ok := familyByReason[generic]; ok {
			t.Errorf("generic reason %q is also in family %q", generic, fam)
		}
	}
}

// The exclusions in genericReasons were each paid for with a live run scored
// wrong, and every one of them names a token that looks like it belongs in a
// family. Putting one back has to be a crash and not a silent regression.
func TestAGenericTokenInAFamilyPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("a generic token in a family did not panic")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "Unschedulable") || !strings.Contains(msg, "genericReasons") {
			t.Errorf("panic = %q, want it to name the token and the contradiction", msg)
		}
	}()
	invertFamilies(map[string][]string{
		"resource-pressure": {"Unschedulable"},
	})
}

// Map iteration order decides which family wins a duplicated token, so a
// duplicate has to be a build-time crash and not a coin flip.
func TestADuplicatedReasonPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("a reason in two families did not panic")
		}
		msg, _ := r.(string)
		// Which of the two spellings is named depends on map iteration order;
		// both families always are.
		for _, want := range []string{"oom", "crash-loop", "is in both"} {
			if !strings.Contains(msg, want) {
				t.Errorf("panic = %q, want it to mention %q", msg, want)
			}
		}
	}()
	invertFamilies(map[string][]string{
		"oom":        {"OOMKilled"},
		"crash-loop": {"OOM_KILLED"},
	})
}

// normReason has to fold exactly the way scenario.ExpectedFinding does, or the
// generic check and the expectation matcher disagree about what one token is.
func TestNormReasonAgreesWithTheExpectationMatcher(t *testing.T) {
	for _, r := range []string{"OOMKilled", "OOM_KILLED", "oom-killed", " OOM KILLED "} {
		e := scenario.ExpectedFinding{Kind: "Pod", Name: "x", Reasons: []string{"OOMKilled"}}
		if !e.MatchesReason(r) {
			t.Errorf("expectation matcher rejects %q", r)
		}
		if familyOf(r) != "oom" {
			t.Errorf("familyOf(%q) = %q, want oom", r, familyOf(r))
		}
	}
}

// --- AlsoTrue ---

// TestBuiltinPacksExemptOnlyRealFailureFamilies is the reason LintAlsoTrue is
// exported. An entry that resolves to nothing fails silently — the author
// wrote it to license a claim, and the claim is charged anyway — so the only
// place it can be caught is a test that reads the packs that ship.
func TestBuiltinPacksExemptOnlyRealFailureFamilies(t *testing.T) {
	for _, name := range scenario.BuiltinPacks {
		for _, s := range scenario.MustBuiltin(name).Scenarios {
			if err := LintAlsoTrue(s); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
	}
}

func TestLintAlsoTrueRejectsAnExemptionThatWouldDoNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token string
		want  string
	}{
		{"unknown token", "TotallyMadeUp", "not in any failure family"},
		{"generic token", "Pending", "generic reason"},
		{"generic, differently spelled", "failed_scheduling", "generic reason"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := LintAlsoTrue(scenario.Scenario{ID: "x", AlsoTrue: []string{tc.token}})
			if err == nil {
				t.Fatalf("LintAlsoTrue(%q) = nil, want an error", tc.token)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("LintAlsoTrue(%q) = %v, want it to mention %q", tc.token, err, tc.want)
			}
		})
	}
}

func TestLintAlsoTrueAcceptsAFamilyMemberInAnySpelling(t *testing.T) {
	s := scenario.Scenario{ID: "x", AlsoTrue: []string{"CrashLoopBackOff", "crash-loop-backoff", "OOMKilled"}}
	if err := LintAlsoTrue(s); err != nil {
		t.Errorf("LintAlsoTrue() = %v, want nil", err)
	}
}
