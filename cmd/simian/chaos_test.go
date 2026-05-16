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

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSpecInlineJSON(t *testing.T) {
	m, err := loadSpec(`{"action":"pod-kill","mode":"one"}`, "", false)
	if err != nil {
		t.Fatalf("loadSpec: %v", err)
	}
	if got := m["action"]; got != "pod-kill" {
		t.Errorf("action=%v, want pod-kill", got)
	}
}

// TestLoadSpecFile is the regression test for the original CLI bug: both
// --spec and --spec-file bound to the same string variable, so passing a
// file path failed JSON-decode on the leading "/". After the fix,
// --spec-file must read the file contents.
func TestLoadSpecFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "spec.json")
	body := `{"labelSelectors":{"app":"frontend"},"directions":["ingress"]}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	m, err := loadSpec("", p, false)
	if err != nil {
		t.Fatalf("loadSpec --spec-file: %v", err)
	}
	dirs, ok := m["directions"].([]any)
	if !ok || len(dirs) != 1 || dirs[0] != "ingress" {
		t.Errorf("directions=%v, want [ingress]", m["directions"])
	}
}

func TestLoadSpecEmptyReturnsEmptyMap(t *testing.T) {
	m, err := loadSpec("", "", false)
	if err != nil {
		t.Fatalf("loadSpec empty: %v", err)
	}
	if len(m) != 0 {
		t.Errorf("empty spec should produce empty map; got %v", m)
	}
}

func TestLoadSpecRejectsOverlappingInputs(t *testing.T) {
	cases := []struct {
		name       string
		spec, file string
		stdin      bool
	}{
		{"spec+file", `{"x":1}`, "/tmp/whatever.json", false},
		{"spec+stdin", `{"x":1}`, "", true},
		{"file+stdin", "", "/tmp/whatever.json", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := loadSpec(tc.spec, tc.file, tc.stdin); err == nil {
				t.Errorf("expected error for overlapping inputs, got nil")
			}
		})
	}
}

func TestLoadSpecFileMissingReturnsError(t *testing.T) {
	if _, err := loadSpec("", "/no/such/path.json", false); err == nil {
		t.Error("expected error for missing spec file")
	}
}

func TestLoadSpecInvalidJSONReturnsError(t *testing.T) {
	if _, err := loadSpec(`{not valid json`, "", false); err == nil {
		t.Error("expected error for invalid JSON")
	}
}
