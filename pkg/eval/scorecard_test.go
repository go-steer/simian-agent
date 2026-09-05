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
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func renderedSummary() Summary {
	return Summary{
		Subject: "core-sre-agent",
		Pack:    "parity",
		Results: []ScenarioResult{
			{
				ScenarioID:   "s-1",
				ScenarioName: "image pull backoff",
				Manifested:   true,
				Scores: []Score{
					{Name: MeasureRecall, Value: 1, Unit: UnitFraction},
					{Name: MeasureRootCause, Skipped: true, Unit: UnitFraction, Comment: "scenario marks no root cause"},
					{Name: MeasureSeverity, Value: 1, Unit: UnitFraction},
					{Name: MeasureHallucination, Value: 1, Unit: UnitFraction},
					{Name: MeasureTimeToDetect, Value: 42.5, Unit: UnitSeconds},
					{Name: MeasureTimeToRemediate, Skipped: true, Unit: UnitSeconds},
				},
			},
			{
				ScenarioID:   "s-2",
				ScenarioName: "oom cascade",
				InjectError:  "fault f-2 has no passing efficacy record",
				Scores: []Score{
					{Name: MeasureRecall, Skipped: true, Comment: "injection failed: fault f-2 has no passing efficacy record"},
				},
			},
		},
		Means: map[string]float64{
			MeasureRecall:       1,
			MeasureSeverity:     1,
			MeasureTimeToDetect: 42.5,
		},
		Scenarios:      2,
		Manifested:     1,
		InjectFailures: 1,
		EfficacyRate:   0.5,
	}
}

func TestWriteTextPutsTheHarnessBeforeTheSubject(t *testing.T) {
	var buf bytes.Buffer
	if err := renderedSummary().WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	out := buf.String()

	harness := strings.Index(out, "harness")
	means := strings.Index(out, "means")
	if harness < 0 || means < 0 || harness > means {
		t.Errorf("harness section must come before the means:\n%s", out)
	}
	if !strings.Contains(out, "subject=core-sre-agent") || !strings.Contains(out, "pack=parity") {
		t.Errorf("header does not say what was graded:\n%s", out)
	}
	if !strings.Contains(out, "efficacy rate    0.50") {
		t.Errorf("efficacy rate missing:\n%s", out)
	}
}

// Below the threshold the scorecard has to say the numbers are unmeasured
// rather than poor, in the rendering itself — a reader who scrolls past the
// exit code still sees it.
func TestWriteTextWarnsWhenTheHarnessDidNotManifest(t *testing.T) {
	var buf bytes.Buffer
	if err := renderedSummary().WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if !strings.Contains(buf.String(), "unmeasured, not as poor") {
		t.Errorf("no warning on a 0.50 efficacy rate:\n%s", buf.String())
	}

	clean := renderedSummary()
	clean.EfficacyRate = 1
	buf.Reset()
	if err := clean.WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if strings.Contains(buf.String(), "unmeasured, not as poor") {
		t.Errorf("warned on a clean run:\n%s", buf.String())
	}
}

// A skipped measure prints as a dash. A column of 0.00 is exactly how a
// reader concludes a subject failed at something it was never asked.
func TestWriteTextPrintsSkippedMeasuresAsADash(t *testing.T) {
	var buf bytes.Buffer
	if err := renderedSummary().WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	row := scenarioRow(t, buf.String(), "s-1")
	if !strings.Contains(row, "-") {
		t.Errorf("row %q has no dash for the skipped measures", row)
	}
	if strings.Contains(row, "0.00") {
		t.Errorf("row %q prints a skipped measure as a zero", row)
	}

	// s-2 was never scored at all, so most of its measures are absent rather
	// than skipped. Absent has to read the same way: no number.
	unscored := scenarioRow(t, buf.String(), "s-2")
	if strings.Contains(unscored, "0.00") || strings.Contains(unscored, "0.0s") {
		t.Errorf("row %q prints a measure that was never taken as a zero", unscored)
	}
}

