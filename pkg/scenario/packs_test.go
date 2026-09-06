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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/go-steer/simian-agent/pkg/catalog"
	"github.com/go-steer/simian-agent/pkg/simian"
	"github.com/go-steer/simian-agent/pkg/sut"
	"github.com/go-steer/simian-agent/pkg/sut/edgeupstream"
)

// The packs are validated on load, so this is the test that the shipped packs
// are well-formed at all: LoadPack refuses a duplicate ID, an unknown source, a
// prompt that leaks its own answer, and a fault with no efficacy gate.
func TestEveryBuiltinPackLoads(t *testing.T) {
	for _, name := range BuiltinPacks {
		pack, err := Builtin(name)
		if err != nil {
			t.Errorf("Builtin(%q) = %v", name, err)
			continue
		}
		if pack.Name != name {
			t.Errorf("Builtin(%q).Name = %q", name, pack.Name)
		}
		if pack.Len() == 0 {
			t.Errorf("Builtin(%q) is empty", name)
		}
	}
}

// A pack in the tree that nothing loads is a pack nothing validates: LoadPack
// is the only thing that runs Validate, so an unregistered directory could
// carry a duplicate ID or a leaking prompt indefinitely.
func TestEveryPackDirectoryIsRegistered(t *testing.T) {
	dirs, err := builtinPackDirs()
	if err != nil {
		t.Fatalf("builtinPackDirs() = %v", err)
	}
	registered := slices.Clone(BuiltinPacks)
	slices.Sort(registered)
	if !slices.Equal(dirs, registered) {
		t.Errorf("packs/ holds %v, BuiltinPacks names %v", dirs, registered)
	}
}

// LoadRef is what --pack takes, and the thing it has to get right is which of
// the two kinds of reference it was handed.
func TestLoadRefResolvesBuiltinNamesAndPaths(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "custom"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "custom", "s.yaml"), []byte(refScenario), 0o600); err != nil {
		t.Fatalf("write scenario: %v", err)
	}

	cases := []struct {
		name string
		ref  string
		want string // pack name, or "" for an error
		errs string
	}{
		{"a built-in name", PackParity, "parity", ""},
		{"the other built-in name", PackLookout, "lookout", ""},
		{"a path", filepath.Join(dir, "custom"), "custom", ""},
		{"a path with a trailing slash", filepath.Join(dir, "custom") + string(filepath.Separator), "custom", ""},
		{"a path that is not there", filepath.Join(dir, "nope"), "", "open pack"},
		{"a file", filepath.Join(dir, "custom", "s.yaml"), "", "not a directory"},
		{"a name that is neither", "paritty", "", "neither a built-in pack"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pack, err := LoadRef(tc.ref)
			if tc.errs != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errs) {
					t.Fatalf("LoadRef(%q) = %v, want an error mentioning %q", tc.ref, err, tc.errs)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadRef(%q) = %v", tc.ref, err)
			}
			if pack.Name != tc.want {
				t.Errorf("LoadRef(%q).Name = %q, want %q", tc.ref, pack.Name, tc.want)
			}
		})
	}
}

// A bare built-in name means the built-in pack wherever it is run from. The
// working directory deciding what `--pack parity` scores would make two
// scorecards carrying the same pack name incomparable, which is the one thing
// the parity pack exists to prevent — so a local directory of the same name has
// to be spelled as a path.
func TestLoadRefPrefersTheBuiltinOverALocalDirectoryOfTheSameName(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, PackParity), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, PackParity, "s.yaml"), []byte(refScenario), 0o600); err != nil {
		t.Fatalf("write scenario: %v", err)
	}
	t.Chdir(dir)

	builtin, err := LoadRef(PackParity)
	if err != nil {
		t.Fatalf("LoadRef(%q) = %v", PackParity, err)
	}
	if builtin.Len() != MustBuiltin(PackParity).Len() {
		t.Errorf("bare %q loaded %d scenarios, want the built-in pack's %d",
			PackParity, builtin.Len(), MustBuiltin(PackParity).Len())
	}

	local, err := LoadRef("." + string(filepath.Separator) + PackParity)
	if err != nil {
		t.Fatalf("LoadRef(./%s) = %v", PackParity, err)
	}
	if local.Len() != 1 {
		t.Errorf("./%s loaded %d scenarios, want the local directory's 1", PackParity, local.Len())
	}
}

