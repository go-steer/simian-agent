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

// Package kindcluster manages the throwaway kind cluster that Simian's
// end-to-end tests and the eval rig run against.
//
// # Why this package is paranoid
//
// Simian is a fault injector with a cluster-scoped provisioner: it creates
// namespaces, binds RBAC, and applies chaos CRDs that kill pods and partition
// networks. A developer workstation typically has many kube contexts, some of
// them pointing at real clusters. Code that resolved the ambient
// current-context would eventually run against one of them, and the failure
// would be silent right up until it wasn't.
//
// So the pin is mechanical, not advisory, and it is enforced four ways:
//
//  1. kind writes to a kubeconfig we create, never ~/.kube/config. The file
//     handed to the executor physically cannot name a production cluster.
//  2. Only clusters whose names carry NamePrefix are created or deleted.
//  3. Create refuses to adopt a cluster that already exists — an existing
//     cluster of that name is by definition not one we made.
//  4. Every kubectl and helm invocation passes both KUBECONFIG and the
//     context, from an environment that does not inherit the caller's
//     KUBECONFIG.
//
// Any one of these would usually be enough. They are all here because the
// check costs milliseconds and the miss costs a production incident.
//
// This design is lifted from core-sre-agent's kindcluster package, which
// solves the same problem for the live eval tier. The additions here are
// cluster-config support (Simian needs a NetworkPolicy-enforcing CNI, see
// dev/kind/cluster.yaml) and Chaos Mesh installation (see chaosmesh.go).
package kindcluster

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// NamePrefix marks a cluster as ours. Create rejects a name without it and
// Delete refuses to remove a cluster that lacks it, so no code path in this
// package can touch a cluster a human made by hand.
const NamePrefix = "simian-e2e-"

// ContextPrefix is what kind prepends to a cluster name to form its context.
const ContextPrefix = "kind-"

// DefaultName is the cluster `make cluster` creates. Tests that may run
// concurrently should append their own suffix instead.
const DefaultName = NamePrefix + "dev"

// DefaultCreateTimeout bounds cluster creation. Three nodes plus a CNI that
// has to converge takes a couple of minutes; ten means something is wrong.
const DefaultCreateTimeout = 10 * time.Minute

// MinKindVersion is the oldest kind that can drive this package. Keep in sync
// with KIND_VERSION in .github/workflows/e2e-kind.yml and the install line in
// dev/README.md.
//
// The floor is not cosmetic. The node image is pinned to a Kubernetes version
// whose containerd writes a config the older kind CLI cannot parse, so
// `kind load docker-image` fails with "unknown containerd config version: 4"
// while everything else — create, CNI, Chaos Mesh, kubectl — works. A
// developer on an older kind therefore gets a cluster that comes up clean and
// then cannot be given the image under test, which is the one thing the e2e
// path exists to do. CI pins the right version and never sees it.
const MinKindVersion = "v0.32.0"

// Config describes the cluster to create.
type Config struct {
	// Name is the kind cluster name. It must start with NamePrefix. Callers
	// should make it unique per run so concurrent runs cannot collide.
	Name string

	// ConfigFile is a kind cluster config YAML (node topology, CNI choice).
	// Empty uses kind's defaults, which is almost never what Simian wants —
	// the default CNI does not enforce NetworkPolicy, so the network-policy
	// engine would silently no-op. See dev/kind/cluster.yaml.
	ConfigFile string

	// Image is the node image (e.g. "kindest/node:v1.34.8"). Empty uses the
	// default for the installed kind, or whatever ConfigFile specifies.
	Image string

	// Kubeconfig is where the cluster's credentials are written. Empty creates
	// a temp file that Delete removes. It must not be an existing kubeconfig:
	// kind merges into the file it is given, and merging into a real one would
	// defeat the isolation this package exists to provide.
	Kubeconfig string

	// CreateTimeout bounds `kind create`. Zero uses DefaultCreateTimeout.
	CreateTimeout time.Duration

	// Out receives progress lines. Nil discards them.
	Out io.Writer
}

// Cluster is a live kind cluster, pinned to its own kubeconfig.
type Cluster struct {
	// Name is the kind cluster name.
	Name string
	// Context is the kube context, always ContextPrefix + Name.
	Context string
	// Kubeconfig is the isolated credential file. Pass this and Context to
	// anything that talks to the cluster; together they are the pin.
	Kubeconfig string

	ownsKubeconfig bool
	out            io.Writer
}

