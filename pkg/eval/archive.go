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
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/scenario"
	"github.com/go-steer/simian-agent/pkg/simian"
)

// An Archive is several runs' scorecards combined into one self-describing
// JSON document: the scorecard grid (scenarios × subjects) and everything a
// drill-down shows, with no backend behind it. The web UI renders it from a
// file, which is what makes a run attachable to a CI job, e-mailable and
// diffable — see "One site, two data sources" in web-ui-design.md.
//
// Nothing in it is scored here. Every number comes from Join and Summarize,
// the same calls `simian evaluate` and `simian-eval` make, so a cell in the
// grid is the number that run's own text scorecard printed. What this file
// adds is arrangement — rows, columns, the floor — and the evidence a reader
// needs beside a number: the fault, the ground truth, the subject's report.

// Archive format identifiers. Version changes when a reader written for the
// previous one would misread the document, not when a field is added.
const (
	ArchiveFormat  = "simian.scorecard"
	ArchiveVersion = 1
)

// Archive trimming limits: enough to read, not so much that one runaway
// subject makes the document unopenable.
const (
	archiveMaxFindings = 200
	archiveMaxText     = 4000
	archiveMaxObserved = 400
)

// ArchiveInput is one run directory's artifacts, already read.
type ArchiveInput struct {
	// Label names the run in the grid; usually the directory's base name.
	Label string
	// Source is where the run came from, recorded as given.
	Source string
	// Pack is the ground truth the run is scored against.
	Pack scenario.Pack
	// Audit and RunFile are the two artifacts, verbatim.
	Audit   []byte
	RunFile []byte
	// Evidence names other files the run directory holds (a subject's own
	// transcripts), listed so a reader knows where to look; not embedded.
	Evidence []string
}

// ArchiveOptions are the knobs on combining.
type ArchiveOptions struct {
	// Floor names the subjects that are the deterministic-detector floor.
	// Nil means IsFloorSubject decides; an empty non-nil slice means no
	// floor.
	Floor []string
	// MinEfficacy is the efficacy rate below which a run measured the
	// harness rather than the subject.
	MinEfficacy float64
	// GeneratedAt and Generator describe the document itself.
	GeneratedAt time.Time
	Generator   string
}

// Archive is the combined document.
type Archive struct {
	Format      string    `json:"format"`
	Version     int       `json:"version"`
	GeneratedAt time.Time `json:"generated_at"`
	Generator   string    `json:"generator"`

	// Headline is the measure each grid cell shows: recall, and on a healthy
	// control (which has nothing to recall) hallucinated_fault. The text
	// scorecard leads with recall for the same reason.
	Headline       string           `json:"headline"`
	ControlMeasure string           `json:"control_headline"`
	Measures       []ArchiveMeasure `json:"measures"`
	MinEfficacy    float64          `json:"min_efficacy"`

	// Packs are the packs the runs used. More than one is allowed; the grid
	// groups scenarios by pack and MixedPacks says so.
	Packs      []string `json:"packs"`
	MixedPacks bool     `json:"mixed_packs"`

	Floor ArchiveFloor `json:"floor"`

	Runs      []ArchiveRun      `json:"runs"`
	Scenarios []ArchiveScenario `json:"scenarios"`
}

// ArchiveMeasure describes one measure so a reader needs no table of its own.
type ArchiveMeasure struct {
	Name string `json:"name"`
	Unit Unit   `json:"unit"`
}

// ArchiveFloor says which runs are the floor and how that was decided.
type ArchiveFloor struct {
	// Rule is "auto" (subject name), "flag" (--floor) or "none".
	Rule     string   `json:"rule"`
	Subjects []string `json:"subjects"`
	Runs     []string `json:"runs"`
}

// ArchiveRun is one run's header: one column of the grid.
type ArchiveRun struct {
	ID      string    `json:"id"`
	Label   string    `json:"label"`
	Source  string    `json:"source,omitempty"`
	Subject string    `json:"subject"`
	Pack    string    `json:"pack"`
	Cluster string    `json:"cluster,omitempty"`
	Started time.Time `json:"started_at,omitzero"`
	Ended   time.Time `json:"ended_at,omitzero"`
	Floor   bool      `json:"floor"`
	// Null marks the noop subject, which answers "nothing is wrong" to
	// everything: the zero baseline. It is never flagged below the floor,
	// since it is below everything by construction.
	Null     bool     `json:"null_subject,omitempty"`
	Evidence []string `json:"evidence,omitempty"`

	// The harness's report card, from Summarize.
	Scenarios        int                `json:"scenarios"`
	Manifested       int                `json:"manifested"`
	InjectFailures   int                `json:"inject_failures"`
	TeardownFailures int                `json:"teardown_failures"`
	EfficacyRate     float64            `json:"efficacy_rate"`
	BelowMinEfficacy bool               `json:"below_min_efficacy"`
	Means            map[string]float64 `json:"means"`
}

