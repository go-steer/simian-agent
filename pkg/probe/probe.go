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
	"sort"
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

// Target is the fault's own subject, supplied to every probe as the default it
// inherits when its spec does not say otherwise.
//
// A probe spec may name a namespace and selector explicitly, but almost never
// should: a probe that can read outside the arena is a probe that can be
// pointed outside the arena. Inheritance also makes a probe reusable across
// faults — the default gate for "NetworkChaos partition" is written once and
// aims itself at whatever the manifest targets.
type Target struct {
	Namespace string
	Labels    map[string]string
}

// Selector renders Labels as a label-selector string, sorted so it is stable
// across runs. Empty if there are no labels.
func (t Target) Selector() string {
	if len(t.Labels) == 0 {
		return ""
	}
	parts := make([]string, 0, len(t.Labels))
	for k, v := range t.Labels {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// Prober runs a single probe to completion.
type Prober interface {
	Run(ctx context.Context, p simian.ProbeSpec, target Target) Result
}

// Mux dispatches a probe to the Prober registered for its type.
type Mux struct {
	byType map[string]Prober
}

// NewMux builds a dispatching Prober. Keys are simian.ProbeType* values.
func NewMux(byType map[string]Prober) *Mux {
	return &Mux{byType: byType}
}

// Run implements Prober. An unregistered type is an error, not a pass: a
// manifest asking for a gate Simian cannot run must not be treated as gated.
func (m *Mux) Run(ctx context.Context, p simian.ProbeSpec, target Target) Result {
	sub, ok := m.byType[p.Type]
	if !ok {
		known := make([]string, 0, len(m.byType))
		for t := range m.byType {
			known = append(known, t)
		}
		sort.Strings(known)
		return Result{
			Name: p.Name,
			Type: p.Type,
			Err: fmt.Errorf("probe %q: no prober for type %q (have: %s)",
				p.Name, p.Type, strings.Join(known, ", ")),
		}
	}
	return sub.Run(ctx, p, target)
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

// --- spec decoding helpers, shared by every probe type ---

func optString(raw map[string]any, key string) (string, error) {
	v, ok := raw[key]
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("probe: %q must be a string, got %T", key, v)
	}
	return s, nil
}

func optBool(raw map[string]any, key string) (bool, error) {
	v, ok := raw[key]
	if !ok || v == nil {
		return false, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("probe: %q must be a bool, got %T", key, v)
	}
	return b, nil
}

// optDuration accepts a Go duration string ("90s", "2m"). Numbers are rejected
// rather than guessed at: "timeout": 30 is ambiguous between seconds and
// nanoseconds, and picking wrong turns the gate into a no-op or a hang.
func optDuration(raw map[string]any, key string, def time.Duration) (time.Duration, error) {
	v, ok := raw[key]
	if !ok || v == nil {
		return def, nil
	}
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("probe: %q must be a duration string like \"90s\", got %T", key, v)
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("probe: %q: %w", key, err)
	}
	return d, nil
}

// optInt accepts a JSON number. Decoded specs come off the wire as float64,
// so both shapes are handled; a fractional value is a mistake, not a rounding.
func optInt(raw map[string]any, key string) (int, error) {
	v, ok := raw[key]
	if !ok || v == nil {
		return 0, nil
	}
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case float64:
		if n != float64(int(n)) {
			return 0, fmt.Errorf("probe: %q must be a whole number, got %v", key, n)
		}
		return int(n), nil
	default:
		return 0, fmt.Errorf("probe: %q must be a number, got %T", key, v)
	}
}
