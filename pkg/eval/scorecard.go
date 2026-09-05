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
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// DefaultMinEfficacy is the efficacy rate below which a scorecard reports the
// harness rather than the subject.
//
// core-sre-agent's own live tier refuses to publish below a threshold, and the
// reasoning is worth repeating here because it is easy to talk yourself out
// of. Every measure on a scorecard assumes the cluster was broken the way the
// scenario said. Where it was not, a zero means "there was nothing to find"
// and not "the subject missed it", and nothing downstream can tell those
// apart. A suite that mostly did not manifest has not measured a poor subject;
// it has failed to measure anything, and publishing a number for it is worse
// than publishing none.
//
// 0.8 rather than 1.0 because a single flaky gate in a pack of a dozen is a
// bad afternoon, not a broken rig, and the affected scenario is already
// excluded from every mean.
const DefaultMinEfficacy = 0.8

// WriteJSON writes the summary as indented JSON.
func (s Summary) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

// WriteText renders the scorecard for a terminal.
//
// The harness's own numbers come first, before any measure of the subject.
// That ordering is the point: a reader who takes the recall figure off a suite
// that half-manifested has drawn a conclusion the data does not support, and
// the layout should make that hard rather than possible.
func (s Summary) WriteText(w io.Writer) error {
	var b strings.Builder

	fmt.Fprintf(&b, "scorecard — subject=%s pack=%s\n\n", orDash(s.Subject), orDash(s.Pack))

	fmt.Fprintf(&b, "harness\n")
	fmt.Fprintf(&b, "  scenarios        %d\n", s.Scenarios)
	fmt.Fprintf(&b, "  manifested       %d\n", s.Manifested)
	fmt.Fprintf(&b, "  inject failures  %d\n", s.InjectFailures)
	fmt.Fprintf(&b, "  efficacy rate    %s\n", formatFraction(s.EfficacyRate))
	if s.EfficacyRate < DefaultMinEfficacy {
		fmt.Fprintf(&b, "  ** the harness did not break the cluster reliably; read the measures below as unmeasured, not as poor **\n")
	}

	names := s.MeasureNames()
	if len(names) > 0 {
		fmt.Fprintf(&b, "\nmeans\n")
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		for _, n := range names {
			fmt.Fprintf(tw, "  %s\t%s\n", n, formatValue(n, s.Means[n]))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	if len(s.Results) > 0 {
		fmt.Fprintf(&b, "\nscenarios\n")
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		header := []string{"  scenario", "name"}
		header = append(header, MeasureNames()...)
		fmt.Fprintln(tw, strings.Join(header, "\t"))
		for _, r := range s.Results {
			row := []string{"  " + r.ScenarioID, r.ScenarioName}
			for _, n := range MeasureNames() {
				row = append(row, cell(r, n))
			}
			fmt.Fprintln(tw, strings.Join(row, "\t"))
		}
		if err := tw.Flush(); err != nil {
			return err
		}

		for _, r := range s.Results {
			if r.InjectError != "" {
				fmt.Fprintf(&b, "\n  %s: NOT SCORED — %s\n", r.ScenarioID, r.InjectError)
			}
		}
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// cell renders one measure for one scenario. A skipped measure prints as a
// dash and not as 0.00, because the two mean opposite things and a column of
// zeros is exactly how a reader concludes a subject failed at something it was
// never asked.
func cell(r ScenarioResult, measure string) string {
	sc, ok := r.Score(measure)
	switch {
	case !ok:
		return "-"
	case sc.Skipped:
		return "-"
	default:
		return formatValue(measure, sc.Value)
	}
}

// MeasureNames returns the per-run measures in report order. Package-level
// counterpart to Summary.MeasureNames, which reports only what a given summary
// actually has.
func MeasureNames() []string {
	measures := DefaultMeasures()
	out := make([]string, 0, len(measures))
	for _, m := range measures {
		out = append(out, m.Name())
	}
	return out
}

// formatValue prints a measure the way its unit reads. Seconds are not
// fractions, and printing "42.10" next to "0.83" invites the reader to average
// them.
func formatValue(measure string, v float64) string {
	switch measure {
	case MeasureTimeToDetect, MeasureTimeToRemediate:
		return fmt.Sprintf("%.1fs", v)
	default:
		return formatFraction(v)
	}
}

func formatFraction(v float64) string { return fmt.Sprintf("%.2f", v) }

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