// Create brings up a kind cluster and returns it pinned to its own kubeconfig.
//
// The caller must Delete it. Create is not idempotent by design — see the
// package comment on why adopting an existing cluster is refused rather than
// treated as success.
func Create(ctx context.Context, cfg Config) (c *Cluster, err error) {
	if err := checkName(cfg.Name); err != nil {
		return nil, err
	}
	for _, tool := range []string{"kind", "kubectl"} {
		if _, err := exec.LookPath(tool); err != nil {
			return nil, fmt.Errorf("kindcluster: %s is not on PATH: %w", tool, err)
		}
	}
	if err := checkKindVersion(ctx, cfg.Out); err != nil {
		return nil, err
	}
	if cfg.ConfigFile != "" {
		if _, err := os.Stat(cfg.ConfigFile); err != nil {
			return nil, fmt.Errorf("kindcluster: cluster config %s: %w", cfg.ConfigFile, err)
		}
	}

	existing, err := List(ctx)
	if err != nil {
		return nil, err
	}
	if slices.Contains(existing, cfg.Name) {
		return nil, fmt.Errorf("kindcluster: cluster %q already exists — refusing to adopt a cluster "+
			"this process did not create; run `make cluster-down` first or pick a unique name", cfg.Name)
	}

	kubeconfig, owns, err := prepareKubeconfig(cfg)
	if err != nil {
		return nil, err
	}
	// A failure between here and the end leaves a half-built cluster and a
	// stray file, both of which cost real resources.
	defer func() {
		if err == nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		_ = destroy(cleanupCtx, cfg.Name)
		if owns {
			_ = os.Remove(kubeconfig)
		}
	}()

	timeout := cfg.CreateTimeout
	if timeout == 0 {
		timeout = DefaultCreateTimeout
	}
	createCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// --wait blocks until the control-plane node reports Ready. Without it
	// `kind create` returns while the node still carries its not-ready taint
	// and the first workloads applied sit unschedulable for over a minute.
	// That delay lands inside a fault's settle budget, where it reads as "the
	// fault took four minutes to manifest" rather than "the cluster was not up
	// yet". Paying it here, once, makes the settle budget measure the fault.
	//
	// With disableDefaultCNI the node cannot reach Ready until a CNI is
	// installed, so this waits for the shortest interval that still catches a
	// broken create; readiness is asserted properly in WaitForNodes, after
	// InstallCNI.
	args := []string{
		"create", "cluster",
		"--name", cfg.Name,
		"--kubeconfig", kubeconfig,
		"--wait", "30s",
	}
	if cfg.ConfigFile != "" {
		args = append(args, "--config", cfg.ConfigFile)
	}
	if cfg.Image != "" {
		args = append(args, "--image", cfg.Image)
	}
	cl := &Cluster{
		Name:           cfg.Name,
		Context:        ContextPrefix + cfg.Name,
		Kubeconfig:     kubeconfig,
		ownsKubeconfig: owns,
		out:            cfg.Out,
	}
	cl.logf("creating kind cluster %s", cfg.Name)
	out, createErr := runKind(createCtx, args...)
	// `kind create --wait` exits non-zero when nodes stay NotReady, which is
	// the expected state with disableDefaultCNI until InstallCNI runs. The
	// cluster itself is up, so distinguish that from a real create failure by
	// asking the API server.
	if createErr != nil {
		if _, probeErr := cl.Kubectl(createCtx, "get", "nodes"); probeErr != nil {
			return nil, fmt.Errorf("kindcluster: create %s: %w\n%s", cfg.Name, createErr, out)
		}
		cl.logf("nodes not Ready yet (expected before the CNI is installed)")
	}

	if err := cl.verifyIsolation(); err != nil {
		return nil, err
	}
	return cl, nil
}

// Delete tears the cluster down and removes the kubeconfig Create made.
//
// Safe to call twice, and safe to call on a context that is already cancelled:
// teardown normally runs from a defer after the run that failed, and a
// cancelled context must not be the reason a fault-injected cluster survives.
func (c *Cluster) Delete(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if err := checkName(c.Name); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()

	c.logf("deleting kind cluster %s", c.Name)
	err := destroy(ctx, c.Name)
	if c.ownsKubeconfig {
		if rmErr := os.Remove(c.Kubeconfig); rmErr != nil && !os.IsNotExist(rmErr) && err == nil {
			err = rmErr
		}
	}
	return err
}

