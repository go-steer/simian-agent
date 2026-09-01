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

package simian

import "fmt"

// ExecutorStage is the stage of the executor pipeline that produced an error.
type ExecutorStage string

const (
	StageSchema ExecutorStage = "schema"
	StageSafety ExecutorStage = "safety"
	StageAudit  ExecutorStage = "audit"
	StageDriver ExecutorStage = "driver"
	StageLease  ExecutorStage = "lease"
	// StageProbe is the efficacy gate that runs after the driver applied the
	// fault: the fault exists, but has not been observed to take effect.
	StageProbe ExecutorStage = "probe"
	// StagePrecheck is the gate that runs before the driver: the cluster was
	// not in the state the fault's own verification assumes it starts from.
	// Nothing has been applied when this stage fails.
	StagePrecheck ExecutorStage = "precheck"
)

// RejectionReason is a stable identifier for why a manifest was rejected.
// Stable strings so audit logs and metrics labels stay queryable.
type RejectionReason string

const (
	ReasonUnknownGVK           RejectionReason = "unknown-gvk"
	ReasonSchemaInvalid        RejectionReason = "schema-invalid"
	ReasonNamespaceNotEligible RejectionReason = "namespace-not-eligible"
	ReasonWorkloadExcluded     RejectionReason = "workload-excluded"
	ReasonRBACDenied           RejectionReason = "rbac-denied"
	ReasonTierNotPermitted     RejectionReason = "tier-not-permitted"
	ReasonDurationOverCeiling  RejectionReason = "duration-over-ceiling"
	ReasonBudgetExceeded       RejectionReason = "budget-exceeded"
	ReasonDriverFailed         RejectionReason = "driver-failed"
	ReasonLeaseFailed          RejectionReason = "lease-failed"

	// ReasonProbeFailed means a Settle probe never passed within its timeout.
	// Distinct from driver-failed on purpose: the fault was accepted by the
	// cluster and simply did nothing, which is the failure mode that quietly
	// turns an eval result into a confident wrong number.
	ReasonProbeFailed RejectionReason = "probe-failed"

	// ReasonPrecheckFailed means an SOT probe never passed, so the experiment
	// would have started from a state its own verification cannot interpret —
	// a workload that was already unreachable, or already slow. Rejected before
	// the driver runs, so there is nothing to roll back.
	ReasonPrecheckFailed RejectionReason = "precheck-failed"

	// ReasonProbeNotConfigured means the manifest carries Settle probes but no
	// prober is wired in. Loud by design: silently skipping the gate would
	// report unverified faults as verified.
	ReasonProbeNotConfigured RejectionReason = "probe-not-configured"
)

// ExecutorError is the typed error returned by FaultExecutor methods. Callers
// can inspect Stage and Reason for programmatic handling; Wrapped is the
// underlying cause if any.
type ExecutorError struct {
	Stage   ExecutorStage
	Reason  RejectionReason
	Message string
	Wrapped error
}

func (e *ExecutorError) Error() string {
	if e.Wrapped != nil {
		return fmt.Sprintf("executor[%s:%s]: %s: %v", e.Stage, e.Reason, e.Message, e.Wrapped)
	}
	return fmt.Sprintf("executor[%s:%s]: %s", e.Stage, e.Reason, e.Message)
}

func (e *ExecutorError) Unwrap() error { return e.Wrapped }

// NewExecutorError is a convenience constructor.
func NewExecutorError(stage ExecutorStage, reason RejectionReason, msg string, cause error) *ExecutorError {
	return &ExecutorError{Stage: stage, Reason: reason, Message: msg, Wrapped: cause}
}
