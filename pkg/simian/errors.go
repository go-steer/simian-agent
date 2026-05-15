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