// RemoveKubeconfig deletes the kubeconfig file, but only after proving it
// describes this cluster and nothing else.
//
// Delete removes a kubeconfig it created itself; this is for the caller-supplied
// case, where the path came from a flag or an environment variable. Removing it
// unconditionally would mean `kindctl down --kubeconfig ~/.kube/config` deletes
// a developer's real credentials — a one-flag mistake with no undo. The same
// one-context check that makes the file safe to hand to the executor is what
// makes it safe to delete.
//
// A file that does not verify is left in place and reported.
func (c *Cluster) RemoveKubeconfig() error {
	if c.Kubeconfig == "" {
		return nil
	}
	raw, err := os.ReadFile(c.Kubeconfig)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("kindcluster: read %s: %w", c.Kubeconfig, err)
	}
	if err := verifyKubeconfig(string(raw), c.Context, c.Kubeconfig); err != nil {
		return fmt.Errorf("refusing to delete a kubeconfig that is not solely this cluster's: %w", err)
	}
	if err := os.Remove(c.Kubeconfig); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// WaitForNodes blocks until every node reports Ready.
func (c *Cluster) WaitForNodes(ctx context.Context, timeout time.Duration) error {
	c.logf("waiting for all nodes Ready")
	_, err := c.Kubectl(ctx, "wait", "--for=condition=Ready", "nodes", "--all",
		"--timeout="+durationArg(timeout))
	return err
}

// LoadImage copies an image from the host daemon into the cluster's nodes.
//
// Fixtures use it so a workload that is supposed to run does not depend on
// registry reachability at eval time. Without it a network blip turns a
// healthy control workload into an ImagePullBackOff, and the subject under
// test is then marked wrong for reporting a fault that genuinely exists.
func (c *Cluster) LoadImage(ctx context.Context, image string) error {
	if err := checkName(c.Name); err != nil {
		return err
	}
	if out, err := runKind(ctx, "load", "docker-image", image, "--name", c.Name); err != nil {
		return fmt.Errorf("kindcluster: load %s: %w\n%s", image, err, out)
	}
	return nil
}

// Kubectl runs kubectl against this cluster and only this cluster.
func (c *Cluster) Kubectl(ctx context.Context, args ...string) (string, error) {
	return c.run(ctx, "kubectl", nil, append([]string{"--context", c.Context}, args...)...)
}

// Helm runs helm against this cluster and only this cluster.
func (c *Cluster) Helm(ctx context.Context, args ...string) (string, error) {
	return c.run(ctx, "helm", nil, append([]string{"--kube-context", c.Context}, args...)...)
}

// Apply pipes a manifest to `kubectl apply -f -`.
func (c *Cluster) Apply(ctx context.Context, manifest string) (string, error) {
	return c.run(ctx, "kubectl", strings.NewReader(manifest),
		"--context", c.Context, "apply", "-f", "-")
}

// DeleteManifest removes a manifest's objects. Absent objects are not an
// error; fixtures are usually torn down with the whole cluster anyway, so this
// exists for tests that reuse one.
func (c *Cluster) DeleteManifest(ctx context.Context, manifest string) (string, error) {
	return c.run(ctx, "kubectl", strings.NewReader(manifest),
		"--context", c.Context, "delete", "--ignore-not-found", "-f", "-")
}

// run invokes a cluster-facing tool with the environment built from scratch.
// An ambient KUBECONFIG in the caller's environment would otherwise be merged
// with ours, putting every context on the machine back within reach of a
// single mistyped flag.
func (c *Cluster) run(ctx context.Context, tool string, stdin io.Reader, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.Env = []string{
		"KUBECONFIG=" + c.Kubeconfig,
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
	}
	cmd.Stdin = stdin
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%s %s: %w\n%s", tool, strings.Join(args, " "), err, out.String())
	}
	return out.String(), nil
}

