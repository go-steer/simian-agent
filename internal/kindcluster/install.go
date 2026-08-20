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
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Pinned versions. Bump deliberately: a CNI or Chaos Mesh upgrade can change
// whether a fault manifests at all, which would move eval scores for reasons
// that have nothing to do with the subject under test.
const (
	// CalicoVersion is the Calico release installed as the cluster CNI.
	// See dev/kind/cluster.yaml for why the default CNI is not used.
	CalicoVersion = "v3.32.1"

	// ChaosMeshVersion is the Chaos Mesh chart/app version installed.
	ChaosMeshVersion = "2.8.4"

	// ChaosMeshNamespace is where Chaos Mesh runs. Chaos Mesh's own
	// components must never be an eligible chaos target, so this namespace
	// carries no simian.chaos/eligible annotation.
	ChaosMeshNamespace = "chaos-mesh"
)

// calicoManifest is fetched by URL rather than vendored. The version is
// pinned, so the content is stable; vendoring ~250KB of third-party YAML into
// this repo would need its own license accounting for no benefit.
const calicoManifest = "https://raw.githubusercontent.com/projectcalico/calico/" +
	CalicoVersion + "/manifests/calico.yaml"

// chaosMeshChart is the packaged chart URL. Installing from a URL rather than
// `helm repo add chaos-mesh ...` keeps this from mutating the developer's helm
// repository list as a side effect of running the tests.
const chaosMeshChart = "https://charts.chaos-mesh.org/chaos-mesh-" + ChaosMeshVersion + ".tgz"

// InstallCNI installs Calico and waits for it to converge.
//
// The cluster config sets disableDefaultCNI, so nodes stay NotReady until this
// runs. See dev/kind/cluster.yaml for why kindnet is not good enough.
func (c *Cluster) InstallCNI(ctx context.Context) error {
	c.logf("installing Calico %s (the CNI that actually enforces NetworkPolicy)", CalicoVersion)
	if _, err := c.Kubectl(ctx, "apply", "-f", calicoManifest); err != nil {
		return fmt.Errorf("kindcluster: install calico: %w", err)
	}
	// Rollout status on the DaemonSet, not `wait --for=condition=Ready`: the
	// DaemonSet does not exist for a moment after apply, and `wait` fails
	// outright on a missing object where `rollout status` retries.
	c.logf("waiting for calico-node to roll out")
	if _, err := c.Kubectl(ctx, "-n", "kube-system", "rollout", "status",
		"ds/calico-node", "--timeout=300s"); err != nil {
		return fmt.Errorf("kindcluster: calico rollout: %w", err)
	}
	return nil
}

// InstallChaosMesh installs Chaos Mesh via helm and waits for it to be ready.
//
// Requires helm on PATH. The runtime settings are the part that matters and
// the part that is easy to get wrong: kind nodes run containerd, not docker,
// and chaos-daemon silently fails to inject anything if it is pointed at the
// wrong socket — faults appear to apply and then do nothing, which is the one
// failure mode an eval rig must never have.
func (c *Cluster) InstallChaosMesh(ctx context.Context) error {
	if _, err := exec.LookPath("helm"); err != nil {
		return fmt.Errorf("kindcluster: helm is not on PATH: %w", err)
	}
	c.logf("installing Chaos Mesh %s", ChaosMeshVersion)
	_, err := c.Helm(ctx, "upgrade", "--install", "chaos-mesh", chaosMeshChart,
		"--namespace", ChaosMeshNamespace,
		"--create-namespace",
		"--set", "chaosDaemon.runtime=containerd",
		"--set", "chaosDaemon.socketPath=/run/containerd/containerd.sock",
		"--wait",
		"--timeout", "10m",
	)
	if err != nil {
		return fmt.Errorf("kindcluster: install chaos-mesh: %w", err)
	}
	return nil
}

// VerifyChaosMesh asserts Chaos Mesh is actually usable: the CRDs Simian's
// driver applies are registered, and chaos-daemon is running on every node.
//
// Installing and being able to inject are different things, and the gap
// between them is exactly where a silently-inert eval rig lives.
func (c *Cluster) VerifyChaosMesh(ctx context.Context) error {
	for _, crd := range []string{
		"networkchaos.chaos-mesh.org",
		"podchaos.chaos-mesh.org",
		"stresschaos.chaos-mesh.org",
		"iochaos.chaos-mesh.org",
	} {
		if _, err := c.Kubectl(ctx, "get", "crd", crd); err != nil {
			return fmt.Errorf("kindcluster: chaos-mesh CRD %s not registered: %w", crd, err)
		}
	}
	if _, err := c.Kubectl(ctx, "-n", ChaosMeshNamespace, "rollout", "status",
		"ds/chaos-daemon", "--timeout=300s"); err != nil {
		return fmt.Errorf("kindcluster: chaos-daemon not ready: %w", err)
	}
	return nil
}

// Nodes returns the cluster's node names.
func (c *Cluster) Nodes(ctx context.Context) ([]string, error) {
	out, err := c.Kubectl(ctx, "get", "nodes", "-o", "name")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(strings.TrimPrefix(line, "node/")); s != "" {
			names = append(names, s)
		}
	}
	return names, nil
}

// Provision is the whole standing-up sequence: create, CNI, nodes Ready,
// Chaos Mesh, verify. It is what `make cluster` and the e2e workflow run, so
// that a developer's cluster and CI's cluster are the same cluster.
func Provision(ctx context.Context, cfg Config) (*Cluster, error) {
	c, err := Create(ctx, cfg)
	if err != nil {
		return nil, err
	}
	// Anything past Create that fails leaves a cluster with no CNI or no
	// chaos-daemon. That is worse than no cluster: it looks usable and
	// silently swallows faults.
	fail := func(err error) (*Cluster, error) {
		delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
		defer cancel()
		_ = c.Delete(delCtx)
		return nil, err
	}
	if err := c.InstallCNI(ctx); err != nil {
		return fail(err)
	}
	if err := c.WaitForNodes(ctx, 5*time.Minute); err != nil {
		return fail(err)
	}
	if err := c.InstallChaosMesh(ctx); err != nil {
		return fail(err)
	}
	if err := c.VerifyChaosMesh(ctx); err != nil {
		return fail(err)
	}
	c.logf("cluster %s ready (kubeconfig: %s)", c.Name, c.Kubeconfig)
	return c, nil
}