// ArchiveScenario is one row of the grid and its drill-down.
type ArchiveScenario struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Pack     string `json:"pack"`
	Prompt   string `json:"prompt,omitempty"`
	Control  bool   `json:"control"`
	Headline string `json:"headline"`

	Faults      []ArchiveFault     `json:"faults"`
	GroundTruth ArchiveGroundTruth `json:"ground_truth"`

	// Results is keyed by run ID. A run that did not include this scenario
	// has no entry: not run, which is neither a zero nor "not measured".
	Results map[string]*ArchiveResult `json:"results"`
}

// ArchiveFault is a fault as the pack declares it.
type ArchiveFault struct {
	Engine   simian.Engine      `json:"engine"`
	Kind     string             `json:"kind"`
	Targets  []simian.TargetRef `json:"targets,omitempty"`
	Spec     map[string]any     `json:"spec,omitempty"`
	Duration string             `json:"duration,omitempty"`
	Probes   []string           `json:"probes,omitempty"`
}

// ArchiveGroundTruth is what a correct report says.
type ArchiveGroundTruth struct {
	Expect   []scenario.ExpectedFinding `json:"expect"`
	AlsoTrue []string                   `json:"also_true,omitempty"`
	Severity scenario.Severity          `json:"severity,omitempty"`
}

// ArchiveResult is one subject's cell: whether the fault landed in that run,
// its scores, and what it said.
type ArchiveResult struct {
	// Manifested is efficacy for this run. False means not measured: every
	// score is skipped, and the grid must not show a number.
	Manifested    bool   `json:"manifested"`
	InjectError   string `json:"inject_error,omitempty"`
	SubjectError  string `json:"subject_error,omitempty"`
	TeardownError string `json:"teardown_error,omitempty"`

	InjectedAt time.Time `json:"injected_at,omitzero"`
	DetectedAt time.Time `json:"detected_at,omitzero"`
	ClearedAt  time.Time `json:"cleared_at,omitzero"`

	// Scores are this run's scores for the scenario, verbatim from Summarize.
	Scores []Score `json:"scores"`

	// Value is the headline measure's value; nil when it was not measured.
	Value *float64 `json:"value"`
	// FloorValue is the best floor run's headline value on this row, and
	// BelowFloor says this (non-floor) run scored under it: the subject did
	// worse than the deterministic tool it could have used.
	FloorValue *float64 `json:"floor_value,omitempty"`
	BelowFloor bool     `json:"below_floor"`

	Report        *Report `json:"report,omitempty"`
	FindingsTotal int     `json:"findings_total,omitempty"`

	// Faults is what the audit log says happened to each fault in this run.
	Faults []ArchiveFaultTrail `json:"faults,omitempty"`
}

// ArchiveFaultTrail is one fault's audit trail in one run.
type ArchiveFaultTrail struct {
	UID       string            `json:"uid"`
	Engine    string            `json:"engine,omitempty"`
	Kind      string            `json:"kind,omitempty"`
	EngineUID string            `json:"engine_uid,omitempty"`
	AppliedAt time.Time         `json:"applied_at,omitzero"`
	Deadline  string            `json:"deadline,omitempty"`
	ClearedAt time.Time         `json:"cleared_at,omitzero"`
	Problem   string            `json:"problem,omitempty"`
	Efficacy  []ArchiveEfficacy `json:"efficacy,omitempty"`
}

// ArchiveEfficacy is one efficacy-gate record.
type ArchiveEfficacy struct {
	Probe    string    `json:"probe"`
	Passed   bool      `json:"passed"`
	Expected string    `json:"expected,omitempty"`
	Observed string    `json:"observed,omitempty"`
	At       time.Time `json:"at,omitzero"`
}

// RunMeta is what a run's audit log records about the run itself, apart from
// any fault.
type RunMeta struct {
	Pack    string
	Subject string
	Cluster string
	Started time.Time
	Ended   time.Time
}

