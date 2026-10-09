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
	"slices"
	"strings"
)

// withCORS lets pages on the listed origins — another Simian's UI, or the
// standalone UI — call this controller from the browser, carrying the
// user's IAP cookie, so one page can watch several Simians while each
// controller still checks who the user is.
//
// Requests from any other origin get no CORS headers and the browser keeps
// their answers from the page. A preflight is answered here, before
// authentication: browsers send it without credentials, and it carries no
// data. Writes still need X-Simian-UI, which only a listed origin's page
// may now send cross-origin.
func withCORS(origins []string, next http.Handler) http.Handler {
	if len(origins) == 0 {
		return next
	}
	allowed := make([]string, 0, len(origins))
	for _, o := range origins {
		allowed = append(allowed, strings.TrimRight(strings.TrimSpace(o), "/"))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		ok := origin != "" && slices.Contains(allowed, origin)
		if origin != "" {
			w.Header().Add("Vary", "Origin")
		}
		if ok {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			if !ok {
				http.Error(w, "origin not allowed", http.StatusForbidden)
				return
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, "+writeHeader)
			w.Header().Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
