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
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/auth/credentials/idtoken"

	"github.com/go-steer/simian-agent/pkg/simian"
)

// fakeIAP signs assertions the way Identity-Aware Proxy does, with a key it
// serves in IAP's JWK format.
type fakeIAP struct {
	key  *ecdsa.PrivateKey
	keys *httptest.Server
}

func newFakeIAP(t *testing.T) *fakeIAP {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b64 := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	jwks, _ := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "EC", "crv": "P-256", "alg": "ES256", "use": "sig", "kid": "k1",
		"x": b64(key.X.FillBytes(make([]byte, 32))), "y": b64(key.Y.FillBytes(make([]byte, 32))),
	}}})
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(jwks) }))
	t.Cleanup(keys.Close)
	return &fakeIAP{key: key, keys: keys}
}

const testAudience = "/projects/123456/global/backendServices/987"

// token signs claims; edit adjusts the defaults first.
func (f *fakeIAP) token(t *testing.T, alg string, edit func(map[string]any)) string {
	t.Helper()
	claims := map[string]any{"iss": iapIssuer, "aud": testAudience, "email": "alice@example.com",
		"sub": "accounts.google.com:1", "iat": time.Now().Unix(), "exp": time.Now().Add(10 * time.Minute).Unix()}
	if edit != nil {
		edit(claims)
	}
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	signing := enc(map[string]string{"alg": alg, "kid": "k1", "typ": "JWT"}) + "." + enc(claims)
	h := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, f.key, h[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (f *fakeIAP) auth(t *testing.T, audience string, writers ...string) *IAP {
	t.Helper()
	v, err := idtoken.NewValidator(&idtoken.ValidatorOptions{ES256CertsURL: f.keys.URL, Client: f.keys.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return &IAP{Audience: audience, ProjectNumber: "123456", Writers: writers, Validator: v}
}

func TestOnlyAGenuineIAPAssertionGetsIn(t *testing.T) {
	f := newFakeIAP(t)
	a := f.auth(t, testAudience, "alice@example.com")
	identify := func(tok string) (Identity, error) {
		r := httptest.NewRequest("GET", "/api/me", nil)
		if tok != "" {
			r.Header.Set(iapHeader, tok)
		}
		return a.Identify(r)
	}

	id, err := identify(f.token(t, "ES256", nil))
	if err != nil || id.Email != "alice@example.com" || !id.CanWrite || id.Auth != "iap" {
		t.Fatalf("a genuine assertion: %+v, %v", id, err)
	}
	for name, tok := range map[string]string{
		"no assertion":                           "",
		"not a JWT":                              "abc",
		"RS256, as a service account's ID token": rs256Token(t),
		"another issuer":                         f.token(t, "ES256", func(c map[string]any) { c["iss"] = "https://accounts.google.com" }),
		"another audience":                       f.token(t, "ES256", func(c map[string]any) { c["aud"] = "/projects/123456/global/backendServices/1" }),
		"expired":                                f.token(t, "ES256", func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() }),
		"no email":                               f.token(t, "ES256", func(c map[string]any) { delete(c, "email") }),
		"short signature":                        f.token(t, "ES256", nil)[:strings.LastIndex(f.token(t, "ES256", nil), ".")] + ".AAAA",
		"tampered claims":                        tamper(f.token(t, "ES256", nil)),
	} {
		if id, err := identify(tok); err == nil {
			t.Errorf("%s: accepted as %+v", name, id)
		}
	}
	// These two must be stopped by the UI's own check, not left to the
	// validator: it would try RS256 against Google's OAuth keys, and slice
	// a short signature out of range.
	if _, err := identify(rs256Token(t)); err == nil || !strings.Contains(err.Error(), "want ES256") {
		t.Errorf("RS256: err = %v, want the ES256 check", err)
	}
	good := f.token(t, "ES256", nil)
	if _, err := identify(good[:strings.LastIndex(good, ".")] + ".AAAA"); err == nil || !strings.Contains(err.Error(), "bad signature") {
		t.Errorf("short signature: err = %v, want the signature-length check", err)
	}
}

// Without an exact audience, any backend service in the project is accepted,
// and none outside it.
func TestTheProjectNumberBoundsTheAudience(t *testing.T) {
	f := newFakeIAP(t)
	a := f.auth(t, "")
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set(iapHeader, f.token(t, "ES256", func(c map[string]any) { c["aud"] = "/projects/123456/global/backendServices/42" }))
	if _, err := a.Identify(r); err != nil {
		t.Errorf("a backend service in the project: %v", err)
	}
	r.Header.Set(iapHeader, f.token(t, "ES256", func(c map[string]any) { c["aud"] = "/projects/999/global/backendServices/42" }))
	if _, err := a.Identify(r); err == nil {
		t.Error("a backend service in another project was accepted")
	}
}

func TestWriters(t *testing.T) {
	w := Writers{"Bob@Example.com", "domain:sre.example.org"}
	for email, want := range map[string]bool{
		"bob@example.com": true, "carol@sre.example.org": true,
		"carol@example.com": false, "eve@evilsre.example.org": false, "": false,
	} {
		if got := w.Allow(email); got != want {
			t.Errorf("Allow(%q) = %v, want %v", email, got, want)
		}
	}
	if (Writers{}).Allow("bob@example.com") {
		t.Error("an empty list let someone write")
	}
}

// rs256Token is a well-formed token claiming RS256, as a Google ID token a
// service account minted for IAP's audience would.
func rs256Token(t *testing.T) string {
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	return enc(map[string]string{"alg": "RS256", "kid": "x"}) + "." +
		enc(map[string]any{"iss": iapIssuer, "aud": testAudience, "email": "mallory@example.com", "exp": time.Now().Add(time.Hour).Unix()}) +
		"." + base64.RawURLEncoding.EncodeToString(make([]byte, 256))
}

func tamper(tok string) string {
	parts := strings.Split(tok, ".")
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	b = []byte(strings.Replace(string(b), "alice@example.com", "mallory@example.com", 1))
	parts[1] = base64.RawURLEncoding.EncodeToString(b)
	return strings.Join(parts, ".")
}

// fakeExecutor records what the UI asked of it, and in whose name.
type fakeExecutor struct {
	applied []simian.FaultManifest
	actors  []string
	cleared []string
	refuse  error
}

func (e *fakeExecutor) Apply(ctx context.Context, m simian.FaultManifest) (string, error) {
	if e.refuse != nil {
		return "", e.refuse
	}
	e.applied = append(e.applied, m)
	e.actors = append(e.actors, simian.ActorFrom(ctx))
	return "f-1", nil
}

func (e *fakeExecutor) Clear(ctx context.Context, uid string) error {
	e.cleared = append(e.cleared, uid)
	e.actors = append(e.actors, simian.ActorFrom(ctx))
	return nil
}

func writeServer(t *testing.T, auth Authenticator, exec *fakeExecutor) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(Handler(Deps{
		Auth: auth, Executor: exec,
		Catalog: func(context.Context) ([]simian.CatalogEntry, error) {
			return []simian.CatalogEntry{{Engine: simian.EngineChaosMesh, APIVersion: "chaos-mesh.org/v1alpha1", ResourceKind: "PodChaos"}}, nil
		},
	}))
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, srv *httptest.Server, path, tok string, uiHeader bool, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if tok != "" {
		req.Header.Set(iapHeader, tok)
	}
	if uiHeader {
		req.Header.Set(writeHeader, "1")
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

const podKill = `{"mode":"manifest","namespace":"boutique","workload":"cartservice","kind":"PodChaos","spec":{"action":"pod-kill","mode":"one"},"duration":"90s"}`

func TestAWriterSubmitsAndClearsInTheirOwnName(t *testing.T) {
	f := newFakeIAP(t)
	exec := &fakeExecutor{}
	srv := writeServer(t, f.auth(t, testAudience, "alice@example.com"), exec)
	tok := f.token(t, "ES256", nil)

	code, body := post(t, srv, "/api/faults", tok, true, podKill)
	if code != 200 || !strings.Contains(body, `"fault_uid":"f-1"`) {
		t.Fatalf("submit: %d %s", code, body)
	}
	m := exec.applied[0]
	if m.Engine != simian.EngineChaosMesh || m.APIVersion != "chaos-mesh.org/v1alpha1" || m.Source != simian.SourceDirected ||
		m.Duration != 90*time.Second || m.Targets[0].Name != "cartservice" || m.Targets[0].Namespace != "boutique" {
		t.Errorf("manifest = %+v", m)
	}
	if code, body := post(t, srv, "/api/faults/f-1/clear", tok, true, ""); code != 200 {
		t.Fatalf("clear: %d %s", code, body)
	}
	if len(exec.cleared) != 1 || exec.actors[0] != "alice@example.com" || exec.actors[1] != "alice@example.com" {
		t.Errorf("cleared %v, actors %v: the executor must hear who asked", exec.cleared, exec.actors)
	}
}

func TestWritesAreRefusedUnlessEverythingHolds(t *testing.T) {
	f := newFakeIAP(t)
	exec := &fakeExecutor{}
	iap := writeServer(t, f.auth(t, testAudience, "alice@example.com"), exec)
	open := writeServer(t, NoAuth{}, exec)

	for name, c := range map[string]struct {
		srv      *httptest.Server
		tok      string
		uiHeader bool
		want     int
	}{
		"no IAP in front (port-forward)":     {open, "", true, http.StatusForbidden},
		"IAP user not in the writers list":   {iap, f.token(t, "ES256", func(c map[string]any) { c["email"] = "bob@example.com" }), true, http.StatusForbidden},
		"no X-Simian-UI header (cross-site)": {iap, f.token(t, "ES256", nil), false, http.StatusForbidden},
		"no assertion at all":                {iap, "", true, http.StatusUnauthorized},
	} {
		if code, body := post(t, c.srv, "/api/faults", c.tok, c.uiHeader, podKill); code != c.want {
			t.Errorf("%s: %d %s, want %d", name, code, body, c.want)
		}
	}
	if len(exec.applied) != 0 {
		t.Errorf("refused requests reached the executor: %+v", exec.applied)
	}
}

func TestTheExecutorsRefusalIsPassedOn(t *testing.T) {
	f := newFakeIAP(t)
	exec := &fakeExecutor{refuse: &simian.ExecutorError{Stage: simian.StageSafety, Reason: simian.ReasonWorkloadExcluded, Message: "workload loadgenerator is excluded"}}
	srv := writeServer(t, f.auth(t, testAudience, "alice@example.com"), exec)
	code, body := post(t, srv, "/api/faults", f.token(t, "ES256", nil), true, podKill)
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, "excluded") {
		t.Errorf("refusal: %d %s, want 422 with the reason", code, body)
	}
}

func TestTheHealthCheckNeedsNoAssertionAndTheRestDoes(t *testing.T) {
	f := newFakeIAP(t)
	srv := writeServer(t, f.auth(t, testAudience), &fakeExecutor{})
	for path, want := range map[string]int{"/healthz": 200, "/api/active": 401, "/ui/": 401, "/api/me": 401} {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s without an assertion: %d, want %d", path, resp.StatusCode, want)
		}
	}
}