// checkKindVersion refuses to build a cluster the installed kind cannot
// finish driving. It runs before `kind create` so the failure costs seconds
// rather than four minutes and a half-usable cluster.
//
// An unreadable version is a warning, not an error: a kind built from source
// reports something this cannot parse, and blocking that is worse than the
// problem. Only a version we can read *and* know is too old is fatal.
func checkKindVersion(ctx context.Context, out io.Writer) error {
	raw, err := runKind(ctx, "version")
	if err != nil {
		return fmt.Errorf("kindcluster: kind version: %w", err)
	}
	return judgeKindVersion(raw, out)
}

// judgeKindVersion is the decision checkKindVersion makes, split out from the
// exec so a test can drive it with the version strings that matter instead of
// whatever kind happens to be installed on the machine running the test.
func judgeKindVersion(raw string, out io.Writer) error {
	got, ok := parseKindVersion(raw)
	if !ok {
		if out != nil {
			fmt.Fprintf(out, "▸ warning: cannot parse kind version %q; wanted at least %s\n",
				strings.TrimSpace(raw), MinKindVersion)
		}
		return nil
	}
	want, _ := parseKindVersion(MinKindVersion)
	if compareVersions(got, want) < 0 {
		return fmt.Errorf("kindcluster: kind %s is too old (need %s or newer): "+
			"older kind cannot load images into a node running containerd v2, so "+
			"`kind load docker-image` fails after the cluster comes up. "+
			"Install with: go install sigs.k8s.io/kind@%s",
			formatVersion(got), MinKindVersion, MinKindVersion)
	}
	return nil
}

// parseKindVersion pulls the first vN.N.N out of `kind version` output, which
// looks like "kind v0.32.0 go1.24.0 linux/amd64". Pre-release suffixes are
// truncated at the patch number: v0.32.0-alpha compares equal to v0.32.0,
// which is the lenient direction.
func parseKindVersion(s string) ([3]int, bool) {
	for _, field := range strings.Fields(s) {
		if len(field) < 2 || field[0] != 'v' {
			continue
		}
		var v [3]int
		parts := strings.SplitN(field[1:], ".", 3)
		if len(parts) != 3 {
			continue
		}
		ok := true
		for i, p := range parts {
			// Truncate at the first non-digit so "0-alpha" reads as 0.
			end := 0
			for end < len(p) && p[end] >= '0' && p[end] <= '9' {
				end++
			}
			if end == 0 {
				ok = false
				break
			}
			n, err := strconv.Atoi(p[:end])
			if err != nil {
				ok = false
				break
			}
			v[i] = n
		}
		if ok {
			return v, true
		}
	}
	return [3]int{}, false
}

func compareVersions(a, b [3]int) int {
	for i := range a {
		switch {
		case a[i] < b[i]:
			return -1
		case a[i] > b[i]:
			return 1
		}
	}
	return 0
}

func formatVersion(v [3]int) string { return fmt.Sprintf("v%d.%d.%d", v[0], v[1], v[2]) }

func (c *Cluster) logf(format string, args ...any) {
	if c.out == nil {
		return
	}
	fmt.Fprintf(c.out, "▸ "+format+"\n", args...)
}

// verifyIsolation asserts the kubeconfig kind wrote names our cluster and
// nothing else.
//
// This is the check that makes the other guards redundant rather than
// load-bearing: if the file handed downstream contains exactly one context and
// that context is ours, then no bug — a wrong flag, a dropped --context, a
// regression in a driver — can reach another cluster, because no other cluster
// is described in the only file the child process can read.
func (c *Cluster) verifyIsolation() error {
	raw, err := os.ReadFile(c.Kubeconfig)
	if err != nil {
		return fmt.Errorf("kindcluster: read %s: %w", c.Kubeconfig, err)
	}
	return verifyKubeconfig(string(raw), c.Context, c.Kubeconfig)
}

// verifyKubeconfig is split out from verifyIsolation so the parsing rules can
// be tested without standing up a cluster.
func verifyKubeconfig(text, wantContext, path string) error {
	var current string
	for _, line := range strings.Split(text, "\n") {
		if rest, ok := strings.CutPrefix(line, "current-context:"); ok {
			current = strings.Trim(strings.TrimSpace(rest), `"'`)
			break
		}
	}
	if current != wantContext {
		return fmt.Errorf("kindcluster: %s has current-context %q, want %q", path, current, wantContext)
	}
	// Counted textually rather than by parsing: a context entry is
	// "- context:" under the contexts list, and any count above one means kind
	// merged into a file that already described another cluster.
	if n := strings.Count(text, "- context:"); n != 1 {
		return fmt.Errorf("kindcluster: %s describes %d contexts, want exactly 1 — "+
			"the e2e kubeconfig must not be a merged one", path, n)
	}
	return nil
}

