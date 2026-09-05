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

package subject

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseBuildsAnExecSubject(t *testing.T) {
	s, err := Parse(`exec:./bin/agent --mode "deep triage"`, Options{
		Timeout: 90 * time.Second,
		Dir:     "/tmp",
		Env:     []string{"K=V"},
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	e, ok := s.(*Exec)
	if !ok {
		t.Fatalf("Parse returned %T, want *Exec", s)
	}
	if want := []string{"./bin/agent", "--mode", "deep triage"}; !slices.Equal(e.Argv, want) {
		t.Errorf("Argv = %q, want %q", e.Argv, want)
	}
	if e.Timeout != 90*time.Second || e.Dir != "/tmp" || !slices.Equal(e.Env, []string{"K=V"}) {
		t.Errorf("options were not carried through: %+v", e)
	}
	if s.Name() != "agent" {
		t.Errorf("Name = %q, want the base name of the binary", s.Name())
	}
}

func TestParseBuildsTheNoopSubject(t *testing.T) {
	s, err := Parse("noop:", Options{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s.Name() != "noop" {
		t.Errorf("Name = %q, want noop", s.Name())
	}
	r, err := s.Investigate(context.Background(), "anything")
	if err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	// Non-nil and empty. "The subject answered, and said nothing is wrong" is
	// a real answer — right on a control, wrong everywhere else — and the
	// scorer has to be able to tell it from a subject that never answered.
	if r.Findings == nil {
		t.Fatal("Findings is nil; the noop subject answered, it did not fail to answer")
	}
	if len(r.Findings) != 0 {
		t.Errorf("Findings = %v, want none", r.Findings)
	}
}

func TestParseRefusesSpecsItCannotHonour(t *testing.T) {
	cases := []struct {
		name string
		spec string
		want string
	}{
		{"no scheme", "./bin/agent", "no scheme"},
		{"unknown scheme", "grpc:localhost:9000", "unknown scheme"},
		{"http is not built yet", "http://localhost:8080", "not implemented yet"},
		{"mcp is not built yet", "mcp:stdio", "not implemented yet"},
		{"exec with no command", "exec:   ", "names no command"},
		{"exec that will not tokenise", `exec:agent "oops`, "unbalanced"},
		{"noop takes no argument", "noop:something", "takes no argument"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.spec, Options{})
			if err == nil {
				t.Fatalf("Parse(%q) = nil error, want a refusal", tc.spec)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Parse(%q) = %v, want it to mention %q", tc.spec, err, tc.want)
			}
		})
	}
}

// The scheme is required rather than guessed. A bare path could reasonably
// mean exec: today and something else once http: exists, and a spec whose
// meaning moves with the version is a spec nobody can put in a CI file.
func TestParseDoesNotGuessAScheme(t *testing.T) {
	if _, err := Parse("/usr/local/bin/agent", Options{}); err == nil {
		t.Fatal("a bare path was accepted; it must be spelled exec:")
	}
}
