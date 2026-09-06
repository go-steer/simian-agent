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
	"fmt"
	"strings"
)

// failureFamilies groups the reason tokens that name the same concrete
// failure mode. Only a reason in this table can be hallucinated; a reason
// outside it is a configuration advisory, which this measure has no business
// grading.
//
// Membership has one requirement: **the token must assert a cause.** The test
// is to ask what else the control plane writes it for. A token the API server
// emits for four unrelated situations names no cause, so a subject that uses
// it has claimed nothing that can be false.
//
// This vocabulary and its exclusions are ported from the agent-side rig,
// where most of the exclusions were each paid for with a live run that scored
// an honest report as an invention. They are reproduced rather than
// re-derived, because the numbers are meant to be comparable and because the
// mistakes are not worth making twice. Kept alongside is the rule that
// produced them, so a new entry can be judged the same way.
var failureFamilies = map[string][]string{
	"image-pull": {"ImagePullBackOff", "ErrImagePull", "InvalidImageName", "ImagePullError"},
	"crash-loop": {"CrashLoopBackOff", "CrashLooping"},
	"oom":        {"OOMKilled", "OutOfMemory", "MemoryLimitExceeded"},

	// Named for what it claims, not for the pod condition it once included.
	// Every token here asserts the node ran out of something, which is a claim
	// that can be false. "Unschedulable" was in this family and is now generic
	// — see genericReasons.
	"resource-pressure": {"InsufficientCPU", "InsufficientMemory", "InsufficientResources"},

	// "DeadlineExceeded" is deliberately absent. A Job that outruns
	// activeDeadlineSeconds really does get that token, so it reads like a
	// legitimate member — but it is a substring of "ProgressDeadlineExceeded",
	// which is what the Deployment controller writes on a rollout that cannot
	// progress. Under the old substring matcher every stuck rollout was
	// charged here as an invented job failure. familyOf matches exactly now,
	// so that particular trap is disarmed, but the token still fails the
	// name-a-cause test on its own terms: a subject writing the bare word
	// about a stalled rollout is not making a claim about a Job.
	"job-failed":   {"BackoffLimitExceeded", "JobFailed"},
	"no-endpoints": {"NoEndpoints", "SelectorMismatch", "NoMatchingPods", "EmptyEndpoints", "ServiceHasNoEndpoints"},

	// The workload is fine and something it calls is not. Every token names
	// that relationship, which is what makes the claim falsifiable: a subject
	// saying "upstream timeout" about a crash-looping pod has got it wrong, and
	// should be charged.
	//
	// "Timeout" and "DeadlineExceeded" are deliberately absent, and
	// "ContextDeadlineExceeded" is deliberately present. The bare words are
	// what a pod's own liveness probe failure says, what a slow image pull
	// says, and what an API call from the subject's own tooling says; only the
	// full spelling is a claim about a call this workload made.
	"dependency": {
		"UpstreamTimeout", "UpstreamUnavailable", "UpstreamError",
		"DependencyFailure", "DependencyTimeout", "DependencyUnavailable",
		"ConnectionRefused", "ContextDeadlineExceeded",
	},

	// "NotReady" is deliberately absent. Kubernetes writes Ready=False on
	// pods, containers and nodes alike, so the bare token names a state and
	// not a node. Both survivors name their own subject.
	"node-down":      {"NodeNotReady", "NodeUnreachable"},
	"disk":           {"DiskPressure", "Evicted", "VolumeMountFailed", "FailedMount"},
	"volume-binding": {"Unbound", "UnboundImmediatePersistentVolumeClaims", "VolumeBindingFailed", "ProvisioningFailed", "StorageClassNotFound", "NoStorageClass", "MissingStorageClass"},

	// A deploy that did not land. "ProgressDeadlineExceeded" is the token the
	// Deployment controller actually writes, and the family exists mostly to
	// hold it: it used to fall through to no family at all, which meant a
	// subject could call any fault a stuck rollout for free.
	//
	// "Progressing" is deliberately absent. It is a condition type that is
	// True on every healthy rollout, so it names no failure. So is
	// "Degraded", which belongs to Argo and OpenShift rather than to
	// Kubernetes and is written for half a dozen unrelated situations.
	"rollout": {
		"ProgressDeadlineExceeded", "RolloutStuck", "StuckRollout",
		"FailedRollout", "RolloutStalled", "DeploymentStuck", "BadDeploy",
	},

	// Eviction blocked by a budget with no headroom. Every token names the
	// budget as the thing in the way, which is the claim: a subject saying
	// "PDB blocked" about a node that was merely slow to drain has got it
	// wrong.
	//
	// "DrainBlocked" is the loosest of these and is still in, because a drain
	// is blocked by remarkably few things and a disruption budget is the first
	// of them. "Blocked" and "Stuck" on their own are not, for the usual
	// reason: they name a state and no cause.
	"disruption-budget": {
		"PDBGridlock", "PDBBlocked", "DisruptionsNotAllowed",
		"DisruptionBudgetBlocked", "DrainBlocked",
	},

	// A certificate past — or nearly past — its notAfter. Kubernetes writes
	// none of these itself, which is exactly why the family is needed: the
	// tokens come from the subject's own vocabulary, so without a family they
	// would be ungradeable, and a report could claim an expiring certificate
	// about any fault at all at no cost.
	//
	// "Expired" and "Expiring" alone are absent. A lease expires, a lock
	// expires, a token expires; only the spellings that name a certificate
	// make a claim about one.
	"cert-expiry": {
		"CertificateExpired", "CertificateExpiring", "CertExpired",
		"CertExpiring", "ExpiredCertificate", "TLSCertificateExpiry",
	},
}

