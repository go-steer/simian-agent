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
	"slices"
	"testing"
)

func TestSplitArgsHonoursQuoting(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"plain", "agent --mode triage", []string{"agent", "--mode", "triage"}},
		{"runs of spaces collapse", "  agent   --x  ", []string{"agent", "--x"}},
		{"tabs and newlines separate", "agent\t--x\n--y", []string{"agent", "--x", "--y"}},
		{"double quotes hold a space", `agent --prompt "two words"`, []string{"agent", "--prompt", "two words"}},
		{"single quotes hold a space", `agent --prompt 'two words'`, []string{"agent", "--prompt", "two words"}},
		{"single quotes are literal", `agent 'a\b'`, []string{"agent", `a\b`}},
		{"double quotes take escapes", `agent "say \"hi\""`, []string{"agent", `say "hi"`}},
		{"escape outside quotes", `agent a\ b`, []string{"agent", "a b"}},
		{"quotes glue to a word", `agent --p="a b"`, []string{"agent", "--p=a b"}},
		{"empty argument survives", `agent "" x`, []string{"agent", "", "x"}},
		{"nested quote kinds", `sh -c 'echo "hi"'`, []string{"sh", "-c", `echo "hi"`}},
		{"nothing at all", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := splitArgs(tc.in)
			if err != nil {
				t.Fatalf("splitArgs(%q): %v", tc.in, err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("splitArgs(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// An empty pair of quotes has to survive as an argument. A subject invoked as
// `exec:agent --prompt ""` is passing an empty prompt on purpose, and dropping
// the argument silently shifts every flag after it by one position.
func TestAnEmptyQuotedArgumentIsStillAnArgument(t *testing.T) {
	got, err := splitArgs(`agent ''`)
	if err != nil {
		t.Fatalf("splitArgs: %v", err)
	}
	if len(got) != 2 || got[1] != "" {
		t.Fatalf("splitArgs = %q, want two args ending in an empty one", got)
	}
}

func TestSplitArgsRefusesWhatItCannotParse(t *testing.T) {
	for _, in := range []string{`agent "unbalanced`, `agent 'unbalanced`, `agent trailing\`} {
		if _, err := splitArgs(in); err == nil {
			t.Errorf("splitArgs(%q) = nil error, want a refusal", in)
		}
	}
}

// Nothing shell-ish is honoured. A spec that looks like a pipeline must arrive
// at the process as literal argv, because it is executed directly: quietly
// treating `>` as redirection would make a subject spec mean one thing in a
// shell and another here.
func TestSplitArgsDoesNoShellExpansion(t *testing.T) {
	got, err := splitArgs("agent *.json > out.txt | tee x $HOME")
	if err != nil {
		t.Fatalf("splitArgs: %v", err)
	}
	want := []string{"agent", "*.json", ">", "out.txt", "|", "tee", "x", "$HOME"}
	if !slices.Equal(got, want) {
		t.Fatalf("splitArgs = %q, want %q", got, want)
	}
}
