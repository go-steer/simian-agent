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
	"io/fs"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

const goodScenarioYAML = `
id: s-image-pull
name: image-pull
prompt: Check namespace shop and report what you find.
source: pack:parity
severity: critical
faults:
  - engine: kube-state
    api_version: apps/v1
    resource_kind: ImageUnresolvable
    duration: 5m
    targets:
      - namespace: shop
expect:
  - kind: Pod
    name: checkout-api
    reasons: [ImagePullBackOff, ErrImagePull]
    also_accept_kinds: [Deployment]
    min_severity: warning
    root: true
`

func mapFS(files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return fsys
}

func TestLoadPack(t *testing.T) {
	pack, err := LoadPack(mapFS(map[string]string{
		"packs/parity/image-pull.yaml": goodScenarioYAML,
	}), "packs/parity")
	if err != nil {
		t.Fatalf("LoadPack() = %v", err)
	}
	if pack.Name != "parity" {
		t.Errorf("pack.Name = %q, want %q", pack.Name, "parity")
	}
	if pack.Len() != 1 {
		t.Fatalf("pack.Len() = %d, want 1", pack.Len())
	}

	s, ok := pack.ByID("s-image-pull")
	if !ok {
		t.Fatal("ByID did not find the scenario")
	}
	if s.Source != SourcePackParity {
		t.Errorf("Source = %q, want %q", s.Source, SourcePackParity)
	}
	if s.Severity != SeverityCritical {
		t.Errorf("Severity = %q, want %q", s.Severity, SeverityCritical)
	}
	if len(s.Expect) != 1 || !s.Expect[0].Root {
		t.Errorf("Expect = %+v, want one root expectation", s.Expect)
	}
	if got := s.Expect[0].AlsoAcceptKinds; len(got) != 1 || got[0] != "Deployment" {
		t.Errorf("AlsoAcceptKinds = %v, want [Deployment]", got)
	}
}

// FaultManifest carries a custom JSON unmarshaller so a duration can be
// written the way a human writes it. Loading through sigs.k8s.io/yaml routes
// via encoding/json, which is the only reason "5m" works here — a different
// YAML library would silently produce a zero duration and every fault would
// expire instantly.
func TestLoadPackParsesHumanDurations(t *testing.T) {
	pack, err := LoadPack(mapFS(map[string]string{
		"packs/parity/image-pull.yaml": goodScenarioYAML,
	}), "packs/parity")
	if err != nil {
		t.Fatalf("LoadPack() = %v", err)
	}
	if got := pack.Scenarios[0].Faults[0].Duration; got != 5*time.Minute {
		t.Errorf("Duration = %v, want 5m", got)
	}
}

// reverseDirFS is a filesystem that hands back directory entries in reverse
// order.
//
// It exists because fstest.MapFS sorts its own entries, which makes it
// useless for testing that LoadPack sorts: the test passes whether or not the
// sort is there. fs.ReadDir only sorts when it has to fall back to reading
// the directory as a file — an FS implementing fs.ReadDirFS gets to return
// whatever order it likes, and os.DirFS on some platforms does exactly that.
type reverseDirFS struct{ fstest.MapFS }

func (r reverseDirFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := r.MapFS.ReadDir(name)
	if err != nil {
		return nil, err
	}
	slices.Reverse(entries)
	return entries, nil
}

// Filesystem order is not stable across machines, and a pack whose order
// drifts produces run orders that cannot be compared or reproduced.
func TestLoadPackOrdersScenariosByFilename(t *testing.T) {
	body := func(id, name string) string {
		return strings.NewReplacer("s-image-pull", id, "name: image-pull", "name: "+name).Replace(goodScenarioYAML)
	}
	files := map[string]string{
		"p/c.yaml": body("s-c", "c"),
		"p/a.yaml": body("s-a", "a"),
		"p/b.yaml": body("s-b", "b"),
	}

	for name, fsys := range map[string]fs.FS{
		"sorted source":   mapFS(files),
		"unsorted source": reverseDirFS{mapFS(files)},
	} {
		t.Run(name, func(t *testing.T) {
			pack, err := LoadPack(fsys, "p")
			if err != nil {
				t.Fatalf("LoadPack() = %v", err)
			}
			var got []string
			for _, s := range pack.Scenarios {
				got = append(got, s.ID)
			}
			if strings.Join(got, ",") != "s-a,s-b,s-c" {
				t.Errorf("order = %v, want s-a,s-b,s-c", got)
			}
		})
	}
}

