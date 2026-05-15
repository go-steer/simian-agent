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

// Package audit provides the structured, append-only audit log used by the
// Fault Executor and adjacent subsystems. The default sink is slog; alternative
// sinks (file, Cloud Logging) can satisfy simian.Auditor directly.
package audit

import (
	"context"
	"log/slog"
	"time"

	"github.com/go-steer/simian-agent/pkg/simian"
)

// Canonical event names. Keep additions backward-compatible; downstream
// consumers (queries, dashboards) depend on these strings.
const (
	EventPlanGenerated     = "plan.generated"
	EventExecutorReceived  = "executor.received"
	EventExecutorValidated = "executor.validated"
	EventExecutorRejected  = "executor.rejected"
	EventDriverApplied     = "driver.applied"
	EventDriverFailed      = "driver.failed"
	EventLeaseRegistered   = "lease.registered"
	EventLeaseHeartbeat    = "lease.heartbeat"
	EventLeaseExpired      = "lease.expired"
	EventLeaseCleared      = "lease.cleared"
	EventPageDispatched    = "page.dispatched"
	EventPageFailed        = "page.failed"
	EventAgentResponse     = "agent.response_received"

	// M3 autonomous-mode cycle events.
	EventCycleStarted     = "cycle.started"
	EventCycleSkipped     = "cycle.skipped"
	EventCycleCompleted   = "cycle.completed"
	EventHealthGateFailed = "cycle.health_gate_failed"
	EventLLMUnavailable   = "cycle.llm_unavailable"
	EventStepSkipped      = "cycle.step_skipped"
)

// SLogAuditor is the default Auditor — it writes audit events to a slog.Logger
// under a stable component name. Audit lines are emitted at Info level so
// they're capturable without enabling Debug.
type SLogAuditor struct {
	Logger *slog.Logger
}

// New creates an SLogAuditor wrapping the given logger. If logger is nil,
// slog.Default() is used.
func New(logger *slog.Logger) *SLogAuditor {
	if logger == nil {
		logger = slog.Default()
	}
	return &SLogAuditor{Logger: logger.With(slog.String("component", "audit"))}
}

// Emit implements simian.Auditor.
func (a *SLogAuditor) Emit(ctx context.Context, ev simian.AuditEvent) {
	attrs := []any{
		slog.String("event", ev.Event),
		slog.Time("ts", time.Now().UTC()),
	}
	if ev.FaultUID != "" {
		attrs = append(attrs, slog.String("fault_uid", ev.FaultUID))
	}
	if ev.PlanID != "" {
		attrs = append(attrs, slog.String("plan_id", ev.PlanID))
	}
	if ev.ScenarioID != "" {
		attrs = append(attrs, slog.String("scenario_id", ev.ScenarioID))
	}
	if ev.Mode != "" {
		attrs = append(attrs, slog.String("mode", string(ev.Mode)))
	}
	if ev.Reason != "" {
		attrs = append(attrs, slog.String("reason", ev.Reason))
	}
	if len(ev.Payload) > 0 {
		attrs = append(attrs, slog.Any("payload", ev.Payload))
	}
	a.Logger.LogAttrs(ctx, slog.LevelInfo, "audit", attrsToSlog(attrs)...)
}

func attrsToSlog(attrs []any) []slog.Attr {
	out := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		if at, ok := a.(slog.Attr); ok {
			out = append(out, at)
		}
	}
	return out
}