// ReadRunMeta reads the pack, subject and cluster a run recorded on its
// eval.scenario_started lines, and the span of its audit log.
func ReadRunMeta(r io.Reader) (RunMeta, error) {
	var m RunMeta
	err := eachAuditLine(r, func(l auditLine) {
		at := l.at()
		if !at.IsZero() {
			if m.Started.IsZero() || at.Before(m.Started) {
				m.Started = at
			}
			if at.After(m.Ended) {
				m.Ended = at
			}
		}
		if l.Event != audit.EventEvalScenarioStarted {
			return
		}
		if m.Pack == "" {
			m.Pack = payloadString(l.Payload, "pack")
		}
		if m.Subject == "" {
			m.Subject = payloadString(l.Payload, "subject")
		}
		if m.Cluster == "" {
			m.Cluster = payloadString(l.Payload, "cluster")
		}
	})
	return m, err
}

func eachAuditLine(r io.Reader, fn func(auditLine)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var l auditLine
		if err := json.Unmarshal(scanner.Bytes(), &l); err != nil || l.Event == "" {
			continue
		}
		fn(l)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("eval: reading audit log: %w", err)
	}
	return nil
}

// NullSubject is the name simian-eval gives the noop: subject.
const NullSubject = "noop"

// IsFloorSubject is the default rule for which subjects are the floor: the
// deterministic detector, k8s-lookout. simian-eval names a `lookout:` subject
// after its binary, so that is "lookout", "k8s-lookout", or either with a
// version suffix ("lookout-v0.20.0"). The noop subject is not a floor — it
// scores zero everywhere by construction, so nothing can fall below it.
func IsFloorSubject(name string) bool {
	n := strings.ToLower(name)
	for _, base := range []string{"lookout", "k8s-lookout"} {
		if n == base || strings.HasPrefix(n, base+"-") {
			return true
		}
	}
	return false
}

