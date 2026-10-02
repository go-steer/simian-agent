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

package lease

import (
	"context"
	"sort"
	"time"

	"github.com/go-steer/simian-agent/pkg/simian"
)

// reasonUntrackedAfterRestart mirrors audit.ReasonUntrackedAfterRestart,
// duplicated rather than imported to keep this package free of pkg/audit.
const reasonUntrackedAfterRestart = "untracked-after-restart"

// AdoptLive asks every driver that can list its own live faults for the ones
// in namespaces, and adopts each the registry does not already hold (#177).
// Returns how many it adopted.
//
// Run at start-up after whatever the audit file adopted, which knows more
// about each fault (its spec, its plan): this only fills in what the file
// could not, because it was lost with the pod, rotated away, or never
// written. Each adoption is audited as lease.adopted, found_in "cluster".
// A driver that fails to list is audited and skipped; the others still run.
func AdoptLive(ctx context.Context, registry *Registry, drivers map[simian.Engine]simian.ChaosDriver,
	namespaces []string, auditor simian.Auditor, now time.Time) int {
	if len(namespaces) == 0 {
		return 0
	}
	engines := make([]simian.Engine, 0, len(drivers))
	for e := range drivers {
		engines = append(engines, e)
	}
	sort.Slice(engines, func(i, j int) bool { return engines[i] < engines[j] })

	adopted := 0
	for _, engine := range engines {
		lister, ok := drivers[engine].(simian.LiveFaultLister)
		if !ok {
			continue
		}
		live, err := lister.ListLive(ctx, namespaces, now)
		if err != nil && auditor != nil {
			auditor.Emit(ctx, simian.AuditEvent{
				Event:   "lease.cleared",
				Reason:  "adopt-live-failed",
				Payload: map[string]any{"error": err.Error(), "engine": string(engine)},
			})
		}
		for _, af := range live {
			if _, held := registry.Get(af.FaultUID); held || af.FaultUID == "" {
				continue
			}
			registry.Adopt(af)
			adopted++
			if auditor != nil {
				auditor.Emit(ctx, simian.AuditEvent{
					Event:    "lease.adopted",
					FaultUID: af.FaultUID,
					Reason:   reasonUntrackedAfterRestart,
					Payload: map[string]any{
						"engine_uid": af.EngineUID,
						"engine":     string(engine),
						"deadline":   af.Deadline.UTC().Format(time.RFC3339),
						"found_in":   "cluster",
					},
				})
			}
		}
	}
	return adopted
}
