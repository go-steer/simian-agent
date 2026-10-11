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

package webui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// simian web --scorecards: the *.json files in one directory, read-only, and
// nothing else in or outside it.
func TestScorecardsServesOnlyJSONInItsDirectory(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "scorecards")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "b.json"), `{"format":"simian.scorecard"}`)
	write(filepath.Join(dir, "a.json"), `{}`)
	write(filepath.Join(dir, ".hidden.json"), `{}`)
	write(filepath.Join(dir, "notes.txt"), "no")
	write(filepath.Join(dir, "sub", "c.json"), `{}`)
	write(filepath.Join(parent, "secret.json"), `{"secret":true}`)
	if err := os.Symlink(filepath.Join(parent, "secret.json"), filepath.Join(dir, "link.json")); err != nil {
		t.Fatal(err)
	}

	sc, err := OpenScorecards(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	srv := httptest.NewServer(StandaloneHandler(nil, sc))
	defer srv.Close()

	if body := get(t, srv, "/scorecards/"); strings.TrimSpace(body) != `["a.json","b.json"]` {
		t.Errorf("/scorecards/ = %s, want only the servable names (the symlink is not a regular file)", body)
	}
	if body := get(t, srv, "/scorecards/b.json"); !strings.Contains(body, "simian.scorecard") {
		t.Errorf("b.json = %q", body)
	}
	for _, p := range []string{
		"/scorecards/.hidden.json", "/scorecards/notes.txt", "/scorecards/sub/c.json",
		"/scorecards/link.json", "/scorecards/..%2fsecret.json", "/scorecards/missing.json",
	} {
		resp, err := srv.Client().Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s: served, want refused", p)
		}
	}
	resp, err := srv.Client().Post(srv.URL+"/scorecards/a.json", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Error("POST /scorecards/a.json accepted")
	}

	// Without the flag there is no /scorecards/ at all.
	plain := httptest.NewServer(StandaloneHandler(nil))
	defer plain.Close()
	resp, err = plain.Client().Get(plain.URL + "/scorecards/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/scorecards/ without a directory: %d, want 404", resp.StatusCode)
	}
}
