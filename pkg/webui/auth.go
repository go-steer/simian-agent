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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"cloud.google.com/go/auth/credentials/idtoken"
)

// Identity is who a request is from, as far as the UI can tell.
type Identity struct {
	// Email is the verified user, or "" when the UI runs without
	// authentication (a port-forward).
	Email string `json:"email,omitempty"`
	// CanWrite says whether this user may submit and clear faults.
	CanWrite bool `json:"can_write"`
	// CanAdmin says whether this user may configure, pause and resume
	// autonomous mode and clear every fault at once.
	CanAdmin bool `json:"can_admin"`
	// Auth is the mode the UI runs in: "none" or "iap".
	Auth string `json:"auth"`
}

// Authenticator decides who a request is from. Identify returns an error
// for a request that must be refused outright.
type Authenticator interface {
	Identify(r *http.Request) (Identity, error)
}

// NoAuth is a UI with no authentication in front of it — reached through a
// port-forward. It reads, and never writes: anyone who can reach the port
// would otherwise be able to inject faults.
type NoAuth struct{}

func (NoAuth) Identify(*http.Request) (Identity, error) { return Identity{Auth: "none"}, nil }

// iapHeader carries the JWT Identity-Aware Proxy signs for each request it
// lets through.
const iapHeader = "X-Goog-Iap-Jwt-Assertion"

// iapIssuer is the only issuer IAP's tokens carry.
const iapIssuer = "https://cloud.google.com/iap"

// IAP accepts only requests Identity-Aware Proxy let through, and names the
// user from IAP's signed assertion — not from the plain X-Goog-Authenticated
// headers, which anything that reaches the pod directly could set.
type IAP struct {
	// Audience is the backend service's audience,
	// /projects/NUMBER/global/backendServices/ID. When empty, any backend
	// service in ProjectNumber is accepted.
	Audience      string
	ProjectNumber string
	Writers       Writers
	// Admins may also configure autonomous mode and clear all faults.
	Admins Writers
	// Validator checks the signature against IAP's keys. Nil uses Google's.
	Validator *idtoken.Validator
}

func (a *IAP) Identify(r *http.Request) (Identity, error) {
	tok := r.Header.Get(iapHeader)
	if tok == "" {
		return Identity{}, errors.New("no IAP assertion: the UI is only served through Identity-Aware Proxy")
	}
	// Only IAP signs ES256 with IAP's keys. The validator also accepts any
	// RS256 Google ID token, and anyone with a service account can mint one
	// for any audience; so insist on ES256 and IAP's issuer here.
	if err := checkIAPShape(tok); err != nil {
		return Identity{}, err
	}
	v := a.Validator
	if v == nil {
		var err error
		if v, err = idtoken.NewValidator(nil); err != nil {
			return Identity{}, err
		}
		a.Validator = v
	}
	p, err := v.Validate(r.Context(), tok, a.Audience)
	if err != nil {
		return Identity{}, fmt.Errorf("IAP assertion: %w", err)
	}
	if p.Issuer != iapIssuer {
		return Identity{}, fmt.Errorf("IAP assertion: issuer %q, want %q", p.Issuer, iapIssuer)
	}
	if a.Audience == "" && !strings.HasPrefix(p.Audience, "/projects/"+a.ProjectNumber+"/global/backendServices/") {
		return Identity{}, fmt.Errorf("IAP assertion: audience %q is not a backend service of project %s", p.Audience, a.ProjectNumber)
	}
	email, _ := p.Claims["email"].(string)
	if email == "" {
		return Identity{}, errors.New("IAP assertion: no email claim")
	}
	return Identity{Email: email, CanWrite: a.Writers.Allow(email), CanAdmin: a.Admins.Allow(email), Auth: "iap"}, nil
}

// checkIAPShape rejects a token that is not ES256 with a 64-byte signature
// before the validator sees it: the validator slices the signature without
// checking its length.
func checkIAPShape(tok string) error {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return errors.New("IAP assertion: not a JWT")
	}
	h, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return errors.New("IAP assertion: bad header")
	}
	var header struct {
		Alg string `json:"alg"`
	}
	if json.Unmarshal(h, &header) != nil || header.Alg != "ES256" {
		return fmt.Errorf("IAP assertion: algorithm %q, want ES256", header.Alg)
	}
	if sig, err := base64.RawURLEncoding.DecodeString(parts[2]); err != nil || len(sig) != 64 {
		return errors.New("IAP assertion: bad signature")
	}
	return nil
}

// Writers is a list of people — who may submit and clear faults, or who may
// administer: exact emails, and "domain:example.com" for everyone in a
// domain. Empty allows nobody.
type Writers []string

func (w Writers) Allow(email string) bool {
	email = strings.ToLower(email)
	for _, entry := range w {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if d, ok := strings.CutPrefix(entry, "domain:"); ok {
			if strings.HasSuffix(email, "@"+d) {
				return true
			}
		} else if entry == email {
			return true
		}
	}
	return false
}

type identityKey struct{}

func identityFrom(ctx context.Context) Identity {
	id, _ := ctx.Value(identityKey{}).(Identity)
	return id
}

// authenticated refuses requests auth does not accept, and passes the
// identity of the rest to next.
func authenticated(auth Authenticator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := auth.Identify(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, id)))
	})
}
