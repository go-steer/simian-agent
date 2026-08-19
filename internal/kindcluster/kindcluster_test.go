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

package kindcluster

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The guards in this package are the reason it is safe to run a fault injector
// from a laptop, so they are tested without a cluster: a check that only runs
// when Docker is up is a check that does not run in CI.

func TestCheckNameRejectsForeignClusters(t *testing.T) {
	tests := []struct {
		name    string
		cluster string
		wantErr bool
	}{
		{"ours", DefaultName, false},
		{"ours with suffix", NamePrefix + "pr-1234", false},
		{"someone's dev cluster", "kind", true},
		{"a real-looking name", "prod-us-east1", true},
		{"empty", "", true},
		{"prefix only", NamePrefix, true},
		{"prefix not at the start", "not-" + NamePrefix + "dev", true},
		// Guards against a "contains" implementation: this name embeds the
		// prefix but is not one of ours.
		{"prefix in the middle", "acme-" + NamePrefix + "x", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkName(tc.cluster)
			if (err != nil) != tc.wantErr {
				t.Fatalf("checkName(%q) = %v, wantErr %v", tc.cluster, err, tc.wantErr)
			}
		})
	}
}

// Create and Delete must refuse a foreign name before they reach the container
// runtime, so the guard holds on a machine where kind is not even installed.
func TestNameGuardRunsBeforeAnyToolLookup(t *testing.T) {
	ctx := context.Background()

	if _, err := Create(ctx, Config{Name: "prod-us-east1"}); err == nil {
		t.Error("Create accepted a foreign cluster name")
	} else if !strings.Contains(err.Error(), NamePrefix) {
		t.Errorf("Create error should name the required prefix, got: %v", err)
	}

	c := &Cluster{Name: "prod-us-east1", Context: "prod-us-east1"}
	if err := c.Delete(ctx); err == nil {
		t.Error("Delete accepted a foreign cluster name")
	}
	if err := c.LoadImage(ctx, "busybox:1.36"); err == nil {
		t.Error("LoadImage accepted a foreign cluster name")
	}
	if err := destroy(ctx, "prod-us-east1"); err == nil {
		t.Error("destroy accepted a foreign cluster name")
	}
}

