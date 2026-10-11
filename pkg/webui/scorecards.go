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
	"io/fs"
	"net/http"
	"os"
	"sort"
	"strings"
)

// Scorecards serves the scorecard documents (`simian-eval report` output)
// in one directory, read-only, for the page's Scorecard mode:
//
//	GET /scorecards/        the names of the *.json files, as a JSON array
//	GET /scorecards/{name}  one of them
//
// Only regular *.json files directly in the directory are served: no
// subdirectories, no dotfiles, no listing of anything else. The directory is
// opened as an os.Root, so a name cannot reach outside it, by .. or by a
// symlink.
type Scorecards struct {
	root *os.Root
}

// OpenScorecards opens dir for serving.
func OpenScorecards(dir string) (*Scorecards, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Scorecards{root: root}, nil
}

// Close releases the directory.
func (s *Scorecards) Close() error { return s.root.Close() }

func servableName(name string) bool {
	return strings.HasSuffix(name, ".json") && !strings.HasPrefix(name, ".") &&
		!strings.ContainsAny(name, `/\`) && len(name) <= 255
}

func (s *Scorecards) route(mux *http.ServeMux) {
	mux.HandleFunc("GET /scorecards/{$}", func(w http.ResponseWriter, r *http.Request) {
		entries, err := fs.ReadDir(s.root.FS(), ".")
		if err != nil {
			http.Error(w, "cannot list scorecards", http.StatusInternalServerError)
			return
		}
		names := []string{}
		for _, e := range entries {
			if e.Type().IsRegular() && servableName(e.Name()) {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		writeJSON(w, names)
	})
	mux.HandleFunc("GET /scorecards/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !servableName(name) {
			http.NotFound(w, r)
			return
		}
		f, err := s.root.Open(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer func() { _ = f.Close() }()
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		http.ServeContent(w, r, "", info.ModTime(), f)
	})
}
