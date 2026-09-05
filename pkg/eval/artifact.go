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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/scenario"
)

// The offline scorer reconstructs a Run from two artifacts, split along the
// line of who observed what.
//
// The audit log is Simian's record of breaking things: which faults were
// applied, whether their efficacy gates passed, and when. None of it can be
// taken from the subject, because the whole point of the efficacy gate is that
// the harness does not get to take the subject's word for the cluster's state.
//
// The run file is the record of the subject's side: what it reported, when the
// report came back, and — on a run where the subject may write — when the
// fault was observed gone.
//
// They join on ScenarioID, which pkg/audit stamps onto every event from the
// context. That key is the reason this reconstruction is possible at all.

// auditLine is one line of the JSON audit log as SLogAuditor writes it.
type auditLine struct {
	// Time is slog's own record time; TS is the audit event's UTC stamp.
	// Both are written, and TS is the one the log means.
	Time time.Time `json:"time"`
	TS   time.Time `json:"ts"`

	Event      string         `json:"event"`
	FaultUID   string         `json:"fault_uid"`
	ScenarioID string         `json:"scenario_id"`
	Reason     string         `json:"reason"`
	Payload    map[string]any `json:"payload"`
}

func (l auditLine) at() time.Time {
	if !l.TS.IsZero() {
		return l.TS
	}
	return l.Time
}

// ScenarioFacts is what the audit log knows about one scenario: whether Simian
// actually broke the cluster, and when.
type ScenarioFacts struct {
	ScenarioID string `json:"scenario_id"`

	// Manifested is true only when every fault in the scenario has at least
	// one passing efficacy record and no failing one. Absence of evidence is
	// not manifestation — see ReadAudit.
	Manifested bool `json:"manifested"`

	// InjectedAt is the last moment a fault's efficacy gate passed, which is
	// the first moment the whole scenario was observably live.
	InjectedAt time.Time `json:"injected_at,omitempty"`

	// InjectError is why the scenario did not manifest, in the harness's own
	// words. Never scored as a subject miss.
	InjectError string `json:"inject_error,omitempty"`

	// Faults are the fault UIDs seen for this scenario, sorted.
	Faults []string `json:"faults,omitempty"`
}

// faultFacts accumulates one fault's audit trail while the log is read.
type faultFacts struct {
	uid        string
	applied    bool
	passedAt   time.Time
	passes     int
	failure    string
	driverFail string
	rejected   string
}

// ReadAudit reconstructs per-scenario facts from a JSON audit log.
//
// Lines that are not JSON objects are skipped rather than fatal: an audit log
// is routinely a controller's whole stdout, and a startup banner or a stray
// warning from a library is not a reason to refuse to score a run. A line that
// parses but carries no event is skipped for the same reason, and so is one
// carrying no scenario — a manual chaos apply, the controller's own lifecycle,
// or the reaper collecting a lease long after the applying context is gone.
// None of those say anything about whether a scenario's fault landed.
func ReadAudit(r io.Reader) ([]ScenarioFacts, error) {
	scanner := bufio.NewScanner(r)
	// Audit lines carry probe payloads and can be long; the 64KiB default is
	// not enough for a log worth scoring.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	// Ordered so the scorecard is stable, and so a caller diffing two runs
	// sees the difference rather than the map iteration.
	order := []string{}
	byScenario := map[string]map[string]*faultFacts{}

	for scanner.Scan() {
		var l auditLine
		if err := json.Unmarshal(scanner.Bytes(), &l); err != nil {
			continue
		}
		if l.Event == "" || l.ScenarioID == "" {
			continue
		}
		if _, ok := byScenario[l.ScenarioID]; !ok {
			byScenario[l.ScenarioID] = map[string]*faultFacts{}
			order = append(order, l.ScenarioID)
		}
		applyLine(byScenario[l.ScenarioID], l)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("eval: reading audit log: %w", err)
	}

	out := make([]ScenarioFacts, 0, len(order))
	for _, sid := range order {
		out = append(out, summarizeFacts(sid, byScenario[sid]))
	}
	return out, nil
}