// Two scenarios sharing an ID would merge each other's audit evidence into
// one record and produce a score that looks entirely plausible. That is worse
// than refusing to load.
func TestLoadPackRejectsDuplicateIDs(t *testing.T) {
	body := strings.Replace(goodScenarioYAML, "name: image-pull", "name: other", 1)
	_, err := LoadPack(mapFS(map[string]string{
		"p/a.yaml": goodScenarioYAML,
		"p/b.yaml": body,
	}), "p")
	if err == nil || !strings.Contains(err.Error(), "duplicate scenario ID") {
		t.Fatalf("LoadPack() = %v; want a duplicate-ID error", err)
	}
}

func TestLoadPackRejectsDuplicateNames(t *testing.T) {
	body := strings.Replace(goodScenarioYAML, "id: s-image-pull", "id: s-other", 1)
	_, err := LoadPack(mapFS(map[string]string{
		"p/a.yaml": goodScenarioYAML,
		"p/b.yaml": body,
	}), "p")
	if err == nil || !strings.Contains(err.Error(), "duplicate scenario name") {
		t.Fatalf("LoadPack() = %v; want a duplicate-name error", err)
	}
}

// A pack that loads and grades wrongly is worse than one that refuses to
// load, so validation runs at load time rather than at score time.
func TestLoadPackValidatesEveryScenario(t *testing.T) {
	leaky := strings.Replace(goodScenarioYAML,
		"prompt: Check namespace shop and report what you find.",
		"prompt: The image in namespace shop is in ImagePullBackOff.", 1)
	_, err := LoadPack(mapFS(map[string]string{"p/a.yaml": leaky}), "p")
	if err == nil || !strings.Contains(err.Error(), "leaks the diagnosis") {
		t.Fatalf("LoadPack() = %v; want the prompt lint to fire at load time", err)
	}
}

// A typo'd field is silence otherwise: the scenario loads, the field is
// ignored, and the expectation it was meant to set is simply absent.
func TestLoadPackRejectsUnknownFields(t *testing.T) {
	typo := strings.Replace(goodScenarioYAML, "    root: true", "    rooot: true", 1)
	_, err := LoadPack(mapFS(map[string]string{"p/a.yaml": typo}), "p")
	if err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("LoadPack() = %v; want a parse error for the unknown field", err)
	}
}

func TestLoadPackIgnoresNonScenarioFiles(t *testing.T) {
	pack, err := LoadPack(mapFS(map[string]string{
		"p/a.yaml":    goodScenarioYAML,
		"p/README.md": "not a scenario",
		"p/.gitkeep":  "",
	}), "p")
	if err != nil {
		t.Fatalf("LoadPack() = %v", err)
	}
	if pack.Len() != 1 {
		t.Errorf("pack.Len() = %d, want 1", pack.Len())
	}
}

func TestLoadPackRejectsAnEmptyDir(t *testing.T) {
	_, err := LoadPack(mapFS(map[string]string{"p/README.md": "x"}), "p")
	if err == nil || !strings.Contains(err.Error(), "no scenario files") {
		t.Fatalf("LoadPack() = %v; want an empty-pack error", err)
	}
}

func TestLoadPackRejectsAMissingDir(t *testing.T) {
	if _, err := LoadPack(mapFS(nil), "nope"); err == nil {
		t.Fatal("LoadPack() on a missing dir = nil; want an error")
	}
}

func TestPackControls(t *testing.T) {
	control := `
id: s-healthy
name: healthy
prompt: Check namespace shop and report what you find.
source: pack:parity
`
	pack, err := LoadPack(mapFS(map[string]string{
		"p/a.yaml": goodScenarioYAML,
		"p/b.yaml": control,
	}), "p")
	if err != nil {
		t.Fatalf("LoadPack() = %v", err)
	}
	controls := pack.Controls()
	if len(controls) != 1 || controls[0].ID != "s-healthy" {
		t.Errorf("Controls() = %+v, want just s-healthy", controls)
	}
}

func TestPackByIDMisses(t *testing.T) {
	pack := Pack{Scenarios: []Scenario{{ID: "a"}}}
	if _, ok := pack.ByID("b"); ok {
		t.Error("ByID found a scenario that is not there")
	}
}