// BuildArchive scores each input exactly as `simian evaluate` would and
// combines the scorecards.
func BuildArchive(inputs []ArchiveInput, opts ArchiveOptions) (Archive, error) {
	if len(inputs) == 0 {
		return Archive{}, errors.New("eval: no runs to combine")
	}
	doc := Archive{
		Format:         ArchiveFormat,
		Version:        ArchiveVersion,
		GeneratedAt:    opts.GeneratedAt.UTC(),
		Generator:      opts.Generator,
		Headline:       MeasureRecall,
		ControlMeasure: MeasureHallucination,
		MinEfficacy:    opts.MinEfficacy,
		Floor:          ArchiveFloor{Rule: "auto", Subjects: []string{}, Runs: []string{}},
	}
	for _, m := range DefaultMeasures() {
		unit := UnitFraction
		if m.Name() == MeasureTimeToDetect || m.Name() == MeasureTimeToRemediate {
			unit = UnitSeconds
		}
		doc.Measures = append(doc.Measures, ArchiveMeasure{Name: m.Name(), Unit: unit})
	}
	floorSet := map[string]bool{}
	if opts.Floor != nil {
		doc.Floor.Rule = "flag"
		if len(opts.Floor) == 0 {
			doc.Floor.Rule = "none"
		}
		for _, f := range opts.Floor {
			floorSet[f] = true
		}
	}
	isFloor := func(subject string) bool {
		if opts.Floor != nil {
			return floorSet[subject]
		}
		return IsFloorSubject(subject)
	}

	rows := map[string]*ArchiveScenario{}
	var order []string
	labels := map[string]int{}
	packSeen := map[string]bool{}
	floorSubjects := map[string]bool{}

	for i, in := range inputs {
		id := fmt.Sprintf("r%d", i+1)
		where := in.Source
		if where == "" {
			where = id
		}

		facts, err := ReadAudit(bytes.NewReader(in.Audit))
		if err != nil {
			return Archive{}, fmt.Errorf("%s: %w", where, err)
		}
		rf, err := ReadRunFile(bytes.NewReader(in.RunFile))
		if err != nil {
			return Archive{}, fmt.Errorf("%s: %w", where, err)
		}
		runs, err := Join(in.Pack, facts, rf)
		if err != nil {
			return Archive{}, fmt.Errorf("%s: %w", where, err)
		}
		sum, err := Summarize(rf.Subject, in.Pack, runs)
		if err != nil {
			return Archive{}, fmt.Errorf("%s: %w", where, err)
		}
		meta, err := ReadRunMeta(bytes.NewReader(in.Audit))
		if err != nil {
			return Archive{}, fmt.Errorf("%s: %w", where, err)
		}
		trails, err := readTrails(bytes.NewReader(in.Audit))
		if err != nil {
			return Archive{}, fmt.Errorf("%s: %w", where, err)
		}

		label := in.Label
		if label == "" {
			label = filepath.Base(where)
		}
		labels[label]++
		if n := labels[label]; n > 1 {
			label = fmt.Sprintf("%s #%d", label, n)
		}

		floor := isFloor(rf.Subject)
		if floor {
			doc.Floor.Runs = append(doc.Floor.Runs, id)
			floorSubjects[rf.Subject] = true
		}
		if !packSeen[in.Pack.Name] {
			packSeen[in.Pack.Name] = true
			doc.Packs = append(doc.Packs, in.Pack.Name)
		}

		doc.Runs = append(doc.Runs, ArchiveRun{
			ID:               id,
			Label:            label,
			Source:           in.Source,
			Subject:          rf.Subject,
			Pack:             in.Pack.Name,
			Cluster:          meta.Cluster,
			Started:          meta.Started,
			Ended:            meta.Ended,
			Floor:            floor,
			Null:             rf.Subject == NullSubject,
			Evidence:         in.Evidence,
			Scenarios:        sum.Scenarios,
			Manifested:       sum.Manifested,
			InjectFailures:   sum.InjectFailures,
			TeardownFailures: sum.TeardownFailures,
			EfficacyRate:     sum.EfficacyRate,
			BelowMinEfficacy: sum.EfficacyRate < opts.MinEfficacy,
			Means:            sum.Means,
		})

		byRun := make(map[string]Run, len(runs))
		for _, r := range runs {
			byRun[r.ScenarioID] = r
		}
		for _, res := range sum.Results {
			s, _ := in.Pack.ByID(res.ScenarioID)
			key := in.Pack.Name + "\x00" + s.ID
			row, ok := rows[key]
			if !ok {
				row = newArchiveScenario(in.Pack.Name, s)
				rows[key] = row
				order = append(order, key)
			}
			run := byRun[res.ScenarioID]
			cell := &ArchiveResult{
				Manifested:    res.Manifested,
				InjectError:   res.InjectError,
				SubjectError:  trimText(run.SubjectError, archiveMaxText),
				TeardownError: res.TeardownError,
				InjectedAt:    run.InjectedAt,
				DetectedAt:    run.DetectedAt,
				ClearedAt:     run.ClearedAt,
				Scores:        res.Scores,
				Faults:        trails[res.ScenarioID],
			}
			if cell.Manifested {
				if sc, ok := res.Score(row.Headline); ok && !sc.Skipped {
					v := sc.Value
					cell.Value = &v
				}
			}
			if run.Report != nil {
				rep := *run.Report
				cell.FindingsTotal = len(rep.Findings)
				if len(rep.Findings) > archiveMaxFindings {
					rep.Findings = rep.Findings[:archiveMaxFindings]
				}
				cell.Report = &rep
			}
			row.Results[id] = cell
		}
	}

	for s := range floorSubjects {
		doc.Floor.Subjects = append(doc.Floor.Subjects, s)
	}
	sort.Strings(doc.Floor.Subjects)
	doc.MixedPacks = len(doc.Packs) > 1

	floorRuns := map[string]bool{}
	for _, id := range doc.Floor.Runs {
		floorRuns[id] = true
	}
	nullRuns := map[string]bool{}
	for _, r := range doc.Runs {
		nullRuns[r.ID] = r.Null
	}
	for _, key := range order {
		row := rows[key]
		markFloor(row, floorRuns, nullRuns)
		doc.Scenarios = append(doc.Scenarios, *row)
	}
	return doc, nil
}

// markFloor sets each non-floor cell's comparison against the best floor
// value on the row. A row where no floor run measured anything has no floor,
// and a cell that was not measured is never below anything.
func markFloor(row *ArchiveScenario, floorRuns, nullRuns map[string]bool) {
	var best *float64
	for id, cell := range row.Results {
		if !floorRuns[id] || cell.Value == nil {
			continue
		}
		if best == nil || *cell.Value > *best {
			v := *cell.Value
			best = &v
		}
	}
	if best == nil {
		return
	}
	for id, cell := range row.Results {
		if floorRuns[id] || nullRuns[id] || cell.Value == nil {
			continue
		}
		v := *best
		cell.FloorValue = &v
		// A small tolerance so two equal fractions that took different
		// roads to the same float are not called different.
		cell.BelowFloor = *cell.Value < v-1e-9
	}
}

