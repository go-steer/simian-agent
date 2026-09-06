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

package scenario

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// upstreamRecordEnv points the opt-in half of this file at a checkout of the
// agent repository's fixture file.
//
//	SIMIAN_UPSTREAM_FIXTURES=~/src/k8s-sre-agent/internal/faults/fixtures.go go test ./pkg/scenario
const upstreamRecordEnv = "SIMIAN_UPSTREAM_FIXTURES"

// upstreamRecord is testdata/upstream-fixtures.yaml: what the agent-side
// fixtures said when the parity pack was transcribed from them.
type upstreamRecord struct {
	Fixtures []upstreamFixture `json:"fixtures"`
}

type upstreamFixture struct {
	Fixture      string            `json:"fixture"`
	Scenario     string            `json:"scenario"`
	RenamedTo    string            `json:"renamed_to,omitempty"`
	Workloads    []string          `json:"workloads"`
	WantSeverity Severity          `json:"want_severity"`
	Wants        []ExpectedFinding `json:"wants,omitempty"`
}

// Namespace is the namespace the parity scenario runs in — the upstream one
// unless LintPrompt refused it.
func (f upstreamFixture) Namespace() string {
	if f.RenamedTo != "" {
		return f.RenamedTo
	}
	return f.Fixture
}

func loadUpstreamRecord(t *testing.T) upstreamRecord {
	t.Helper()
	data, err := os.ReadFile("testdata/upstream-fixtures.yaml")
	if err != nil {
		t.Fatalf("read upstream record: %v", err)
	}
	var rec upstreamRecord
	if err := yaml.UnmarshalStrict(data, &rec); err != nil {
		t.Fatalf("parse upstream record: %v", err)
	}
	if len(rec.Fixtures) == 0 {
		t.Fatal("upstream record is empty")
	}
	return rec
}

// The always-run half. The parity pack exists to be comparable with a rig in
// another repository, and nothing in the build can see that repository — so
// what is checked continuously is the pack against the written-down record of
// what it mirrors. An edit to a reason list, a min severity or a root marker
// that does not also edit the record is caught here, which is where a
// comparability claim quietly stops being true.
func TestParityPackMatchesTheUpstreamRecord(t *testing.T) {
	rec := loadUpstreamRecord(t)
	pack := MustBuiltin(PackParity)

	if pack.Len() != len(rec.Fixtures) {
		t.Fatalf("parity pack has %d scenarios, the record names %d upstream fixtures",
			pack.Len(), len(rec.Fixtures))
	}

	for _, f := range rec.Fixtures {
		s, ok := pack.ByID(f.Scenario)
		if !ok {
			t.Errorf("fixture %q maps to scenario %q, which the pack does not contain", f.Fixture, f.Scenario)
			continue
		}
		if s.Severity != f.WantSeverity {
			t.Errorf("%s: Severity = %q, upstream WantSeverity is %q", f.Scenario, s.Severity, f.WantSeverity)
		}
		// reflect.DeepEqual on the whole slice rather than field by field, and
		// in order: the tolerances are the measurement. An extra accepted
		// reason or a dropped AlsoAcceptKind changes what the score means
		// without changing anything a looser comparison would notice.
		if !reflect.DeepEqual(s.Expect, f.Wants) {
			t.Errorf("%s: expectations differ from the upstream record\n got: %+v\nwant: %+v",
				f.Scenario, s.Expect, f.Wants)
		}
		if got := namespacesOf(s); len(got) != 1 || got[0] != f.Namespace() {
			t.Errorf("%s: namespaces = %v, want [%s]", f.Scenario, got, f.Namespace())
		}
	}
}

// A rename is a divergence from the thing being mirrored, so it is recorded
// per fixture rather than inferred — and the reason has to hold: the upstream
// name must be one LintPrompt would actually have refused. A rename recorded
// for a namespace that was fine is a difference nobody had to make, and it
// would go unnoticed as a comment.
func TestEveryRenamedNamespaceWasOneLintWouldRefuse(t *testing.T) {
	for _, f := range loadUpstreamRecord(t).Fixtures {
		probe := Scenario{
			Name:   f.Fixture,
			Prompt: fmt.Sprintf("Assess the health of the %q namespace and report what you find.", f.Fixture),
		}
		refused := LintPrompt(probe) != nil
		switch {
		case f.RenamedTo != "" && !refused:
			t.Errorf("fixture %q is renamed to %q, but its own name does not leak — drop the rename",
				f.Fixture, f.RenamedTo)
		case f.RenamedTo == "" && refused:
			t.Errorf("fixture %q leaks its diagnosis and is not renamed: %v", f.Fixture, LintPrompt(probe))
		}
	}
}

// The opt-in half. It answers the other question — has upstream moved since
// the record was written — and it can only be asked by someone with both
// repositories checked out.
//
// Skipped rather than failed when the environment variable is unset, because
// CI has no access to that repository and never will: the two rigs are
// deliberately independent, and a build dependency would be the very coupling
// the parity measurement exists to avoid.
//
// It reads the source as text. That is a weaker check than compiling against
// the fixtures and dumping them, and it is the strongest one available without
// a Go dependency in either direction. What it catches is what actually drifts:
// a fixture renamed or removed, a workload renamed, a reason token dropped, or
// a twelfth fixture added.
func TestUpstreamRecordStillMatchesTheSource(t *testing.T) {
	path := os.Getenv(upstreamRecordEnv)
	if path == "" {
		t.Skipf("set %s to a checkout of internal/faults/fixtures.go to check the record against upstream", upstreamRecordEnv)
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s=%q: %v", upstreamRecordEnv, path, err)
	}
	src := string(blob)

	rec := loadUpstreamRecord(t)

	// Every fixture declares its namespace as `const ns = "..."`, one per
	// fixture function. Counting them is what catches an addition — a twelfth
	// fixture is a hole in the parity claim, and nothing else here would see
	// it.
	if got := strings.Count(src, `const ns = "`); got != len(rec.Fixtures) {
		t.Errorf("upstream declares %d fixture namespaces, the record has %d entries",
			got, len(rec.Fixtures))
	}

	for _, f := range rec.Fixtures {
		if !strings.Contains(src, fmt.Sprintf(`const ns = %q`, f.Fixture)) {
			t.Errorf("fixture %q is in the record and not in the source", f.Fixture)
			continue
		}
		// Workloads are matched bare, not as Go string literals. Some fixtures
		// name theirs in an inline YAML manifest, where `name: ledger-writer`
		// carries no quotes. Bare matching is looser — "frontend" is inside
		// "frontend-v2" — and still catches the drift that matters, which is a
		// workload renamed or dropped.
		for _, w := range f.Workloads {
			if !strings.Contains(src, w) {
				t.Errorf("fixture %q: workload %q is in the record and not in the source", f.Fixture, w)
			}
		}
		for _, want := range f.Wants {
			for _, r := range want.Reasons {
				if !strings.Contains(src, fmt.Sprintf("%q", r)) {
					t.Errorf("fixture %q: reason %q is in the record and not in the source", f.Fixture, r)
				}
			}
			for _, k := range want.AlsoAcceptKinds {
				if !strings.Contains(src, fmt.Sprintf("%q", k)) {
					t.Errorf("fixture %q: also-accept kind %q is in the record and not in the source", f.Fixture, k)
				}
			}
		}
	}
}
