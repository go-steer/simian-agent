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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func corsServer(t *testing.T) (*httptest.Server, *fakeIAP) {
	t.Helper()
	f := newFakeIAP(t)
	srv := httptest.NewServer(Handler(Deps{
		Version: "v-test", Name: "simian-iap",
		Auth:           f.auth(t, testAudience, "alice@example.com"),
		Executor:       &fakeExecutor{},
		AllowedOrigins: []string{"https://simian-2.demo.gke.ninja/", " https://ui.example.com"},
	}))
	t.Cleanup(srv.Close)
	return srv, f
}

func doReq(t *testing.T, srv *httptest.Server, method, path, origin, tok string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if tok != "" {
		req.Header.Set(iapHeader, tok)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp
}

// Another Simian's page may call this one with the user's credentials; any
// other page gets nothing it can read.
func TestOnlyListedOriginsMayCallFromTheBrowser(t *testing.T) {
	srv, f := corsServer(t)
	tok := f.token(t, "ES256", nil)

	resp := doReq(t, srv, "GET", "/api/info", "https://simian-2.demo.gke.ninja", tok, nil)
	if resp.StatusCode != 200 || resp.Header.Get("Access-Control-Allow-Origin") != "https://simian-2.demo.gke.ninja" ||
		resp.Header.Get("Access-Control-Allow-Credentials") != "true" {
		t.Errorf("listed origin: %d %v", resp.StatusCode, resp.Header)
	}
	resp = doReq(t, srv, "GET", "/api/info", "https://evil.example.com", tok, nil)
	if resp.Header.Get("Access-Control-Allow-Origin") != "" || resp.Header.Get("Access-Control-Allow-Credentials") != "" {
		t.Errorf("unlisted origin got CORS headers: %v", resp.Header)
	}
	resp = doReq(t, srv, "GET", "/api/info", "", tok, nil)
	if resp.StatusCode != 200 || resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("same-origin request: %d %v", resp.StatusCode, resp.Header)
	}
}

// A preflight carries no credentials, so it is answered before IAP is
// checked — for listed origins only — and it opens nothing by itself.
func TestPreflightIsAnsweredForListedOriginsOnly(t *testing.T) {
	srv, _ := corsServer(t)
	pre := map[string]string{"Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "content-type, x-simian-ui"}

	resp := doReq(t, srv, "OPTIONS", "/api/faults", "https://ui.example.com", "", pre)
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Origin") != "https://ui.example.com" ||
		resp.Header.Get("Access-Control-Allow-Headers") != "Content-Type, X-Simian-UI" {
		t.Errorf("listed preflight: %d %v", resp.StatusCode, resp.Header)
	}
	if resp := doReq(t, srv, "OPTIONS", "/api/faults", "https://evil.example.com", "", pre); resp.StatusCode != http.StatusForbidden {
		t.Errorf("unlisted preflight: %d", resp.StatusCode)
	}
	// The real request still needs IAP's assertion.
	if resp := doReq(t, srv, "POST", "/api/faults", "https://ui.example.com", "", map[string]string{writeHeader: "1"}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("cross-origin POST without an assertion: %d", resp.StatusCode)
	}
}

func TestInfoNamesTheController(t *testing.T) {
	srv, f := corsServer(t)
	req, _ := http.NewRequest("GET", srv.URL+"/api/info", nil)
	req.Header.Set(iapHeader, f.token(t, "ES256", nil))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var info struct{ Name, Version string }
	if err := decodeJSON(resp, &info); err != nil || info.Name != "simian-iap" || info.Version != "v-test" {
		t.Errorf("info = %+v, %v", info, err)
	}
}

func decodeJSON(resp *http.Response, v any) error { return json.NewDecoder(resp.Body).Decode(v) }