const refScenario = `id: ref-0001
name: a scenario to load
prompt: Assess the health of the "ref-shop" namespace and report what you find.
source: pack:parity
severity: warning
faults:
  - engine: kube-state
    api_version: apps/v1
    resource_kind: ImageUnresolvable
    duration: 5m
    blast_radius_tier: namespace
    spec:
      name: catalog
    targets:
      - namespace: ref-shop
expect:
  - kind: Pod
    name: catalog
    reasons: ["ImagePullBackOff"]
`

// Recall alone is trivially gamed by a subject that reports every failure mode
// it knows about in every namespace. The control is what makes precision cost
// something, so a pack without one measures half of what it claims to.
func TestEveryBuiltinPackHasAControl(t *testing.T) {
	for _, name := range BuiltinPacks {
		pack := MustBuiltin(name)
		if len(pack.Controls()) == 0 {
			t.Errorf("pack %q has no control scenario", name)
		}
	}
}

// Every scenario's ID must be unique across *all* packs, not just within one.
// Pack.Validate enforces the within-pack half; this is the other half, and it
// matters because the ID is what joins an audit event to a report. Two packs
// run in one session with a shared ID would merge their evidence, and the
// resulting score would look plausible.
func TestScenarioIDsAreUniqueAcrossPacks(t *testing.T) {
	seen := map[string]string{}
	for _, name := range BuiltinPacks {
		for _, s := range MustBuiltin(name).Scenarios {
			if prev, dup := seen[s.ID]; dup {
				t.Errorf("scenario ID %q is used by pack %q and pack %q", s.ID, prev, name)
			}
			seen[s.ID] = name
		}
	}
}

// The pack name is part of the source, and a score is only comparable within
// its source. A lookout scenario labelled pack:parity would be averaged into
// the wrong yardstick.
func TestEveryScenarioDeclaresItsOwnPacksSource(t *testing.T) {
	want := map[string]Source{
		PackParity:    SourcePackParity,
		PackLookout:   SourcePackLookout,
		PackDataplane: SourcePackDataplane,
	}
	for _, name := range BuiltinPacks {
		for _, s := range MustBuiltin(name).Scenarios {
			if s.Source != want[name] {
				t.Errorf("pack %q scenario %q: Source = %q, want %q", name, s.ID, s.Source, want[name])
			}
		}
	}
}

// Two scenarios that share a namespace cannot be run at the same time without
// each grading the other's faults as findings in its own namespace. Nothing
// enforces sequencing today, so the packs are written so that the question does
// not arise.
func TestNoTwoScenariosShareANamespace(t *testing.T) {
	seen := map[string]string{}
	for _, name := range BuiltinPacks {
		for _, s := range MustBuiltin(name).Scenarios {
			for _, ns := range namespacesOf(s) {
				if prev, dup := seen[ns]; dup && prev != s.ID {
					t.Errorf("namespace %q is used by scenario %q and scenario %q", ns, prev, s.ID)
				}
				seen[ns] = s.ID
			}
		}
	}
}

// A scenario whose faults spanned two namespaces would be asking the subject
// about somewhere it was never pointed at: the prompt names one namespace, and
// Finding.Namespace is used to discard findings about anywhere else.
func TestEveryScenarioTargetsExactlyOneNamespace(t *testing.T) {
	for _, name := range BuiltinPacks {
		for _, s := range MustBuiltin(name).Scenarios {
			ns := namespacesOf(s)
			if len(ns) != 1 {
				t.Errorf("pack %q scenario %q: targets %v, want exactly one namespace", name, s.ID, ns)
				continue
			}
			if !strings.Contains(s.Prompt, ns[0]) {
				t.Errorf("pack %q scenario %q: prompt does not name its namespace %q: %q",
					name, s.ID, ns[0], s.Prompt)
			}
		}
	}
}

