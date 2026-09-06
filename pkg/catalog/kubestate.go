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

package catalog

import (
	"encoding/base64"
	"strings"
)

// Fault kinds synthesized by the kube-state engine.
//
// Spelled out here as literals rather than imported from
// pkg/driver/kubestate: that package imports this one for tier and gate
// lookup, so the dependency cannot run the other way. A test in the driver
// asserts the two lists agree.
const (
	KubeStateImageUnresolvable  = "ImageUnresolvable"
	KubeStateContainerExitLoop  = "ContainerExitLoop"
	KubeStateMemoryLimitSqueeze = "MemoryLimitSqueeze"
	KubeStateUnschedulable      = "Unschedulable"
	KubeStateJobFailure         = "JobFailure"
	KubeStateSelectorDrift      = "SelectorDrift"
	KubeStateBackendCrashLoop   = "BackendCrashLoop"
	KubeStateUnboundClaim       = "UnboundClaim"
	KubeStateDependencyStall    = "DependencyStall"
	KubeStatePDBGridlock        = "PDBGridlock"
	KubeStateRolloutStuck       = "RolloutStuck"
	KubeStateCertExpiry         = "CertExpiry"
	KubeStateNoOp               = "NoOp"
)

// kubeStateDefaultNames give each synthesized workload a plausible name when
// the manifest does not choose one. See KubeStateWorkloadName.
var kubeStateDefaultNames = map[string]string{
	KubeStateImageUnresolvable:  "catalog-sync",
	KubeStateContainerExitLoop:  "report-worker",
	KubeStateMemoryLimitSqueeze: "session-cache",
	KubeStateUnschedulable:      "batch-runner",
	KubeStateJobFailure:         "schema-migrate",
	KubeStateSelectorDrift:      "storefront",
	KubeStateBackendCrashLoop:   "orders-api",
	KubeStateUnboundClaim:       "media-store",
	KubeStateDependencyStall:    "checkout-api",
	KubeStatePDBGridlock:        "ledger-api",
	KubeStateRolloutStuck:       "web-frontend",
	KubeStateCertExpiry:         "edge-gateway",
	KubeStateNoOp:               "inventory-api",
}

// KubeStateDefaultStallMessage is what a DependencyStall workload writes when
// the manifest does not choose a line.
//
// It names a dependency, a port and a timeout, because that is what the
// diagnosis turns on: the workload is fine and the thing behind it is not. It
// deliberately does not mention Simian. The log is the only evidence this fault
// leaves, so a subject that could recognise the injector by its wording would be
// pattern-matching on the rig instead of reading the failure.
const KubeStateDefaultStallMessage = `level=error msg="upstream request failed" upstream=payments-api addr=10.0.0.31:8443 err="context deadline exceeded after 30s"`

// KubeStateStallMessage resolves the line a DependencyStall workload writes to
// its log.
//
// Like KubeStateWorkloadName, it lives here because two callers have to agree
// on it before the pod exists: the driver, which puts it in the container's
// environment, and the default efficacy gate, which greps it back out of the
// log. A gate matching a different string than the container writes would fail
// against a fault that landed perfectly.
//
// Trimmed, and an empty or whitespace-only request falls back to the default.
// The gate is a substring match, so "" and " " would both pass against any log
// at all — see the anti-vacuity rule in pkg/probe's logs prober.
func KubeStateStallMessage(requested string) string {
	if s := strings.TrimSpace(requested); s != "" {
		return s
	}
	return KubeStateDefaultStallMessage
}

// kubeStateDefaultReplicas overrides the engine-wide default of one for the
// kinds whose fault needs more than one pod to be what it is.
var kubeStateDefaultReplicas = map[string]int{
	// A stalled rollout is about the *old* revision continuing to serve while
	// the new one cannot start, and one surviving pod makes a thin case for
	// "users saw nothing". Two is the smallest count that reads like a real
	// service and matches the shape the lookout fixture this kind is
	// transcribed from produces: new_ready=0/1, old_ready=2/2.
	KubeStateRolloutStuck: 2,

	// A Service that lost every backend is a different diagnosis from a pod
	// that is crash-looping, and with one replica the two are the same
	// sentence. Two makes "all of them are down" a claim about a set, which is
	// what the Service-level finding is about, and matches the shape of the
	// upstream cascade fixture this kind is transcribed from.
	KubeStateBackendCrashLoop: 2,
}

