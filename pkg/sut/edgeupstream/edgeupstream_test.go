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

package edgeupstream

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// doc is the part of a manifest these tests read.
type doc struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
}

func parseDocs(t *testing.T) []doc {
	t.Helper()
	var out []doc
	for i, raw := range strings.Split(string(manifestsYAML), "\n---\n") {
		if strings.TrimSpace(stripComments(raw)) == "" {
			continue
		}
		var d doc
		if err := yaml.Unmarshal([]byte(raw), &d); err != nil {
			t.Fatalf("document %d does not parse: %v", i, err)
		}
		out = append(out, d)
	}
	return out
}

// section returns the manifest text from one marker up to the next, so a test
// can read one document without parsing the whole thing. A missing marker is
// fatal rather than an empty match: a renamed field should fail the test that
// depends on it, not quietly pass it on a zero-length string.
func section(t *testing.T, from, to string) string {
	t.Helper()
	y := string(manifestsYAML)
	start := strings.Index(y, from)
	if start < 0 {
		t.Fatalf("the manifests no longer contain %q", from)
	}
	rest := y[start:]
	if to == "" {
		return rest
	}
	end := strings.Index(rest, to)
	if end < 0 {
		t.Fatalf("the manifests no longer contain %q after %q", to, from)
	}
	return rest[:end]
}

func stripComments(s string) string {
	var kept []string
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// The Manager applies these in order and deletes them in reverse. nginx
// resolves its proxy_pass host at config load and mounts its config from a
// ConfigMap, so a Deployment that lands before either exists is a Deployment
// that restarts — and a restart count nobody injected is a finding a subject
// will report and be charged for inventing.
func TestTheDeploymentsComeLastSoTheyHaveSomethingToResolve(t *testing.T) {
	docs := parseDocs(t)
	seenDeployment := false
	for _, d := range docs {
		if d.Kind == "Deployment" {
			seenDeployment = true
			continue
		}
		if seenDeployment {
			t.Errorf("%s/%s is applied after a Deployment; ConfigMaps and Services must come first",
				d.Kind, d.Metadata.Name)
		}
	}
	if !seenDeployment {
		t.Fatal("no Deployment in the manifests")
	}
}

// Every workload the baseline waits on has to actually be in the manifests,
// or Deploy waits out its whole ReadyTimeout on something that will never
// appear and the scenario dies of a substrate typo.
func TestEveryExpectedWorkloadIsInTheManifests(t *testing.T) {
	docs := parseDocs(t)
	have := map[string]bool{}
	for _, d := range docs {
		have[d.Kind+"/"+d.Metadata.Name] = true
	}
	e := &edgeUpstream{}
	for _, w := range e.ExpectedWorkloads() {
		if !have[w.Kind+"/"+w.Name] {
			t.Errorf("ExpectedWorkloads names %s/%s, which the manifests do not create", w.Kind, w.Name)
		}
	}
}

// The names are the contract. A scenario's fault selects on them and its
// expectations are matched against them, and a substrate that renamed a tier
// would break every fixture pointed at it — silently, because a selector that
// matches nothing still applies.
func TestTheWorkloadNamesAreTheOnesScenariosUse(t *testing.T) {
	if EdgeWorkload != "edge" || UpstreamWorkload != "upstream" {
		t.Fatalf("workload names changed: edge=%q upstream=%q", EdgeWorkload, UpstreamWorkload)
	}
}

// Liveness must not go through the proxy. If it did, a slow upstream would
// get the edge killed, and CrashLoopBackOff is a different incident with a
// different diagnosis — one the fixture did not inject and the subject would
// be right to report.
func TestTheEdgeIsNotRestartedByASlowUpstream(t *testing.T) {
	edge := section(t, "name: edge-config", "")
	if !strings.Contains(edge, `path: /healthz`) {
		t.Fatal("the edge's liveness probe does not use the local /healthz path")
	}
	// The proxied path is the readiness one. Belt and braces: if these ever
	// converge, the pair of scenarios stops measuring what it claims to.
	if strings.Count(edge, "proxy_pass") != 1 {
		t.Errorf("the edge proxies on %d locations; exactly one — the readiness path — should", strings.Count(edge, "proxy_pass"))
	}
}

// A CPU limit on the upstream is load-bearing. StressChaos joins the target's
// cgroup: with no limit the stressors compete for a whole node and mostly
// lose, the upstream stays fast, and stress-real quietly stops producing the
// symptom it is scored on.
func TestTheUpstreamHasACPULimitForTheStressorsToBiteAgainst(t *testing.T) {
	upstream := section(t, "kind: Deployment", "name: edge")
	if !strings.Contains(upstream, "cpu: 200m") {
		t.Error("the upstream container has no CPU limit; StressChaos has nothing to saturate")
	}
}

// The CGI script must not greet the client before it does the work.
//
// This one was found the expensive way. busybox httpd streams CGI output as
// the script produces it, so a script that echoes its headers first hands the
// caller a 200 status line in milliseconds and only then goes slow. nginx
// forwards the 200, kubelet sees a 200, and the edge stays Ready while every
// response through it is truncated — the fixture applies its fault, passes
// nothing, and grades a subject on a symptom that is not there.
func TestTheCGIScriptDoesTheWorkBeforeItAnswers(t *testing.T) {
	script := section(t, "  work: |", "  healthz: |")
	loop := strings.Index(script, "while [")
	header := strings.Index(script, "Content-type")
	if loop < 0 || header < 0 {
		t.Fatalf("the CGI script no longer has both a loop and a header:\n%s", script)
	}
	if loop > header {
		t.Error("the CGI script emits its headers before doing the work; the caller sees a fast 200 and a truncated body")
	}
}

func TestRegisterIsIdempotentlyDescribable(t *testing.T) {
	e := &edgeUpstream{}
	if e.Name() != Name {
		t.Errorf("Name() = %q, want %q", e.Name(), Name)
	}
	if strings.TrimSpace(e.Description()) == "" {
		t.Error("Description() is empty; `simian sut list` would show a blank row")
	}
	if len(e.Manifests()) == 0 {
		t.Fatal("Manifests() is empty")
	}
	// Manifests hands out a copy, so a caller that scribbles on it does not
	// corrupt every later deploy in the process.
	first := e.Manifests()
	first[0] = 'x'
	if e.Manifests()[0] == 'x' {
		t.Error("Manifests() returned the embedded buffer, not a copy")
	}
}