// applyLine folds one audit event into the fault it is about.
func applyLine(faults map[string]*faultFacts, l auditLine) {
	if l.FaultUID == "" {
		return
	}
	f, ok := faults[l.FaultUID]
	if !ok {
		f = &faultFacts{uid: l.FaultUID}
		faults[l.FaultUID] = f
	}

	switch l.Event {
	case audit.EventDriverApplied:
		f.applied = true
	case audit.EventDriverFailed:
		f.driverFail = payloadString(l.Payload, "error")
		if f.driverFail == "" {
			f.driverFail = "driver apply failed"
		}
	case audit.EventExecutorRejected:
		f.rejected = l.Reason
		if f.rejected == "" {
			f.rejected = "rejected by the executor"
		}
	case audit.EventFaultEfficacy:
		if passed, _ := l.Payload["passed"].(bool); passed {
			f.passes++
			if at := l.at(); at.After(f.passedAt) {
				f.passedAt = at
			}
			return
		}
		if f.failure == "" {
			f.failure = describeProbeFailure(l)
		}
	}
}

// describeProbeFailure turns a failed efficacy record into one line an
// operator can act on: which gate, what it wanted, what it saw.
func describeProbeFailure(l auditLine) string {
	name := payloadString(l.Payload, "probe")
	if name == "" {
		name = "efficacy gate"
	}
	msg := fmt.Sprintf("%s did not pass", name)
	if want := payloadString(l.Payload, "expected"); want != "" {
		msg += fmt.Sprintf(": expected %q, observed %q", want, payloadString(l.Payload, "observed"))
	}
	if errText := payloadString(l.Payload, "error"); errText != "" {
		msg += ": " + errText
	}
	return msg
}

func payloadString(p map[string]any, key string) string {
	if p == nil {
		return ""
	}
	switch v := p[key].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

// summarizeFacts decides whether a scenario manifested.
//
// The rule is deliberately the strict one: every fault needs a passing
// efficacy record of its own. A scenario is one incident, and half a cascade
// is not the incident the expectations describe — scoring a subject on the
// half that landed would grade it against ground truth that was never true.
func summarizeFacts(sid string, faults map[string]*faultFacts) ScenarioFacts {
	out := ScenarioFacts{ScenarioID: sid}
	for uid := range faults {
		out.Faults = append(out.Faults, uid)
	}
	sort.Strings(out.Faults)

	if len(faults) == 0 {
		// Events stamped with the scenario but naming no fault. Read on its
		// own that is a scenario nothing was injected for. A healthy control
		// looks exactly the same and is the opposite of a problem, but only
		// the pack knows which this is, so Join makes that call.
		out.InjectError = "the audit log records no fault for this scenario"
		return out
	}

	var problems []string
	manifested := true
	for _, uid := range out.Faults {
		f := faults[uid]
		switch {
		case f.rejected != "":
			problems = append(problems, fmt.Sprintf("fault %s was rejected: %s", uid, f.rejected))
		case f.driverFail != "":
			problems = append(problems, fmt.Sprintf("fault %s failed to apply: %s", uid, f.driverFail))
		case f.failure != "":
			problems = append(problems, fmt.Sprintf("fault %s did not manifest: %s", uid, f.failure))
		case f.passes == 0:
			// The loud case. An applied fault with no efficacy record at all
			// is not a fault that landed — it is a fault nobody checked, and
			// every score built on it would be a confident number about a
			// cluster whose state is unknown.
			problems = append(problems, fmt.Sprintf("fault %s has no passing efficacy record", uid))
		default:
			if f.passedAt.After(out.InjectedAt) {
				out.InjectedAt = f.passedAt
			}
			continue
		}
		manifested = false
	}

	out.Manifested = manifested
	if !manifested {
		out.InjectError = joinProblems(problems)
		// A partial injection has no meaningful "the cluster became broken"
		// moment, and a timestamp from the half that landed would make the
		// timing measures look valid.
		out.InjectedAt = time.Time{}
	}
	return out
}

func joinProblems(problems []string) string {
	switch len(problems) {
	case 0:
		return ""
	case 1:
		return problems[0]
	default:
		out := problems[0]
		for _, p := range problems[1:] {
			out += "; " + p
		}
		return out
	}
}

// RunRecord is one scenario's worth of the subject's side of the run.
//
// Everything here is something the audit log cannot know: what the subject
// said, when it said it, and — on a run where the subject is allowed to write
// — when the fault was observed gone.
type RunRecord struct {
	ScenarioID string `json:"scenario_id"`

	// Report is what the subject produced, or absent if it produced nothing
	// structured. Absent is a zero, not a skip.
	Report *Report `json:"report,omitempty"`

	// SubjectError is the subject's own failure: crashed, timed out, returned
	// something that would not parse. Distinct from an inject failure, and
	// scored rather than skipped.
	SubjectError string `json:"subject_error,omitempty"`

	DetectedAt time.Time `json:"detected_at,omitempty"`
	ClearedAt  time.Time `json:"cleared_at,omitempty"`
}

// RunFile is the report artifact: one subject, one run per scenario.
type RunFile struct {
	Subject string      `json:"subject"`
	Runs    []RunRecord `json:"runs"`
}

// ReadRunFile decodes a report artifact.
func ReadRunFile(r io.Reader) (RunFile, error) {
	var rf RunFile
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rf); err != nil {
		return RunFile{}, fmt.Errorf("eval: reading run file: %w", err)
	}
	if rf.Subject == "" {
		return RunFile{}, errors.New("eval: run file names no subject; a scorecard has to say what it graded")
	}
	seen := map[string]bool{}
	for i, rec := range rf.Runs {
		if rec.ScenarioID == "" {
			return RunFile{}, fmt.Errorf("eval: run %d has no scenario_id, so nothing can be joined to it", i)
		}
		if seen[rec.ScenarioID] {
			return RunFile{}, fmt.Errorf("eval: run file has two runs for scenario %q", rec.ScenarioID)
		}
		seen[rec.ScenarioID] = true
	}
	return rf, nil
}

