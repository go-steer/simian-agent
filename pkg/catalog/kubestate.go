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

import "strings"

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
	KubeStateUnboundClaim       = "UnboundClaim"
	KubeStateDependencyStall    = "DependencyStall"
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
	KubeStateUnboundClaim:       "media-store",
	KubeStateDependencyStall:    "checkout-api",
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
