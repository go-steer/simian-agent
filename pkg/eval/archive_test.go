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
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/simian-agent/pkg/scenario"
)

// The fixtures under testdata/archive are real simian-eval runs, trimmed:
//
//   - dataplane-lookout: k8s-lookout v0.20.0 on two dataplane scenarios — the
//     floor.
//   - dataplane-vantage: sre-agent on the same two, which on
//     partition-one-way scored below lookout.
//   - gke-parity-partial: sre-agent on six parity scenarios on GKE, where
//     parity-cascade's efficacy gate failed and parity-unbound-volume never
//     got a fault — the run's efficacy rate is 0.67, under the minimum.
//
// A GKE context name in a subject's stderr was replaced; nothing that is
// scored was changed.
var archiveFixtures = []string{"dataplane-lookout", "dataplane-vantage", "gke-parity-partial"}

func fixtureInput(t *testing.T, name string) ArchiveInput {
	t.Helper()
	dir := filepath.Join("testdata", "archive", name)
	auditBytes, err := os.ReadFile(filepath.Join(dir, "audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	runBytes, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	meta, err := ReadRunMeta(bytes.NewReader(auditBytes))
	if err != nil {
		t.Fatal(err)
	}
	pack, err := scenario.Builtin(meta.Pack)
	if err != nil {
		t.Fatalf("%s: pack %q: %v", name, meta.Pack, err)
	}
	return ArchiveInput{Label: name, Source: dir, Pack: pack, Audit: auditBytes, RunFile: runBytes}
}

func buildFixtureArchive(t *testing.T, opts ArchiveOptions) (Archive, []ArchiveInput) {
	t.Helper()
	var inputs []ArchiveInput
	for _, name := range archiveFixtures {
		inputs = append(inputs, fixtureInput(t, name))
	}
	if opts.MinEfficacy == 0 {
		opts.MinEfficacy = DefaultMinEfficacy
	}
	doc, err := BuildArchive(inputs, opts)
	if err != nil {
		t.Fatalf("BuildArchive: %v", err)
	}
	// Round-trip through JSON, so the test reads what the web UI reads.
	var buf bytes.Buffer
	if err := doc.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var back Archive
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("the document does not read back: %v", err)
	}
	return back, inputs
}

// ownSummary is the run's own scorecard, computed the way `simian evaluate`
// and simian-eval compute it.
func ownSummary(t *testing.T, in ArchiveInput) Summary {
	t.Helper()
	facts, err := ReadAudit(bytes.NewReader(in.Audit))
	if err != nil {
		t.Fatal(err)
	}
	rf, err := ReadRunFile(bytes.NewReader(in.RunFile))
	if err != nil {
		t.Fatal(err)
	}
	runs, err := Join(in.Pack, facts, rf)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := Summarize(rf.Subject, in.Pack, runs)
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

func findScenario(doc Archive, id string) *ArchiveScenario {
	for i := range doc.Scenarios {
		if doc.Scenarios[i].ID == id {
			return &doc.Scenarios[i]
		}
	}
	return nil
}

// Every number in the combined document is the number the run's own scorecard
// printed: the archive arranges scores, it does not compute them.
func TestTheArchiveCarriesEachRunsOwnScorecard(t *testing.T) {
	doc, inputs := buildFixtureArchive(t, ArchiveOptions{GeneratedAt: time.Unix(0, 0)})

	if doc.Format != ArchiveFormat || doc.Version != ArchiveVersion {
		t.Fatalf("format = %q v%d", doc.Format, doc.Version)
	}
	if len(doc.Runs) != len(inputs) {
		t.Fatalf("runs = %d, want %d", len(doc.Runs), len(inputs))
	}
	for i, in := range inputs {
		own := ownSummary(t, in)
		run := doc.Runs[i]
		if run.Subject != own.Subject || run.Pack != own.Pack {
			t.Errorf("%s: subject/pack = %s/%s, want %s/%s", in.Label, run.Subject, run.Pack, own.Subject, own.Pack)
		}
		if run.EfficacyRate != own.EfficacyRate || run.Scenarios != own.Scenarios ||
			run.Manifested != own.Manifested || run.InjectFailures != own.InjectFailures {
			t.Errorf("%s: harness numbers = %+v, want those of %+v", in.Label, run, own)
		}
		if !reflect.DeepEqual(run.Means, own.Means) {
			t.Errorf("%s: means = %v, want %v", in.Label, run.Means, own.Means)
		}
		for _, res := range own.Results {
			row := findScenario(doc, res.ScenarioID)
			if row == nil {
				t.Fatalf("%s: scenario %s is not in the document", in.Label, res.ScenarioID)
			}
			cell := row.Results[run.ID]
			if cell == nil {
				t.Fatalf("%s: no cell for %s", in.Label, res.ScenarioID)
			}
			if !reflect.DeepEqual(cell.Scores, res.Scores) {
				t.Errorf("%s/%s: scores = %+v, want %+v", in.Label, res.ScenarioID, cell.Scores, res.Scores)
			}
			if cell.Manifested != res.Manifested || cell.InjectError != res.InjectError {
				t.Errorf("%s/%s: efficacy = %v %q, want %v %q", in.Label, res.ScenarioID, cell.Manifested, cell.InjectError, res.Manifested, res.InjectError)
			}
			head, _ := res.Score(row.Headline)
			switch {
			case !res.Manifested || head.Skipped:
				if cell.Value != nil {
					t.Errorf("%s/%s: value = %v, want none", in.Label, res.ScenarioID, *cell.Value)
				}
			case cell.Value == nil || *cell.Value != head.Value:
				t.Errorf("%s/%s: value = %v, want %v", in.Label, res.ScenarioID, cell.Value, head.Value)
			}
		}
	}

	// The fixtures' own text scorecards, spot-checked by hand.
	part := findScenario(doc, "dataplane-partition-one-way")
	if v := part.Results["r1"].Value; v == nil || *v != 0.5 {
		t.Errorf("lookout on partition-one-way = %v, want 0.50", v)
	}
	if v := part.Results["r2"].Value; v == nil || *v != 0 {
		t.Errorf("sre-agent on partition-one-way = %v, want 0.00", v)
	}
	if r := doc.Runs[2].EfficacyRate; math.Abs(r-2.0/3) > 1e-9 {
		t.Errorf("gke-parity-partial efficacy = %v, want 0.67", r)
	}
}

// Rule 1: a fault that did not land measured nothing about anyone. The cell
// carries no value — never a zero — and the run says it fell below the minimum.
func TestAFailedEfficacyGateIsNotMeasuredNotZero(t *testing.T) {
	doc, _ := buildFixtureArchive(t, ArchiveOptions{})

	for _, id := range []string{"parity-cascade", "parity-unbound-volume"} {
		row := findScenario(doc, id)
		if row == nil {
			t.Fatalf("%s missing", id)
		}
		cell := row.Results["r3"]
		if cell.Manifested {
			t.Errorf("%s: manifested, want not", id)
		}
		if cell.Value != nil {
			t.Errorf("%s: value = %v, want none (not measured)", id, *cell.Value)
		}
		if cell.InjectError == "" {
			t.Errorf("%s: no inject error to say why it was not measured", id)
		}
		if cell.BelowFloor {
			t.Errorf("%s: an unmeasured cell is flagged below the floor", id)
		}
		for _, sc := range cell.Scores {
			if !sc.Skipped {
				t.Errorf("%s: %s scored %v on a fault that did not land", id, sc.Name, sc.Value)
			}
		}
	}
	cascade := findScenario(doc, "parity-cascade").Results["r3"]
	if len(cascade.Faults) != 1 || len(cascade.Faults[0].Efficacy) == 0 {
		t.Fatalf("cascade fault trail = %+v, want the failed gate", cascade.Faults)
	}
	failed := false
	for _, e := range cascade.Faults[0].Efficacy {
		failed = failed || (!e.Passed && e.Probe == "simian-restarts-climbing")
	}
	if !failed {
		t.Errorf("the failed efficacy record is not in the trail: %+v", cascade.Faults[0].Efficacy)
	}
	if !doc.Runs[2].BelowMinEfficacy {
		t.Error("a run at 0.67 efficacy is not flagged below the minimum")
	}
	if doc.Runs[0].BelowMinEfficacy || doc.Runs[1].BelowMinEfficacy {
		t.Error("a run at full efficacy is flagged below the minimum")
	}
}

// Rule 2: the deterministic detector is the floor, and a subject below it on a
// row is flagged — only there, and only against a measured floor.
func TestASubjectBelowTheFloorIsFlagged(t *testing.T) {
	doc, _ := buildFixtureArchive(t, ArchiveOptions{})

	if doc.Floor.Rule != "auto" || !reflect.DeepEqual(doc.Floor.Runs, []string{"r1"}) ||
		!reflect.DeepEqual(doc.Floor.Subjects, []string{"lookout-v0.20.0"}) {
		t.Fatalf("floor = %+v, want lookout's run r1, by name", doc.Floor)
	}
	if !doc.Runs[0].Floor || doc.Runs[1].Floor {
		t.Errorf("run floor marks = %v %v", doc.Runs[0].Floor, doc.Runs[1].Floor)
	}

	part := findScenario(doc, "dataplane-partition-one-way").Results["r2"]
	if !part.BelowFloor || part.FloorValue == nil || *part.FloorValue != 0.5 {
		t.Errorf("partition-one-way: below=%v floor=%v, want flagged against 0.50", part.BelowFloor, part.FloorValue)
	}
	lat := findScenario(doc, "dataplane-latency-not-saturation").Results["r2"]
	if lat.BelowFloor {
		t.Error("latency-not-saturation: equal to the floor, flagged below it")
	}
	if findScenario(doc, "dataplane-latency-not-saturation").Results["r1"].BelowFloor {
		t.Error("the floor is flagged below itself")
	}
	// Parity has no floor run, so nothing there can be below one.
	for _, s := range doc.Scenarios {
		if s.Pack != "parity" {
			continue
		}
		for _, c := range s.Results {
			if c.BelowFloor || c.FloorValue != nil {
				t.Errorf("%s: flagged against a floor that did not run it", s.ID)
			}
		}
	}
	if !doc.MixedPacks || !reflect.DeepEqual(doc.Packs, []string{"dataplane", "parity"}) {
		t.Errorf("packs = %v mixed=%v", doc.Packs, doc.MixedPacks)
	}
}

func TestTheFloorCanBeNamedOrTurnedOff(t *testing.T) {
	named, _ := buildFixtureArchive(t, ArchiveOptions{Floor: []string{"sre-agent"}})
	if named.Floor.Rule != "flag" || len(named.Floor.Runs) != 2 {
		t.Errorf("--floor sre-agent: %+v", named.Floor)
	}
	// Now lookout is the subject, and it is not below sre-agent's best.
	if findScenario(named, "dataplane-partition-one-way").Results["r1"].BelowFloor {
		t.Error("lookout flagged below a floor it beat")
	}

	none, _ := buildFixtureArchive(t, ArchiveOptions{Floor: []string{}})
	if none.Floor.Rule != "none" || len(none.Floor.Runs) != 0 {
		t.Errorf("--floor '': %+v", none.Floor)
	}
	if findScenario(none, "dataplane-partition-one-way").Results["r2"].BelowFloor {
		t.Error("flagged below a floor that was turned off")
	}
}

func TestFloorSubjectNames(t *testing.T) {
	for name, want := range map[string]bool{
		"lookout": true, "lookout-v0.20.0": true, "k8s-lookout": true, "K8s-Lookout-v1": true,
		"noop": false, "sre-agent": false, "lookoutish": false, "my-lookout": false,
	} {
		if got := IsFloorSubject(name); got != want {
			t.Errorf("IsFloorSubject(%q) = %v, want %v", name, got, want)
		}
	}
}

// The null subject is below every floor by construction; flagging it says
// nothing.
func TestTheNullSubjectIsNeverFlagged(t *testing.T) {
	row := &ArchiveScenario{Results: map[string]*ArchiveResult{
		"r1": {Manifested: true, Value: ptr(0.5)},
		"r2": {Manifested: true, Value: ptr(0.0)},
		"r3": {Manifested: true, Value: ptr(0.0)},
	}}
	markFloor(row, map[string]bool{"r1": true}, map[string]bool{"r2": true})
	if row.Results["r2"].BelowFloor {
		t.Error("the null subject is flagged below the floor")
	}
	if !row.Results["r3"].BelowFloor {
		t.Error("a subject at 0 against a floor at 0.5 is not flagged")
	}
}

func TestRunMetaAndTrimming(t *testing.T) {
	in := fixtureInput(t, "dataplane-vantage")
	meta, err := ReadRunMeta(bytes.NewReader(in.Audit))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Pack != "dataplane" || meta.Subject != "sre-agent" || meta.Started.IsZero() || !meta.Ended.After(meta.Started) {
		t.Errorf("meta = %+v", meta)
	}

	long := strings.Repeat("é", 3000)
	got := trimText(long, 101)
	if !strings.HasSuffix(got, "more bytes]") || !strings.HasPrefix(got, strings.Repeat("é", 50)) {
		t.Errorf("trimText = %q…", got[:20])
	}
	if !json.Valid([]byte(`"` + strings.Split(got, "…")[0] + `"`)) {
		t.Error("trimText cut a UTF-8 sequence")
	}
}

func TestNoRunsIsAnError(t *testing.T) {
	if _, err := BuildArchive(nil, ArchiveOptions{}); err == nil {
		t.Error("BuildArchive(nil) = nil error")
	}
}

func ptr(v float64) *float64 { return &v }