// Join pairs the audit log's facts with the subject's runs, on ScenarioID.
//
// Both halves are required for every scenario, and a missing one is an error
// rather than a default. A scenario in the report with no audit trail has no
// evidence the cluster was ever broken; a scenario in the audit with no run
// has no evidence the subject was ever asked. Scoring either as a zero is the
// confident wrong number this package exists to avoid.
//
// The pack is here for one reason: a healthy control and a scenario nobody
// injected leave the same trace, and only the pack can tell them apart.
func Join(pack scenario.Pack, facts []ScenarioFacts, rf RunFile) ([]Run, error) {
	byID := make(map[string]ScenarioFacts, len(facts))
	for _, f := range facts {
		byID[f.ScenarioID] = f
	}

	runs := make([]Run, 0, len(rf.Runs))
	for _, rec := range rf.Runs {
		f, ok := byID[rec.ScenarioID]
		if !ok {
			return nil, fmt.Errorf("eval: the run file reports scenario %q, but the audit log has no record of it being injected", rec.ScenarioID)
		}
		delete(byID, rec.ScenarioID)

		s, inPack := pack.ByID(rec.ScenarioID)
		if !inPack {
			return nil, fmt.Errorf("eval: run file and audit log both name scenario %q, which is not in pack %q", rec.ScenarioID, pack.Name)
		}
		if s.IsControl() {
			// Nothing was injected because nothing was meant to be. A control
			// that reached the subject at all has done its job, and its
			// measures must be scored — a control skipped for "injection
			// failure" measures nothing, and measuring invention is the only
			// reason controls are in the pack.
			f.Manifested = true
			f.InjectError = ""
		}

		runs = append(runs, Run{
			ScenarioID:   rec.ScenarioID,
			Subject:      rf.Subject,
			Report:       rec.Report,
			InjectError:  f.InjectError,
			SubjectError: rec.SubjectError,
			Manifested:   f.Manifested,
			InjectedAt:   f.InjectedAt,
			DetectedAt:   rec.DetectedAt,
			ClearedAt:    rec.ClearedAt,
		})
	}

	if len(byID) > 0 {
		missing := make([]string, 0, len(byID))
		for id := range byID {
			missing = append(missing, id)
		}
		sort.Strings(missing)
		return nil, fmt.Errorf("eval: the audit log records scenario(s) %v, which the run file does not report on", missing)
	}
	return runs, nil
}
