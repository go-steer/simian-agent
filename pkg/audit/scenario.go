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

package audit

import (
	"context"

	"github.com/go-steer/simian-agent/pkg/simian"
)

type scenarioIDKey struct{}

// WithScenarioID returns a context that stamps id onto every audit event
// emitted through a ScenarioStamper while it is in scope.
//
// The scenario ID is carried in the context rather than passed as an argument
// because it is ambient to a whole run and irrelevant to every function that
// would have to forward it. Simian emits audit events from roughly thirty
// call sites across the executor, the autonomous loop and the lease reaper;
// adding a parameter to each is churn that the next new call site silently
// forgets, and a missing join key is invisible until someone tries to
// correlate a run and finds a hole in it.
func WithScenarioID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, scenarioIDKey{}, id)
}

// ScenarioIDFrom returns the scenario ID carried by ctx, if any.
func ScenarioIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(scenarioIDKey{}).(string)
	return id
}

// ScenarioStamper wraps an Auditor and fills in ScenarioID from the context
// on every event that does not already carry one.
//
// An event that sets ScenarioID explicitly wins. That matters for the lease
// reaper, which outlives the request context that applied a fault: a fault
// reaped after its scenario's context is gone has no ambient ID to read, so
// the lease carries its own and sets it directly.
type ScenarioStamper struct {
	Inner simian.Auditor
}

// NewScenarioStamper wraps inner so that audit events pick up the scenario ID
// from their context. A nil inner makes this a no-op sink rather than a panic,
// matching how the rest of the package treats absent dependencies.
func NewScenarioStamper(inner simian.Auditor) *ScenarioStamper {
	return &ScenarioStamper{Inner: inner}
}

// Emit implements simian.Auditor.
func (s *ScenarioStamper) Emit(ctx context.Context, ev simian.AuditEvent) {
	if s == nil || s.Inner == nil {
		return
	}
	if ev.ScenarioID == "" {
		ev.ScenarioID = ScenarioIDFrom(ctx)
	}
	s.Inner.Emit(ctx, ev)
}
