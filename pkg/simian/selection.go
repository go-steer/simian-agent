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

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
)

// Most kinds act on the pods their spec selects, so the executor can judge
// them before the driver runs: it reads the selector. A few kinds choose what
// they act on when they are applied — ZoneOutage finds the arena's workloads
// with pods in a zone at that moment. What follows lets the executor and such
// a driver share that decision without changing ChaosDriver: the executor
// hands the driver the arena's exclusions, asks it beforehand whether the
// fault would act on anything, and collects what it did act on for the audit
// trail.

// ExcludeWorkloadsAnnotation is the namespace annotation listing workloads,
// comma-separated, that no fault may act on. Here so a driver that picks its
// own workloads can honour it without importing the executor.
const ExcludeWorkloadsAnnotation = "simian.chaos/exclude-workloads"

// ErrTargetIncompatible marks a TargetChecker answer that the fault would act
// on nothing — no workload of the arena in the zone it names, say. The
// executor refuses such a fault as target-incompatible before the driver
// runs.
var ErrTargetIncompatible = errors.New("target incompatible")

// TargetChecker is an optional ChaosDriver capability for kinds that choose
// their pods when applied. The executor calls it before Apply, with the
// arena's exclusions on ctx (WithExcludedWorkloads).
type TargetChecker interface {
	// CheckTargets reports whether Apply would act on anything now, without
	// changing the cluster. An error wrapping ErrTargetIncompatible means it
	// would not; any other error means the driver could not tell, and is
	// left to Apply. Returns nil for kinds it does not choose pods for.
	CheckTargets(ctx context.Context, m FaultManifest) error
}

type excludedKey struct{}

// WithExcludedWorkloads returns ctx carrying each target namespace's excluded
// workloads, as the executor's eligibility checker read them. A driver that
// picks workloads itself must skip these.
func WithExcludedWorkloads(ctx context.Context, byNamespace map[string][]string) context.Context {
	return context.WithValue(ctx, excludedKey{}, maps.Clone(byNamespace))
}

// ExcludedWorkloadsFrom returns the excluded workloads WithExcludedWorkloads
// put in ctx for namespace, or nil.
func ExcludedWorkloadsFrom(ctx context.Context, namespace string) []string {
	m, _ := ctx.Value(excludedKey{}).(map[string][]string)
	return slices.Clone(m[namespace])
}

type applyDetailsKey struct{}

// ApplyDetails collects what a driver says it acted on, for the
// driver.applied audit event. Safe for concurrent use.
type ApplyDetails struct {
	mu sync.Mutex
	m  map[string]any
}

// WithApplyDetails returns ctx carrying a fresh ApplyDetails, and the
// ApplyDetails itself for the caller to read once Apply returns.
func WithApplyDetails(ctx context.Context) (context.Context, *ApplyDetails) {
	d := &ApplyDetails{}
	return context.WithValue(ctx, applyDetailsKey{}, d), d
}

// RecordApplyDetail records key on the ApplyDetails in ctx, if any. A driver
// calls it from Apply; without a collector on ctx it is a no-op.
func RecordApplyDetail(ctx context.Context, key string, value any) {
	d, _ := ctx.Value(applyDetailsKey{}).(*ApplyDetails)
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.m == nil {
		d.m = map[string]any{}
	}
	d.m[key] = value
}

// All returns a copy of what was recorded. Nil when nothing was.
func (d *ApplyDetails) All() map[string]any {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return maps.Clone(d.m)
}
