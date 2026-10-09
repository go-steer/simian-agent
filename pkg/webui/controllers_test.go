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
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestParseControllers(t *testing.T) {
	got, err := ParseControllers([]string{
		"simian-2=https://Simian-2.demo.gke.ninja/",
		" https://simian-3.example.com/ui/ ",
		"local=http://localhost:18111",
		"",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []Controller{
		{Name: "simian-2", URL: "https://simian-2.demo.gke.ninja"},
		{Name: "simian-3.example.com", URL: "https://simian-3.example.com"},
		{Name: "local", URL: "http://localhost:18111"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	for _, bad := range []string{
		"x=simian-2.example.com",              // no scheme
		"x=ftp://simian-2.example.com",        // not http(s)
		"x=https://simian-2.example.com/api/", // a path the page cannot use
		"x=https://simian-2.example.com/?a=b", // a query
		"x=https://user@simian-2.example.com", // credentials in the URL
		"x=https://",                          // no host
	} {
		if _, err := ParseControllers([]string{bad}); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
	if _, err := ParseControllers([]string{"a=https://s.example.com", "b=https://s.example.com/"}); err == nil {
		t.Error("the same origin twice: want an error")
	}
	if got, err := ParseControllers(nil); err != nil || len(got) != 0 {
		t.Errorf("none: %v %v", got, err)
	}
}

// The controller serves its deployment's list behind the same
// authentication as the rest of /api/.
func TestControllersAreServedBehindAuth(t *testing.T) {
	f := newFakeIAP(t)
	srv := httptest.NewServer(Handler(Deps{
		Version: "v-test", Auth: f.auth(t, testAudience),
		Controllers: []Controller{{Name: "simian-2", URL: "https://simian-2.example.com"}},
	}))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/api/controllers")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("without IAP's assertion: %d, want 401", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/api/controllers", nil)
	req.Header.Set(iapHeader, f.token(t, "ES256", nil))
	resp, err = srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list []Controller
	if err := decodeJSON(resp, &list); err != nil || len(list) != 1 || list[0].Name != "simian-2" || list[0].URL != "https://simian-2.example.com" {
		t.Errorf("list = %+v, %v", list, err)
	}

	// None listed: an empty array, not null.
	none := httptest.NewServer(Handler(Deps{}))
	defer none.Close()
	if body := get(t, none, "/api/controllers"); strings.TrimSpace(body) != "[]" {
		t.Errorf("none listed: %q", body)
	}
}

// simian web: the page and its list, and nothing that pretends to be a
// controller.
func TestStandaloneServesThePageAndTheListOnly(t *testing.T) {
	srv := httptest.NewServer(StandaloneHandler([]Controller{
		{Name: "a", URL: "http://localhost:18110"}, {Name: "b", URL: "http://127.0.0.1:18111"},
	}))
	defer srv.Close()

	if body := get(t, srv, "/ui/"); !strings.Contains(body, "<title>Simian</title>") {
		t.Errorf("/ui/ is not the page: %.80q", body)
	}
	if body := get(t, srv, "/ui/app.js"); !strings.Contains(body, "use strict") {
		t.Error("/ui/app.js missing")
	}
	if body := get(t, srv, "/healthz"); body != "ok\n" {
		t.Errorf("/healthz = %q", body)
	}
	if body := get(t, srv, "/api/controllers"); !strings.Contains(body, `{"name":"a","url":"http://localhost:18110"}`) ||
		!strings.Contains(body, `{"name":"b","url":"http://127.0.0.1:18111"}`) {
		t.Errorf("/api/controllers = %q", body)
	}
	for _, p := range []string{"/api/info", "/api/active", "/api/events", "/api/me", "/api/config"} {
		resp, err := srv.Client().Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, resp.StatusCode)
		}
	}
	resp, err := srv.Client().Post(srv.URL+"/api/faults", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST /api/faults: %d, want 404", resp.StatusCode)
	}
}

func get(t *testing.T, srv *httptest.Server, path string) string {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, resp.StatusCode, b)
	}
	return string(b)
}