// genericReasons name *that* something failed without naming *how*.
//
// They are perfectly good words for a report to use — an expectation may list
// them in Reasons and recall accepts them — but they can never be
// hallucinated, because they attribute nothing to attribute wrongly.
//
// This is an explicit set rather than mere absence from failureFamilies
// because the two tables make contradictory claims about a token they share —
// the family says it names a cause, this says it does not — and a
// contradiction should be a crash rather than a preference. invertFamilies
// enforces that, which is also what keeps the exclusions below from being
// quietly undone by someone adding a plausible-looking token to a family.
//
// FailedScheduling and Unschedulable are here despite Simian having a fault
// kind called Unschedulable, and that is the point. The scheduler writes both
// for unbound volumes, taints, node selectors and genuine resource shortage
// alike, so they say scheduling failed and not why. A subject that reports a
// pod blocked behind a PVC using the literal token the cluster itself wrote
// must not be charged with inventing a resource shortage.
var genericReasons = map[string]bool{
	"pending":          true,
	"failed":           true,
	"error":            true,
	"unhealthy":        true,
	"failedscheduling": true,
	"unschedulable":    true,
}

// normReason folds a reason the way scenario.ExpectedFinding.MatchesReason
// does, so the generic-token check and the family lookup agree on what one
// token is.
func normReason(s string) string {
	return strings.ToLower(strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.TrimSpace(s)))
}

// familyByReason inverts failureFamilies onto the normalized token, which
// makes familyOf a lookup rather than a scan. Built once, and the two ways the
// tables can contradict themselves — a token in two families, or a token that
// is both a family member and generic — are programming errors caught at init
// rather than silent dependences on map iteration order.
var familyByReason = invertFamilies(failureFamilies)

func invertFamilies(families map[string][]string) map[string]string {
	out := make(map[string]string)
	for family, reasons := range families {
		for _, r := range reasons {
			norm := normReason(r)
			if genericReasons[norm] {
				panic(fmt.Sprintf("eval: reason %q is in family %q and in genericReasons; it cannot both name a cause and not", r, family))
			}
			if prev, dup := out[norm]; dup {
				panic(fmt.Sprintf("eval: reason %q is in both %q and %q", r, prev, family))
			}
			out[norm] = family
		}
	}
	return out
}

// familyOf returns the failure family a reason belongs to, or "".
//
// The match is on the exact normalized token — case, underscores, hyphens and
// spaces folded, and nothing else. Not a substring match, in either
// direction, and the difference is not academic.
//
// The lenient bidirectional matcher is right for an expectation, where the
// author lists the spellings of one answer and "CrashLoop" and
// "CrashLoopBackOff" are the same answer. It is wrong here, whose argument is
// whatever the *subject* chose to write: under it a bare family member
// annexes every longer token containing it, and an honest report is scored as
// an invention. "Failed" claimed FailedMount for the disk family; "Error"
// claimed ImagePullError; "NotReady" claimed PodsNotReady.
//
// Exact matching is one-directional in the direction that matters here: a
// token that no longer resolves is a concrete claim no longer *credited*,
// which can only fail to charge a misdiagnosis, never invent one.
//
// It is not one-directional in the other use. HallucinatedFault.Score calls
// this twice in opposite directions — once over a scenario's own expectation
// reasons to learn which families were injected, and once over the subject's
// findings. An expectation reason that stops resolving drops a family out of
// `injected`, and then a correct report about the scenario's own fault is
// charged as an invention. TestEveryFaultKindNamesItsOwnFamily is what keeps
// that from happening as fault kinds are added.
// A generic token needs no test here: invertFamilies has already refused to
// let one into the table, so the lookup misses and the answer is "".
func familyOf(reason string) string {
	return familyByReason[normReason(reason)]
}