// The means section lists what was measured, not what could have been. A mean
// printed for a measure no scenario produced is a number with nothing behind
// it.
func TestMeansListOnlyTheMeasuresTheSummaryHas(t *testing.T) {
	sum := renderedSummary()
	var buf bytes.Buffer
	if err := sum.WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	_, means, ok := strings.Cut(buf.String(), "means")
	if !ok {
		t.Fatalf("no means section:\n%s", buf.String())
	}
	means, _, ok = strings.Cut(means, "scenarios")
	if !ok {
		t.Fatalf("means section does not end:\n%s", buf.String())
	}

	for _, n := range MeasureNames() {
		_, want := sum.Means[n]
		if got := strings.Contains(means, n); got != want {
			t.Errorf("means section lists %q = %v, want %v:\n%s", n, got, want, means)
		}
	}
}

// The means and the scorecard columns are read side by side, so they are
// ordered the same way: the order the measures report in.
func TestMeansFollowMeasureOrder(t *testing.T) {
	sum := renderedSummary()
	sum.Means[MeasureRootCause] = 0.5
	sum.Means["custom-measure"] = 1
	sum.Means["another-custom"] = 1

	want := []string{
		MeasureRecall, MeasureRootCause, MeasureSeverity, MeasureTimeToDetect,
		"another-custom", "custom-measure",
	}
	if !slices.Equal(sum.MeasureNames(), want) {
		t.Errorf("MeasureNames() = %v, want %v", sum.MeasureNames(), want)
	}
}

// Seconds and fractions are different things, and printing "42.50" beside
// "1.00" invites the reader to average them.
func TestWriteTextMarksSecondsAsSeconds(t *testing.T) {
	var buf bytes.Buffer
	if err := renderedSummary().WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if !strings.Contains(buf.String(), "42.5s") {
		t.Errorf("time_to_detect is not rendered in seconds:\n%s", buf.String())
	}
}

// An unscored scenario says so in words. A row of dashes alone reads like a
// scenario that asked nothing, which is the opposite of what happened.
func TestWriteTextSpellsOutAnInjectFailure(t *testing.T) {
	var buf bytes.Buffer
	if err := renderedSummary().WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "s-2: NOT SCORED") {
		t.Errorf("inject failure is not called out:\n%s", out)
	}
	if !strings.Contains(out, "no passing efficacy record") {
		t.Errorf("the reason is missing:\n%s", out)
	}
}

func scenarioRow(t *testing.T, out, id string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), id+" ") {
			return line
		}
	}
	t.Fatalf("no row for %q in:\n%s", id, out)
	return ""
}

func TestWriteTextOnAnEmptySummary(t *testing.T) {
	var buf bytes.Buffer
	if err := (Summary{}).WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if !strings.Contains(buf.String(), "subject=-") {
		t.Errorf("an empty summary should still render:\n%s", buf.String())
	}
}

func TestWriteJSONRoundTrips(t *testing.T) {
	var buf bytes.Buffer
	want := renderedSummary()
	if err := want.WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	var got Summary
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Subject != want.Subject || got.EfficacyRate != want.EfficacyRate || len(got.Results) != len(want.Results) {
		t.Errorf("round trip lost data:\n got=%+v\nwant=%+v", got, want)
	}
	if got.Results[0].Scores[1].Skipped != true {
		t.Error("skipped did not survive the round trip; it would read as a zero")
	}
}

// The column order of the scorecard is the report order of the measures, so
// adding a measure cannot silently reorder a scorecard someone is diffing.
func TestMeasureNamesMatchesTheDefaultMeasures(t *testing.T) {
	var want []string
	for _, m := range DefaultMeasures() {
		want = append(want, m.Name())
	}
	if !slices.Equal(MeasureNames(), want) {
		t.Errorf("MeasureNames() = %v, want %v", MeasureNames(), want)
	}
}

func TestScorecardColumnsAreInMeasureOrder(t *testing.T) {
	var buf bytes.Buffer
	if err := renderedSummary().WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	header := ""
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "scenario ") && strings.Contains(line, MeasureRecall) {
			header = line
			break
		}
	}
	if header == "" {
		t.Fatalf("no header row:\n%s", buf.String())
	}
	at := -1
	for _, name := range MeasureNames() {
		i := strings.Index(header, name)
		if i < 0 {
			t.Errorf("header is missing %q: %q", name, header)
			continue
		}
		if i < at {
			t.Errorf("column %q is out of measure order in %q", name, header)
		}
		at = i
	}
}