// KubeStateCrashLoopRestarts is how many restarts a crash-loop gate waits for
// before it will call the fault landed.
//
// Five, and the number is not arbitrary. The kubelet's backoff schedule is
// 10s, 20s, 40s, 80s, 160s, so a container that exits immediately reaches its
// fifth restart about 150 seconds in — and from there it spends 160 seconds of
// every 160 in `waiting: CrashLoopBackOff`. Before that the backoff windows are
// shorter than the gaps between them, which is why polling for the waiting
// reason earlier is a coin flip: the state is not yet where the pod lives.
//
// So this threshold is the point at which a crash loop becomes continuously
// observable, by anything, rather than a state you have to be lucky to sample.
// That it also happens to be where k8s-lookout's restart-count check fires is a
// convenience, not the reason: a gate tuned to one subject's thresholds would
// score that subject on Simian's timing rather than on its judgement.
const KubeStateCrashLoopRestarts = 5

// KubeStateDefaultReplicas is how many replicas a kind synthesizes when the
// manifest does not choose.
//
// Here rather than in the driver for the same reason the workload name is: the
// RolloutStuck gate asserts that every replica of the previous revision is
// still available, and it has to know how many that is before Apply has created
// any of them.
func KubeStateDefaultReplicas(kind string) int {
	if n, ok := kubeStateDefaultReplicas[kind]; ok {
		return n
	}
	return 1
}

// KubeStateCertPEMPrefix is the base64 rendering of the opening bytes of a PEM
// certificate block, which is what a CertExpiry gate matches against the
// Secret's own `data["tls.crt"]`.
//
// Computed rather than written out, because a hand-transcribed base64 literal
// that is one character wrong produces a gate that never passes against a fault
// that landed perfectly, and nothing about the literal would look wrong. Exactly
// 24 bytes, so the encoding is a whole number of base64 groups and the result is
// a genuine prefix of any longer certificate rather than a string that only
// matches when the padding happens to line up.
var KubeStateCertPEMPrefix = base64.StdEncoding.EncodeToString([]byte("-----BEGIN CERTIFICATE--"))

// KubeStateDefaultCertHours is how long a CertExpiry certificate is valid for
// when the manifest does not say. Two days: inside every conventional warning
// threshold, and still a leading indicator rather than an outage — nothing is
// failing yet, which is the whole character of this fault.
const KubeStateDefaultCertHours = 48

// KubeStateCertHoursBounds are the limits on spec.expires_in_hours.
//
// Negative is allowed and useful: a certificate that expired a week ago is a
// different diagnosis from one that expires on Thursday, and both are real. The
// upper bound is a year, past which the certificate is not expiring in any sense
// a report could be graded on.
const (
	KubeStateMinCertHours = -168
	KubeStateMaxCertHours = 8760
)

// KubeStateWorkloadName derives the name of the workload a kube-state fault
// will synthesize, as a pure function of the kind, the manifest's requested
// name, and the fault UID.
//
// It lives in this package, rather than in the driver that creates the object,
// because two callers have to agree on the name before the object exists: the
// driver, and the default efficacy gate, which must write a label selector for
// pods that Apply has not created yet. Deriving the suffix from the fault UID
// instead of from a fresh random value is what makes that agreement possible.
// It also means the workload name is reproducible from an audit record months
// later, which a random suffix would not be.
//
// The name defaults to something neutral because the name is the first thing
// anything diagnosing the namespace reads. A workload called
// `simian-imageunresolvable-01K4…` lets a subject under evaluation identify
// the injected object without diagnosing anything, and the rig would be
// measuring pattern-matching against our own naming convention. The Deployment
// still carries simian.chaos/managed so the reaper can find it; the pods do
// not, and the pods are what a diagnosis looks at.
//
// Returns "" for a kind with no default name and no requested one — the caller
// decides whether that is an error (the driver) or a reason to attach no gate
// (the probe builder).
func KubeStateWorkloadName(kind, requested, faultUID string) string {
	base := requested
	if base == "" {
		base = kubeStateDefaultNames[kind]
	}
	if base == "" {
		return ""
	}
	return base + "-" + kubeStateNameSuffix(faultUID)
}

// kubeStateNameSuffix reduces a fault UID to at most eight lowercase
// alphanumerics — the shape of a pod-template hash rather than of a ULID, so
// the synthesized workload reads like anything else in the namespace.
//
// Non-alphanumerics are dropped rather than replaced: the result is pasted
// onto the end of a DNS-1123 label, which has to end in an alphanumeric.
func kubeStateNameSuffix(faultUID string) string {
	b := make([]rune, 0, 8)
	for _, r := range strings.ToLower(faultUID) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b = append(b, r)
		}
	}
	if len(b) == 0 {
		// No UID to derive from. Still has to produce a legal label, and
		// still has to be the same answer for both callers.
		return "0"
	}
	if len(b) > 8 {
		b = b[len(b)-8:]
	}
	return string(b)
}