// List returns the kind clusters on this machine, ours and otherwise.
func List(ctx context.Context) ([]string, error) {
	out, err := runKind(ctx, "get", "clusters")
	if err != nil {
		// kind exits non-zero with this on a clean machine.
		if strings.Contains(out, "No kind clusters found") {
			return nil, nil
		}
		return nil, fmt.Errorf("kindcluster: list clusters: %w\n%s", err, out)
	}
	return parseClusterList(out), nil
}

func parseClusterList(out string) []string {
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" && !strings.HasPrefix(s, "No kind clusters") {
			names = append(names, s)
		}
	}
	return names
}

// Reap deletes leftover clusters carrying NamePrefix. A run killed with
// SIGKILL leaves its cluster behind, and a stale one is both a resource leak
// and the thing Create's no-adopt rule will trip over on the next run.
func Reap(ctx context.Context) ([]string, error) {
	names, err := List(ctx)
	if err != nil {
		return nil, err
	}
	var reaped []string
	for _, n := range names {
		if !strings.HasPrefix(n, NamePrefix) {
			continue
		}
		if err := destroy(ctx, n); err != nil {
			return reaped, err
		}
		reaped = append(reaped, n)
	}
	return reaped, nil
}

// destroy is the only path to `kind delete`, and it re-checks the prefix.
// Delete and Reap both call it; centralizing the check means a future caller
// cannot forget it.
func destroy(ctx context.Context, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	if out, err := runKind(ctx, "delete", "cluster", "--name", name); err != nil {
		return fmt.Errorf("kindcluster: delete %s: %w\n%s", name, err, out)
	}
	return nil
}

func checkName(name string) error {
	if !strings.HasPrefix(name, NamePrefix) {
		return fmt.Errorf("kindcluster: cluster name %q does not start with %q — "+
			"this package only manages clusters it created", name, NamePrefix)
	}
	if strings.TrimPrefix(name, NamePrefix) == "" {
		return fmt.Errorf("kindcluster: cluster name %q is only the prefix", name)
	}
	return nil
}

// prepareKubeconfig returns the path to write credentials to, and whether
// Delete should remove it.
func prepareKubeconfig(cfg Config) (path string, owns bool, err error) {
	if cfg.Kubeconfig == "" {
		f, err := os.CreateTemp("", "simian-e2e-kubeconfig-*.yaml")
		if err != nil {
			return "", false, err
		}
		name := f.Name()
		if err := f.Close(); err != nil {
			return "", false, err
		}
		// kind writes the file itself; an empty one left here would merely be
		// overwritten, but removing it keeps "the file exists" meaning
		// "the cluster exists".
		if err := os.Remove(name); err != nil {
			return "", false, err
		}
		return name, true, nil
	}
	if _, err := os.Stat(cfg.Kubeconfig); err == nil {
		return "", false, fmt.Errorf("kindcluster: %s already exists — kind merges into an existing "+
			"kubeconfig, which would break the one-context isolation guarantee", cfg.Kubeconfig)
	} else if !os.IsNotExist(err) {
		return "", false, err
	}
	// kind will not create the parent itself, and the default path is a
	// directory (.kube/) that a fresh checkout does not have.
	if dir := filepath.Dir(cfg.Kubeconfig); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", false, err
		}
	}
	return cfg.Kubeconfig, false, nil
}

func runKind(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "kind", args...)
	// kind talks to the container runtime, not to a cluster, so it needs the
	// caller's environment (DOCKER_HOST and friends) — but never an inherited
	// KUBECONFIG, since --kubeconfig is what keeps the write isolated.
	cmd.Env = filterEnv(os.Environ(), "KUBECONFIG")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

func filterEnv(env []string, drop ...string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if slices.Contains(drop, key) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// durationArg renders a timeout the way kubectl --timeout wants it.
func durationArg(d time.Duration) string {
	if d <= 0 {
		return "300s"
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}
