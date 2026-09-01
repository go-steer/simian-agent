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

// Package probe verifies that a fault landed.
//
// This is efficacy, not outcome. Simian checks that the partition exists, that
// the pod is really in CrashLoopBackOff, that the endpoint list is really
// empty — and stops there. What the fault *caused* is what the agent under
// test is scored on, and computing it here would let the harness grade its own
// experiment.
//
// The motivation is a specific failure mode: a fault that is accepted by the
// cluster and then silently does nothing. The eval result reads "the agent
// missed a network partition" when there was no partition. That is worse than
// no measurement — it is a confident wrong number, and it averages in with the
// real ones.
package probe

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-steer/simian-agent/pkg/simian"
)

// Default poll timing, used when a probe spec does not say.
const (
	DefaultTimeout  = 90 * time.Second
	DefaultInterval = 2 * time.Second
)

// Result is the outcome of one probe run. It is deliberately not an error:
// what the probe *saw* is the interesting part, and it is just as interesting
// on failure as on success, so it always travels with the verdict.
type Result struct {
	// Name and Type are copied from the ProbeSpec so a Result stands alone in
	// an audit record.
	Name string
	Type string

	// Passed is the verdict.
	Passed bool

	// Observed is the raw value the last poll read. Recorded pass or fail: an
	// efficacy event carrying only a boolean cannot be debugged after the
	// arena is gone.
	Observed string

	// Expected describes the condition in words, for timeout messages.
	Expected string

	// Attempts is how many polls ran, and Elapsed how long they took.
	Attempts int
	Elapsed  time.Duration

	// Err is set when the probe could not be run at all (bad spec, RBAC, no
	// such resource) as opposed to running and not being satisfied. A probe
	// that ran cleanly and never saw what it wanted has Passed false, a
	// populated Observed, and a nil Err.
	Err error
}

// Describe renders the result as a single line for a timeout message.
func (r Result) Describe() string {
	if r.Err != nil {
		return fmt.Sprintf("probe %q (%s) errored: %v", r.Name, r.Type, r.Err)
	}
	if r.Passed {
		return fmt.Sprintf("probe %q (%s) passed after %s: observed %q",
			r.Name, r.Type, r.Elapsed.Round(time.Millisecond), truncate(r.Observed, 200))
	}
	return fmt.Sprintf("probe %q (%s) never passed in %s (%d polls): wanted %s, last saw %q",
		r.Name, r.Type, r.Elapsed.Round(time.Millisecond), r.Attempts,
		r.Expected, truncate(r.Observed, 200))
}

// Prober runs a single probe to completion.
//
// defaultNamespace is the fault's own target namespace. A probe spec may name
// a namespace explicitly, but almost never should: a probe that can read
// outside the arena is a probe that can be pointed outside the arena.
type Prober interface {
	Run(ctx context.Context, p simian.ProbeSpec, defaultNamespace string) Result
}

// truncate shortens s for log lines. Probe output is jsonpath over whole
// objects and can be very long.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
