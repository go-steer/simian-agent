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

package scenario

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// Pack is a named, ordered set of scenarios loaded from one source.
//
// Packs are the unit of comparison. A score is only meaningful against the
// pack it was produced on, so the pack name travels with every report.
type Pack struct {
	Name      string     `json:"name"`
	Scenarios []Scenario `json:"scenarios"`
}

// Len returns the number of scenarios in the pack.
func (p Pack) Len() int { return len(p.Scenarios) }

// ByID returns the scenario with the given ID.
func (p Pack) ByID(id string) (Scenario, bool) {
	for _, s := range p.Scenarios {
		if s.ID == id {
			return s, true
		}
	}
	return Scenario{}, false
}

// Controls returns the scenarios that expect no findings.
//
// A pack with no controls cannot detect a subject that reports every failure
// mode everywhere: recall would be perfect and precision unmeasured.
func (p Pack) Controls() []Scenario {
	var out []Scenario
	for _, s := range p.Scenarios {
		if s.IsControl() {
			out = append(out, s)
		}
	}
	return out
}

// Validate checks every scenario in the pack, and the pack's own invariants.
func (p Pack) Validate() error {
	var errs []error
	if strings.TrimSpace(p.Name) == "" {
		errs = append(errs, errors.New("pack: Name is required"))
	}
	if len(p.Scenarios) == 0 {
		errs = append(errs, fmt.Errorf("pack %q: contains no scenarios", p.Name))
	}

	seenID := map[string]string{}
	seenName := map[string]bool{}
	for _, s := range p.Scenarios {
		if prev, dup := seenID[s.ID]; dup {
			// IDs join audit events to reports. A duplicate silently merges
			// two scenarios' evidence into one, which is worse than a load
			// failure because the resulting score looks plausible.
			errs = append(errs, fmt.Errorf("pack %q: duplicate scenario ID %q (used by %q and %q)", p.Name, s.ID, prev, s.Name))
		}
		seenID[s.ID] = s.Name

		if seenName[s.Name] {
			errs = append(errs, fmt.Errorf("pack %q: duplicate scenario name %q", p.Name, s.Name))
		}
		seenName[s.Name] = true

		if err := s.Validate(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// LoadPack reads every .yaml and .json file in dir and returns them as one
// pack named after dir.
//
// Each file holds one scenario. One file per scenario rather than one file
// per pack because scenarios are reviewed and argued about individually, and
// a diff that touches one fixture should not re-indent ten others.
//
// The pack is validated before it is returned. A pack that does not load is
// preferable to a pack that loads and grades wrongly.
func LoadPack(fsys fs.FS, dir string) (Pack, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return Pack{}, fmt.Errorf("scenario: read pack dir %q: %w", dir, err)
	}

	pack := Pack{Name: path.Base(dir)}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch strings.ToLower(path.Ext(e.Name())) {
		case ".yaml", ".yml", ".json":
			names = append(names, e.Name())
		}
	}
	// Sorted so that a pack's order — and therefore any run order derived
	// from it — is the same on every machine and in every filesystem.
	sort.Strings(names)

	if len(names) == 0 {
		return Pack{}, fmt.Errorf("scenario: pack dir %q contains no scenario files", dir)
	}

	var errs []error
	for _, name := range names {
		s, err := loadScenarioFile(fsys, path.Join(dir, name))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		pack.Scenarios = append(pack.Scenarios, s)
	}
	if err := errors.Join(errs...); err != nil {
		return Pack{}, err
	}

	if err := pack.Validate(); err != nil {
		return Pack{}, fmt.Errorf("scenario: pack %q is invalid: %w", pack.Name, err)
	}
	return pack, nil
}

func loadScenarioFile(fsys fs.FS, name string) (Scenario, error) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return Scenario{}, fmt.Errorf("scenario: read %q: %w", name, err)
	}
	var s Scenario
	// sigs.k8s.io/yaml converts to JSON and unmarshals through encoding/json,
	// so FaultManifest's custom duration handling ("2m" as well as a
	// nanosecond count) works from YAML unchanged.
	if err := yaml.UnmarshalStrict(data, &s); err != nil {
		return Scenario{}, fmt.Errorf("scenario: parse %q: %w", name, err)
	}
	return s, nil
}
