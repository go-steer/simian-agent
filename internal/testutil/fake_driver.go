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

// Package testutil holds shared test helpers — fake driver, fake auditor,
// and small builders for FaultManifest fixtures.
package testutil

import (
	"context"
	"errors"
	"sync"

	"github.com/go-steer/simian-agent/pkg/simian"
)

// FakeDriver is a deterministic in-memory ChaosDriver for tests.
type FakeDriver struct {
	EngineName simian.Engine
	CatalogVal []simian.CatalogEntry

	ApplyFn func(ctx context.Context, m simian.FaultManifest) (string, error)
	ClearFn func(ctx context.Context, engineUID string) error

	mu      sync.Mutex
	Applied []simian.FaultManifest
	Cleared []string
}

// Engine implements simian.ChaosDriver.
func (f *FakeDriver) Engine() simian.Engine {
	if f.EngineName == "" {
		return simian.EngineChaosMesh
	}
	return f.EngineName
}

// Apply implements simian.ChaosDriver.
func (f *FakeDriver) Apply(ctx context.Context, m simian.FaultManifest) (string, error) {
	f.mu.Lock()
	f.Applied = append(f.Applied, m)
	f.mu.Unlock()
	if f.ApplyFn != nil {
		return f.ApplyFn(ctx, m)
	}
	return "engine-uid-" + m.UID, nil
}

// Clear implements simian.ChaosDriver.
func (f *FakeDriver) Clear(ctx context.Context, engineUID string) error {
	f.mu.Lock()
	f.Cleared = append(f.Cleared, engineUID)
	f.mu.Unlock()
	if f.ClearFn != nil {
		return f.ClearFn(ctx, engineUID)
	}
	return nil
}

// Catalog implements simian.ChaosDriver.
func (f *FakeDriver) Catalog(_ context.Context) ([]simian.CatalogEntry, error) {
	return f.CatalogVal, nil
}

// AppliedCopy returns a thread-safe snapshot of Applied.
func (f *FakeDriver) AppliedCopy() []simian.FaultManifest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]simian.FaultManifest, len(f.Applied))
	copy(out, f.Applied)
	return out
}

// ErrFakeApply is a sentinel for ApplyFn implementations that want to fail.
var ErrFakeApply = errors.New("fake driver: forced apply failure")

// FakeAuditor records every emitted event in memory.
type FakeAuditor struct {
	mu     sync.Mutex
	Events []simian.AuditEvent
}

// Emit implements simian.Auditor.
func (a *FakeAuditor) Emit(_ context.Context, ev simian.AuditEvent) {
	a.mu.Lock()
	a.Events = append(a.Events, ev)
	a.mu.Unlock()
}

// EventNames returns the ordered list of event names captured.
func (a *FakeAuditor) EventNames() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.Events))
	for i, e := range a.Events {
		out[i] = e.Event
	}
	return out
}

// FindEvent returns the first AuditEvent matching name, or false.
func (a *FakeAuditor) FindEvent(name string) (simian.AuditEvent, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.Events {
		if e.Event == name {
			return e, true
		}
	}
	return simian.AuditEvent{}, false
}