func TestVerifyKubeconfigRequiresExactlyOurContext(t *testing.T) {
	const ours = "kind-" + DefaultName

	single := `apiVersion: v1
kind: Config
current-context: ` + ours + `
clusters:
- cluster: {server: https://127.0.0.1:6443}
  name: ` + ours + `
contexts:
- context:
    cluster: ` + ours + `
    user: ` + ours + `
  name: ` + ours + `
`
	// The shape kind produces when it merges into a file that already had a
	// cluster in it. Everything about this file looks right — the
	// current-context is ours — but a dropped --context flag downstream would
	// reach the other cluster.
	merged := single + `- context:
    cluster: prod
    user: prod
  name: prod
`

	tests := []struct {
		name    string
		text    string
		wantErr bool
	}{
		{"single context", single, false},
		{"quoted current-context", strings.Replace(single,
			"current-context: "+ours, `current-context: "`+ours+`"`, 1), false},
		{"merged with a second context", merged, true},
		{"points at another cluster", strings.Replace(single,
			"current-context: "+ours, "current-context: prod", 1), true},
		{"no current-context", strings.Replace(single,
			"current-context: "+ours+"\n", "", 1), true},
		{"empty", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyKubeconfig(tc.text, ours, "/tmp/e2e.yaml")
			if (err != nil) != tc.wantErr {
				t.Fatalf("verifyKubeconfig() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestPrepareKubeconfigRefusesToMergeIntoAnExistingFile(t *testing.T) {
	dir := t.TempDir()

	// The failure this prevents: pointing Kubeconfig at ~/.kube/config. kind
	// would merge rather than replace, and every context on the machine would
	// be back in reach.
	existing := filepath.Join(dir, "config")
	if err := os.WriteFile(existing, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := prepareKubeconfig(Config{Kubeconfig: existing}); err == nil {
		t.Fatal("prepareKubeconfig accepted an existing kubeconfig")
	}

	fresh := filepath.Join(dir, "e2e.yaml")
	path, owns, err := prepareKubeconfig(Config{Kubeconfig: fresh})
	if err != nil {
		t.Fatalf("prepareKubeconfig(fresh): %v", err)
	}
	if path != fresh {
		t.Errorf("path = %q, want %q", path, fresh)
	}
	// A caller-supplied path is the caller's to remove; Delete must not take a
	// file it did not make.
	if owns {
		t.Error("owns = true for a caller-supplied path, want false")
	}

	path, owns, err = prepareKubeconfig(Config{})
	if err != nil {
		t.Fatalf("prepareKubeconfig(empty): %v", err)
	}
	if !owns {
		t.Error("owns = false for a generated temp path, want true")
	}
	// "The file exists" must mean "the cluster exists", so nothing is left
	// behind for kind to merge into.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("temp kubeconfig %s should not exist yet, stat err = %v", path, err)
		_ = os.Remove(path)
	}
}

// `kindctl down --kubeconfig ~/.kube/config` must not be a way to delete real
// credentials, so the same one-context proof that makes the file safe to hand
// to the executor gates deleting it.
func TestRemoveKubeconfigWillNotDeleteSomeoneElsesCredentials(t *testing.T) {
	dir := t.TempDir()
	c := &Cluster{Name: DefaultName, Context: ContextPrefix + DefaultName}

	ours := `current-context: ` + c.Context + `
contexts:
- context: {cluster: ` + c.Context + `}
  name: ` + c.Context + `
`
	realOne := `current-context: prod
contexts:
- context: {cluster: prod}
  name: prod
`

	tests := []struct {
		name       string
		body       string
		wantErr    bool
		wantRemove bool
	}{
		{"ours", ours, false, true},
		{"a real kubeconfig", realOne, true, false},
		{"merged, ours is current", ours + "- context: {cluster: prod}\n  name: prod\n", true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-")+".yaml")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			c.Kubeconfig = path
			err := c.RemoveKubeconfig()
			if (err != nil) != tc.wantErr {
				t.Fatalf("RemoveKubeconfig() = %v, wantErr %v", err, tc.wantErr)
			}
			_, statErr := os.Stat(path)
			if removed := os.IsNotExist(statErr); removed != tc.wantRemove {
				t.Errorf("file removed = %v, want %v", removed, tc.wantRemove)
			}
		})
	}

	// Idempotent: `make cluster-down` twice must not be an error.
	c.Kubeconfig = filepath.Join(dir, "absent.yaml")
	if err := c.RemoveKubeconfig(); err != nil {
		t.Errorf("RemoveKubeconfig() on a missing file = %v, want nil", err)
	}
}

func TestParseClusterList(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want []string
	}{
		{"empty", "", nil},
		{"none found", "No kind clusters found.\n", nil},
		{"one", DefaultName + "\n", []string{DefaultName}},
		{"several with blanks", "\nkind\n" + DefaultName + "\n\nother\n",
			[]string{"kind", DefaultName, "other"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseClusterList(tc.out); !slices.Equal(got, tc.want) {
				t.Errorf("parseClusterList(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}

func TestFilterEnvDropsTheNamedKeysOnly(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		"KUBECONFIG=/home/dev/.kube/config",
		"DOCKER_HOST=unix:///var/run/docker.sock",
		"KUBECONFIG_BACKUP=/home/dev/.kube/config.bak",
	}
	got := filterEnv(env, "KUBECONFIG")
	want := []string{
		"PATH=/usr/bin",
		"DOCKER_HOST=unix:///var/run/docker.sock",
		// Prefix match would wrongly drop this one.
		"KUBECONFIG_BACKUP=/home/dev/.kube/config.bak",
	}
	if !slices.Equal(got, want) {
		t.Errorf("filterEnv() = %v, want %v", got, want)
	}
}

// The pin is only real if the child process cannot see the caller's
// KUBECONFIG. Asserted by running a real process and reading its environment.
func TestRunGivesTheChildOnlyOurKubeconfig(t *testing.T) {
	if _, err := exec.LookPath("env"); err != nil {
		t.Skip("env(1) not available")
	}
	t.Setenv("KUBECONFIG", "/home/dev/.kube/config")
	t.Setenv("SIMIAN_SECRET", "leaked")

	c := &Cluster{Name: DefaultName, Context: ContextPrefix + DefaultName, Kubeconfig: "/tmp/e2e.yaml"}
	out, err := c.run(context.Background(), "env", nil)
	if err != nil {
		t.Fatalf("run env: %v", err)
	}
	if !strings.Contains(out, "KUBECONFIG=/tmp/e2e.yaml") {
		t.Errorf("child KUBECONFIG not pinned to ours:\n%s", out)
	}
	if strings.Contains(out, "/home/dev/.kube/config") {
		t.Errorf("caller's KUBECONFIG reached the child:\n%s", out)
	}
	if strings.Contains(out, "SIMIAN_SECRET") {
		t.Errorf("environment is inherited rather than built from scratch:\n%s", out)
	}
}

func TestRunPassesStdinAndReportsFailures(t *testing.T) {
	if _, err := exec.LookPath("cat"); err != nil {
		t.Skip("cat(1) not available")
	}
	c := &Cluster{Name: DefaultName, Kubeconfig: "/tmp/e2e.yaml"}

	out, err := c.run(context.Background(), "cat", strings.NewReader("manifest-body"))
	if err != nil {
		t.Fatalf("run cat: %v", err)
	}
	if out != "manifest-body" {
		t.Errorf("stdin not piped through: got %q", out)
	}

	// Combined output has to come back with the error: a kubectl failure whose
	// message is discarded is unactionable in CI logs.
	if _, err := c.run(context.Background(), "false", nil); err == nil {
		t.Error("run(false) returned nil error")
	}
}

func TestDurationArg(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{2 * time.Minute, "120s"},
		{90 * time.Second, "90s"},
		{0, "300s"},
		{-1 * time.Second, "300s"},
		// Truncation, not rounding — kubectl rejects a fractional suffix.
		{1500 * time.Millisecond, "1s"},
	}
	for _, tc := range tests {
		if got := durationArg(tc.in); got != tc.want {
			t.Errorf("durationArg(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The chart and manifest URLs are built by string concatenation, so a mistyped
// constant would only surface as a download failure minutes into a CI run.
func TestPinnedArtifactURLsCarryTheirVersions(t *testing.T) {
	if !strings.Contains(calicoManifest, CalicoVersion) {
		t.Errorf("calico manifest URL %q does not carry version %q", calicoManifest, CalicoVersion)
	}
	if !strings.Contains(chaosMeshChart, ChaosMeshVersion) {
		t.Errorf("chaos-mesh chart URL %q does not carry version %q", chaosMeshChart, ChaosMeshVersion)
	}
	// A `latest`/`master` pin would make eval scores move for reasons unrelated
	// to the subject under test.
	for _, u := range []string{calicoManifest, chaosMeshChart} {
		for _, floating := range []string{"latest", "master", "/main/"} {
			if strings.Contains(u, floating) {
				t.Errorf("%q is not pinned: contains %q", u, floating)
			}
		}
	}
}