// Every shipped fault is namespace-tier and carries a Settle gate.
//
// Validate already refuses an ungated fault, so the gate half is belt and
// braces. The tier half is not: a node-tier fault in a shipped pack would break
// workloads nobody consented to break, on a cluster somebody ran `simian score`
// against expecting a namespace to be the blast radius.
//
// The gate may be the default one or written into the manifest. It used to have
// to be the default one, which was true for as long as every pack was
// kube-state: a synthesized fault kind has one way of proving itself and the
// catalog knows it. A dataplane fault does not. StressChaos has no default gate
// at all — there is no field on any object that says a cgroup is full — and a
// NetworkChaos whose gate is written out on purpose suppresses the default by
// reusing its probe names. What the shipped packs owe is a gate, not a
// particular provenance for one.
func TestEveryShippedFaultIsNamespaceTierAndGated(t *testing.T) {
	for _, name := range BuiltinPacks {
		for _, s := range MustBuiltin(name).Scenarios {
			for i, f := range s.Faults {
				if f.BlastRadiusTier != simian.TierNamespace {
					t.Errorf("pack %q scenario %q fault %d: tier = %q, want %q",
						name, s.ID, i, f.BlastRadiusTier, simian.TierNamespace)
				}
				if !hasMode(gateFor(f), simian.ProbeModeSettle) {
					t.Errorf("pack %q scenario %q fault %d (%s): no Settle gate, default or declared",
						name, s.ID, i, f.ResourceKind)
				}
			}
		}
	}
}

// A fault aimed at a substrate must prove the substrate was healthy first.
//
// A kube-state fault brings its own subject matter, so there is no "before" to
// measure: the workload did not exist a moment ago and the Settle gate is the
// whole proof. A dataplane fault is aimed at something that was already there,
// and "the callee is slow" is not evidence of anything without "the callee was
// fast". Without the SOT half a substrate that came up degraded would satisfy
// the Settle gate on its own and the subject would be graded on a fault that
// contributed nothing.
//
// The control is exempt by the same rule, and it is not a special case: its
// NoOp injects nothing, so it has no before and after to separate.
func TestEveryFaultAimedAtASubstrateProvesItWasHealthyFirst(t *testing.T) {
	for _, name := range BuiltinPacks {
		for _, s := range MustBuiltin(name).Scenarios {
			if s.Substrate == "" {
				continue
			}
			for i, f := range s.Faults {
				if f.Engine == simian.EngineKubeState {
					continue
				}
				if !hasMode(gateFor(f), simian.ProbeModeSOT) {
					t.Errorf("pack %q scenario %q fault %d (%s): aimed at substrate %q with no SOT probe; its Settle gate proves nothing",
						name, s.ID, i, f.ResourceKind, s.Substrate)
				}
			}
		}
	}
}

// The dataplane pack's premise is that its faults leave no field on any
// object, which means a gate made of object reads cannot prove one landed.
//
// A k8s probe watching the caller go NotReady is worth having and is not
// evidence: the caller can go NotReady because its own image was replaced,
// because a node drained, or because the substrate came up wrong. Only a
// request proves the dataplane changed — a measured latency, an observed 503,
// a connection that used to complete and now does not. So every fault here has
// to carry an http probe on each side of the fault.
//
// This is the acceptance criterion the pack was commissioned against, and it
// is the kind that erodes: a scenario added in a hurry gets a k8s gate because
// a k8s gate is easier to write and passes just as green.
func TestEveryDataplaneFaultIsProvedByARequest(t *testing.T) {
	for _, s := range MustBuiltin(PackDataplane).Scenarios {
		for i, f := range s.Faults {
			if f.Engine == simian.EngineKubeState {
				// The control's NoOp. It injects nothing, so there is no
				// dataplane change for a request to observe.
				continue
			}
			for _, mode := range []string{simian.ProbeModeSOT, simian.ProbeModeSettle} {
				if !hasHTTPProbe(gateFor(f), mode) {
					t.Errorf("scenario %q fault %d (%s): no http probe in mode %s; nothing in this gate proves the dataplane changed rather than that the CR applied",
						s.ID, i, f.ResourceKind, mode)
				}
			}
		}
	}
}

func hasHTTPProbe(probes []simian.ProbeSpec, mode string) bool {
	for _, p := range probes {
		if p.Mode == mode && p.Type == simian.ProbeTypeHTTP {
			return true
		}
	}
	return false
}

// gateFor is every probe a fault actually runs: the ones written into the
// manifest, plus whatever the catalog attaches that the manifest did not
// already declare by name.
func gateFor(f simian.FaultManifest) []simian.ProbeSpec {
	return append(append([]simian.ProbeSpec{}, f.Probes...), catalog.DefaultProbes(f)...)
}