func newArchiveScenario(pack string, s scenario.Scenario) *ArchiveScenario {
	row := &ArchiveScenario{
		ID:       s.ID,
		Name:     s.Name,
		Pack:     pack,
		Prompt:   s.Prompt,
		Control:  s.IsControl(),
		Headline: MeasureRecall,
		GroundTruth: ArchiveGroundTruth{
			Expect:   s.Expect,
			AlsoTrue: s.AlsoTrue,
			Severity: s.Severity,
		},
		Results: map[string]*ArchiveResult{},
	}
	if row.GroundTruth.Expect == nil {
		row.GroundTruth.Expect = []scenario.ExpectedFinding{}
	}
	if row.Control {
		row.Headline = MeasureHallucination
	}
	for _, f := range s.Faults {
		af := ArchiveFault{Engine: f.Engine, Kind: f.ResourceKind, Targets: f.Targets, Spec: f.Spec}
		if f.Duration > 0 {
			af.Duration = f.Duration.String()
		}
		for _, p := range f.Probes {
			af.Probes = append(af.Probes, p.Name)
		}
		row.Faults = append(row.Faults, af)
	}
	return row
}

// readTrails collects, per scenario, what the audit log says happened to each
// fault: for the drill-down, not for scoring (ReadAudit decides that).
func readTrails(r io.Reader) (map[string][]ArchiveFaultTrail, error) {
	type key struct{ sid, uid string }
	byKey := map[key]*ArchiveFaultTrail{}
	order := map[string][]string{}
	err := eachAuditLine(r, func(l auditLine) {
		if l.ScenarioID == "" || l.FaultUID == "" {
			return
		}
		k := key{l.ScenarioID, l.FaultUID}
		t, ok := byKey[k]
		if !ok {
			t = &ArchiveFaultTrail{UID: l.FaultUID}
			byKey[k] = t
			order[l.ScenarioID] = append(order[l.ScenarioID], l.FaultUID)
		}
		switch l.Event {
		case audit.EventExecutorReceived:
			t.Engine = payloadString(l.Payload, "engine")
			t.Kind = payloadString(l.Payload, "kind")
		case audit.EventExecutorRejected:
			t.Problem = firstText(t.Problem, "rejected: "+orText(l.Reason, "by the executor"))
		case audit.EventDriverApplied:
			t.AppliedAt = l.at()
			t.EngineUID = payloadString(l.Payload, "engine_uid")
			t.Deadline = payloadString(l.Payload, "deadline")
		case audit.EventDriverFailed:
			t.Problem = firstText(t.Problem, "failed to apply: "+orText(payloadString(l.Payload, "error"), "driver apply failed"))
		case audit.EventFaultInjected:
			if passed, _ := l.Payload["passed"].(bool); !passed {
				t.Problem = firstText(t.Problem, "the engine did not inject it")
			}
		case audit.EventFaultEfficacy:
			passed, _ := l.Payload["passed"].(bool)
			t.Efficacy = append(t.Efficacy, ArchiveEfficacy{
				Probe:    payloadString(l.Payload, "probe"),
				Passed:   passed,
				Expected: trimText(payloadString(l.Payload, "expected"), archiveMaxObserved),
				Observed: trimText(payloadString(l.Payload, "observed"), archiveMaxObserved),
				At:       l.at(),
			})
		case audit.EventLeaseCleared:
			t.ClearedAt = l.at()
		}
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string][]ArchiveFaultTrail, len(order))
	for sid, uids := range order {
		for _, uid := range uids {
			out[sid] = append(out[sid], *byKey[key{sid, uid}])
		}
	}
	return out, nil
}

func firstText(have, next string) string {
	if have != "" {
		return have
	}
	return next
}

func orText(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// trimText keeps the head of s, marking the cut, so a subject that dumped a
// megabyte of stderr does not make the document unopenable.
func trimText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && cut < len(s) && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + fmt.Sprintf("… [%d more bytes]", len(s)-cut)
}

// WriteJSON writes the archive as indented JSON.
func (a Archive) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(a)
}
