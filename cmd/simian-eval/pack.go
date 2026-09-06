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

package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/go-steer/simian-agent/pkg/scenario"
)

// loadPacks reads one or more packs — built-in names or directories — and
// returns them as a single suite.
//
// Merged rather than run one after another, because everything downstream —
// the namespace fencing, the concurrency ceiling, the scorecard — is defined
// over one set of scenarios. Two packs that share a scenario ID are a load
// error: IDs are the key the audit log joins to the report on, and a duplicate
// silently merges two scenarios' evidence into a plausible-looking wrong score.
func loadPacks(refs []string) (scenario.Pack, error) {
	if len(refs) == 0 {
		return scenario.Pack{}, fmt.Errorf("no packs given")
	}
	if len(refs) == 1 {
		return scenario.LoadRef(refs[0])
	}

	var (
		merged scenario.Pack
		names  []string
	)
	for _, ref := range refs {
		p, err := scenario.LoadRef(ref)
		if err != nil {
			return scenario.Pack{}, err
		}
		names = append(names, p.Name)
		merged.Scenarios = append(merged.Scenarios, p.Scenarios...)
	}
	merged.Name = strings.Join(names, "+")

	if err := merged.Validate(); err != nil {
		return scenario.Pack{}, fmt.Errorf("packs %s do not compose: %w", merged.Name, err)
	}
	return merged, nil
}

// checkFaultDurations reports scenarios whose faults expire before the subject
// is out of time.
//
// The lease reaper exists to stop Simian leaking chaos into a cluster, and it
// will happily clear a fault whose duration ran out while the subject was
// still thinking. The harness watches for exactly that disappearance and
// records it as time-to-remediate — so a fault that is too short does not fail
// the run, it hands the subject a remediation it did not perform. Refused up
// front rather than discovered in a suspiciously good scorecard.
//
// Over the selected scenarios, not the whole pack. A subject slow enough to
// matter here is exactly the subject an operator reaches for --only with, and
// a check that refused the run over a scenario nobody asked to run would make
// the two flags mutually exclusive.
func checkFaultDurations(selected []scenario.Scenario, subjectTimeout time.Duration) error {
	if subjectTimeout <= 0 {
		return nil
	}
	var short []string
	for _, s := range selected {
		for _, f := range s.Faults {
			if f.Duration <= 0 {
				continue // the executor is the authority on an unset duration
			}
			if f.Duration < subjectTimeout {
				short = append(short, fmt.Sprintf("%s (%s)", s.ID, f.Duration))
				break
			}
		}
	}
	if len(short) == 0 {
		return nil
	}
	return fmt.Errorf("these scenarios have faults shorter than --subject-timeout %s: %s\n"+
		"the lease expires mid-investigation and the reaper clears it, which the harness records as the subject having remediated the fault; "+
		"lengthen the faults, shorten --subject-timeout, or pass --allow-short-faults to accept the measurement",
		subjectTimeout, strings.Join(short, ", "))
}
