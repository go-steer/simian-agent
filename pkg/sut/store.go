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

package sut

import "context"

// BaselineStore persists Baselines outside the Manager process so they survive
// `simian serve` restarts. The in-memory cache on Manager remains authoritative
// during a single process lifetime; the store is the durable mirror.
//
// All methods MUST be safe to call against a missing object — Delete on a
// non-existent baseline returns nil; Load on a missing namespace returns
// (zero, false, nil).
type BaselineStore interface {
	// Save persists the given baseline. The namespace is taken from bl.Namespace.
	Save(ctx context.Context, bl Baseline) error

	// Load returns the persisted baseline for a namespace, if any. The bool
	// signals presence; nil error + false means "no baseline persisted".
	Load(ctx context.Context, namespace string) (Baseline, bool, error)

	// Delete removes the persisted baseline for a namespace. No error if absent.
	Delete(ctx context.Context, namespace string) error

	// List returns all persisted baselines across all namespaces. Used at
	// `simian serve` startup to warm the in-memory cache.
	List(ctx context.Context) ([]Baseline, error)
}

// noopStore is the default BaselineStore — discards writes, returns nothing on
// reads. Used by NewManager when no explicit store is configured (tests, the
// out-of-process `simian sut deploy` CLI path, anything that doesn't need
// cross-restart durability).
type noopStore struct{}

func (noopStore) Save(context.Context, Baseline) error                 { return nil }
func (noopStore) Load(context.Context, string) (Baseline, bool, error) { return Baseline{}, false, nil }
func (noopStore) Delete(context.Context, string) error                 { return nil }
func (noopStore) List(context.Context) ([]Baseline, error)             { return nil, nil }
