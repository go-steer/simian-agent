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
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
)

// packFS holds the packs that ship with Simian.
//
// Embedded rather than read from disk so a scored run cannot be affected by
// what happens to be in the working directory, and so `simian` scores the same
// pack whether it was installed from a release binary or built from source. A
// pack read from a path is still supported — that is what LoadPack is for —
// but the built-in ones are part of the binary.
//
//go:embed packs
var packFS embed.FS

// Built-in pack names, as they appear on the wire and in a score report.
const (
	// PackParity mirrors the agent-side live eval fixtures one-for-one. Its
	// reason for existing is comparability: a recall number produced here and
	// one produced there should be arguable about the subject rather than
	// about the rig.
	PackParity = "parity"

	// PackLookout mirrors the observer's example scenarios. The subject is a
	// watcher rather than a triager, so the scenarios lean on faults that are
	// invisible at pod altitude.
	PackLookout = "lookout"
)

// BuiltinPacks lists every pack embedded in the binary, in a stable order.
var BuiltinPacks = []string{PackParity, PackLookout}

// Builtin returns an embedded pack by name.
//
// Loaded once and shared. Pack and Scenario are read-only by convention and
// nothing in this package mutates one, so handing out the same value is safe
// and saves re-parsing nineteen files per call. The slices inside are not
// deeply copied, which is the one thing a caller must not write to.
func Builtin(name string) (Pack, error) {
	loaded := builtinPacks()
	p, ok := loaded[name]
	if !ok {
		return Pack{}, fmt.Errorf("scenario: no built-in pack named %q (have %v)", name, BuiltinPacks)
	}
	return p.pack, p.err
}

// MustBuiltin is Builtin for callers that would have no recovery available.
//
// A built-in pack that does not load is a build-time defect, not a runtime
// condition: the files are compiled into the binary, so a failure here means
// the same failure on every machine and every invocation.
func MustBuiltin(name string) Pack {
	p, err := Builtin(name)
	if err != nil {
		panic("scenario: " + err.Error())
	}
	return p
}

// LoadRef resolves a pack reference — either a built-in name or a directory on
// disk — and loads it.
//
// This is what `--pack` takes. A bare name that matches a built-in wins over a
// directory of the same name in the working directory: the built-in packs are
// the ones a score is comparable across, and having `--pack parity` mean
// something different depending on where it was run from is exactly the
// ambiguity a benchmark cannot afford. Spell the local directory `./parity` to
// mean the local directory.
func LoadRef(ref string) (Pack, error) {
	if isBuiltinRef(ref) {
		return Builtin(ref)
	}
	info, err := os.Stat(ref)
	if err != nil {
		// A bare word that is neither a built-in nor a path is almost always a
		// misremembered pack name, so say what the names are rather than
		// reporting a file that was never going to be there.
		if errors.Is(err, fs.ErrNotExist) && !strings.ContainsRune(ref, filepath.Separator) {
			return Pack{}, fmt.Errorf("open pack: %q is neither a built-in pack (%s) nor a directory",
				ref, strings.Join(BuiltinPacks, ", "))
		}
		return Pack{}, fmt.Errorf("open pack: %w", err)
	}
	if !info.IsDir() {
		return Pack{}, fmt.Errorf("open pack: %s is not a directory", ref)
	}
	// LoadPack reads a subdirectory of the FS it is handed, so root the FS at
	// the parent and name the leaf. Cleaned first so a trailing slash does not
	// turn the leaf into "".
	dir := filepath.Clean(ref)
	return LoadPack(os.DirFS(filepath.Dir(dir)), filepath.Base(dir))
}

// isBuiltinRef reports whether a reference names a built-in pack rather than a
// path. Anything with a separator in it, and anything relative-looking, is a
// path — `./parity` and `packs/parity` are both directories.
func isBuiltinRef(ref string) bool {
	if strings.ContainsRune(ref, filepath.Separator) || strings.HasPrefix(ref, ".") {
		return false
	}
	return slices.Contains(BuiltinPacks, ref)
}

type loadedPack struct {
	pack Pack
	err  error
}

var builtinPacks = sync.OnceValue(func() map[string]loadedPack {
	out := map[string]loadedPack{}
	for _, name := range BuiltinPacks {
		p, err := LoadPack(packFS, "packs/"+name)
		out[name] = loadedPack{pack: p, err: err}
	}
	return out
})

// builtinPackDirs lists the directories under packs/, so a test can catch a
// pack that was added to the tree and never registered in BuiltinPacks. A pack
// nobody loads is a pack nobody validates.
func builtinPackDirs() ([]string, error) {
	entries, err := fs.ReadDir(packFS, "packs")
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)
	return dirs, nil
}
