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

package eval

import (
	"fmt"
	"sort"

	"github.com/go-steer/simian-agent/pkg/scenario"
)

// MeasureEfficacyRate is the seventh measure. It is a property of a suite
// rather than of a run, so it lives on Summary and not in DefaultMeasures.
const MeasureEfficacyRate = "efficacy_rate"

// ScenarioResult is one scenario's scores, keyed so a report can be joined
// back to the audit log.
type ScenarioResult struct {
	ScenarioID   string  `json:"scenario_id"`
	ScenarioName string  `json:"scenario_name"`
	Scores       []Score `json:"scores"`

	// Manifested and InjectError are repeated from the Run because a reader
	// of the result needs to know whether a row of skips means "not
	// applicable" or "the harness broke".
	Manifested  bool   `json:"manifested"`
	InjectError string `json:"inject_error,omitempty"`
}

// Score returns the named score from this result.
func (r ScenarioResult) Score(name string) (Score, bool) {
	for _, s := range r.Scores {
		if s.Name == name {
			return s, true
		}
	}
	return Score{}, false
}

// Summary is a whole suite's worth of scoring: one subject, one pack.
type Summary struct {
	Subject string           `json:"subject"`
	Pack    string           `json:"pack"`
	Results []ScenarioResult `json:"results"`

	// Means is the average of each measure over the runs where it applied.
	// Skipped measures are excluded rather than counted as zero.
	Means map[string]float64 `json:"means"`

	// Scenarios, Manifested and InjectFailures are the harness's own report
	// card. A suite with a poor efficacy rate has not measured a poor
	// subject; it has failed to measure anything.
	Scenarios      int `json:"scenarios"`
	Manifested     int `json:"manifested"`
	InjectFailures int `json:"inject_failures"`

	// EfficacyRate is the fraction of scenarios that actually manifested.
	//
	// This number is read before any of the others. Every measure above it
	// assumes the cluster was broken in the way the scenario said; where it
	// was not, a zero means "there was nothing to find" rather than "the
	// subject missed it", and the two are indistinguishable from the score
	// alone. `core-sre-agent`'s harness refuses to report at all below a
	// threshold, on the grounds that the harness is broken, not the agent.
	EfficacyRate float64 `json:"efficacy_rate"`
}

// Summarize scores every run against its scenario and aggregates the result.
//
// Runs are matched to scenarios by ScenarioID — the same key the audit log
// carries — and a run naming a scenario the pack does not contain is an
// error rather than a silently dropped row, because a dropped row shrinks
// the denominator and quietly improves every mean.
func Summarize(subject string, pack scenario.Pack, runs []Run) (Summary, error) {
	sum := Summary{
		Subject: subject,
		Pack:    pack.Name,
		Means:   map[string]float64{},
	}

	totals := map[string]float64{}
	counts := map[string]int{}

	for _, run := range runs {
		s, ok := pack.ByID(run.ScenarioID)
		if !ok {
			return Summary{}, fmt.Errorf("eval: run references scenario %q, which is not in pack %q", run.ScenarioID, pack.Name)
		}

		res := ScenarioResult{
			ScenarioID:   s.ID,
			ScenarioName: s.Name,
			Scores:       ScoreRun(s, run),
			Manifested:   run.Manifested,
			InjectError:  run.InjectError,
		}
		sum.Results = append(sum.Results, res)

		sum.Scenarios++
		if run.Manifested {
			sum.Manifested++
		}
		if run.InjectError != "" {
			sum.InjectFailures++
		}

		for _, sc := range res.Scores {
			if sc.Skipped {
				continue
			}
			totals[sc.Name] += sc.Value
			counts[sc.Name]++
		}
	}

	for name, total := range totals {
		sum.Means[name] = total / float64(counts[name])
	}

	// A control scenario has no fault to manifest, so counting it in the
	// denominator would cap the efficacy rate below 1 on any pack that has
	// controls — and a pack without controls cannot measure invention.
	injectable := 0
	manifested := 0
	for _, run := range runs {
		s, ok := pack.ByID(run.ScenarioID)
		if !ok || len(s.Faults) == 0 {
			continue
		}
		injectable++
		if run.Manifested {
			manifested++
		}
	}
	if injectable > 0 {
		sum.EfficacyRate = float64(manifested) / float64(injectable)
	}

	return sum, nil
}

// MeasureNames returns the measures this summary actually has a mean for, in
// report order, so a scorecard's rows and its columns are ordered the same way
// and two runs of the same pack are diffable.
//
// Measures outside the default set — a caller's own, passed to Summarize —
// have no report order to follow, so they come last, sorted.
func (s Summary) MeasureNames() []string {
	names := make([]string, 0, len(s.Means))
	known := map[string]bool{}
	for _, n := range MeasureNames() {
		known[n] = true
		if _, ok := s.Means[n]; ok {
			names = append(names, n)
		}
	}

	extra := make([]string, 0, len(s.Means))
	for n := range s.Means {
		if !known[n] {
			extra = append(extra, n)
		}
	}
	sort.Strings(extra)
	return append(names, extra...)
}
