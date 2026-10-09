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
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
)

// Controller is another Simian the page offers to watch: how it is
// labelled, and its UI's origin. The browser calls it directly, with the
// user's own credentials for it; nothing is proxied.
type Controller struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// ParseControllers reads --ui-controllers / --controllers values:
// name=https://simian-2.example.com, or just the URL (the name is then its
// host). The URL is the controller UI's origin; a trailing / or /ui/ is
// dropped, and any other path is refused, since the page calls /api/ at
// the origin.
func ParseControllers(specs []string) ([]Controller, error) {
	out := make([]Controller, 0, len(specs))
	seen := map[string]bool{}
	for _, spec := range specs {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			continue
		}
		name, raw, ok := strings.Cut(spec, "=")
		if !ok || strings.Contains(name, "://") {
			name, raw = "", spec
		}
		origin, err := controllerOrigin(raw)
		if err != nil {
			return nil, fmt.Errorf("controller %q: %w", spec, err)
		}
		name = strings.TrimSpace(name)
		if name == "" {
			name = strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
		}
		if seen[origin] {
			return nil, fmt.Errorf("controller %q: %s is listed twice", spec, origin)
		}
		seen[origin] = true
		out = append(out, Controller{Name: name, URL: origin})
	}
	return out, nil
}

func controllerOrigin(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("want an http(s) URL such as https://simian-2.example.com")
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("want the controller UI's origin, such as https://simian-2.example.com")
	}
	switch strings.TrimRight(u.Path, "/") {
	case "", "/ui":
	default:
		return "", fmt.Errorf("path %q: want just the origin (the page calls /api/ there)", u.Path)
	}
	return u.Scheme + "://" + strings.ToLower(u.Host), nil
}

// controllersRoute serves the deployment's list of other Simians.
func controllersRoute(mux *http.ServeMux, list []Controller) {
	mux.HandleFunc("GET /api/controllers", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, nonNil(list))
	})
}

// StandaloneHandler is simian web: the same page with no controller behind
// it. It serves the UI at /ui/, the list of Simians it was started with at
// /api/controllers, and /healthz; every other /api/ path is 404, which is
// how the page knows no Simian serves it and asks which to watch.
func StandaloneHandler(list []Controller) http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web")
	mux.Handle("/ui/", http.StripPrefix("/ui/", http.FileServerFS(static)))
	mux.HandleFunc("/ui", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ui/", http.StatusFound) })
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ui/", http.StatusFound) })
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	controllersRoute(mux, list)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no Simian controller here: this is the standalone UI (simian web)", http.StatusNotFound)
	})
	return mux
}