func hasMode(probes []simian.ProbeSpec, mode string) bool {
	for _, p := range probes {
		if p.Mode == mode {
			return true
		}
	}
	return false
}

// Every expected finding must name an object the scenario actually puts in the
// namespace.
//
// The engine appends a UID-derived suffix to a synthesized workload's name, and
// MatchesName is a prefix match, so an expectation's Name has to be the
// *requested* name — spelled the way spec.name spells it. A typo produces an
// expectation nothing can ever satisfy, which scores every subject zero on that
// scenario and looks exactly like a subject that missed the fault.
//
// A scenario with a Substrate has a second source of objects, and the same
// hazard: an expectation naming a tier the substrate does not have is
// unsatisfiable in exactly the same silent way. The names come from the SUT
// itself rather than a list here, so a substrate that renames a tier breaks
// this test instead of every score.
func TestEveryExpectationNamesAnObjectTheScenarioCreates(t *testing.T) {
	for _, name := range BuiltinPacks {
		for _, s := range MustBuiltin(name).Scenarios {
			created := map[string]bool{}
			for _, f := range s.Faults {
				if n, ok := f.Spec["name"].(string); ok {
					created[n] = true
				}
			}
			for _, w := range substrateWorkloads(t, s.Substrate) {
				created[w] = true
			}
			for _, e := range s.Expect {
				if !created[e.Name] {
					t.Errorf("pack %q scenario %q: expects %s/%s, but the scenario creates %v",
						name, s.ID, e.Kind, e.Name, keysOf(created))
				}
			}
		}
	}
}

// registerSubstrates is once because MustRegister panics on a duplicate, which
// is the right behaviour for a binary and the wrong one for a test binary that
// may reach it from more than one test.
var registerSubstrates = sync.OnceFunc(edgeupstream.Register)

func substrateWorkloads(t *testing.T, name string) []string {
	t.Helper()
	if name == "" {
		return nil
	}
	registerSubstrates()
	spec, ok := sut.Default.Get(name)
	if !ok {
		t.Fatalf("substrate %q is named by a shipped scenario but is not registered", name)
	}
	var out []string
	for _, w := range spec.ExpectedWorkloads() {
		out = append(out, w.Name)
	}
	return out
}

// Every fault names the workload it is aimed at explicitly rather than taking
// the per-kind default. The default is a fine thing for an ad-hoc `simian
// chaos` call and a bad thing here: an expectation is matched by name, so a
// kind whose default name changed would silently stop matching and every
// subject would score zero.
//
// Where the name lives depends on what the fault does. A kube-state fault
// creates its workload and names it in spec.name; a chaos-mesh fault is aimed
// at one that already exists and names it in the selector. Both are a name, and
// a fault with neither is aimed at whatever happens to be in the namespace.
func TestEveryShippedFaultNamesItsWorkload(t *testing.T) {
	for _, name := range BuiltinPacks {
		for _, s := range MustBuiltin(name).Scenarios {
			for i, f := range s.Faults {
				if strings.TrimSpace(targetWorkloadOf(f)) == "" {
					t.Errorf("pack %q scenario %q fault %d (%s): names no workload, in spec.name or in a selector",
						name, s.ID, i, f.ResourceKind)
				}
			}
		}
	}
}

// targetWorkloadOf returns the workload a fault names, from wherever its engine
// keeps it.
func targetWorkloadOf(f simian.FaultManifest) string {
	if n, ok := f.Spec["name"].(string); ok {
		return n
	}
	sel, _ := f.Spec["selector"].(map[string]any)
	labels, _ := sel["labelSelectors"].(map[string]any)
	if n, ok := labels["app"].(string); ok {
		return n
	}
	// The fault declares no selector of its own, so it is aimed at whatever its
	// target says. That is still a name, as long as there is one.
	for _, t := range f.Targets {
		if t.Name != "" {
			return t.Name
		}
		if n, ok := t.Labels["app"]; ok {
			return n
		}
	}
	return ""
}

func namespacesOf(s Scenario) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range s.Faults {
		for _, t := range f.Targets {
			if !seen[t.Namespace] {
				seen[t.Namespace] = true
				out = append(out, t.Namespace)
			}
		}
	}
	slices.Sort(out)
	return out
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
