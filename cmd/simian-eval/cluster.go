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

package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/go-steer/simian-agent/internal/kindcluster"
)

// kindProvisionTimeout bounds standing a throwaway cluster up. Provision pulls
// a node image, installs a CNI and installs Chaos Mesh; on a cold machine that
// is minutes, and a bound short enough to cut it off produces a half-built
// cluster that silently swallows faults.
const kindProvisionTimeout = 15 * time.Minute

// resolveCluster produces the REST config the run talks to, and the function
// that puts the cluster back.
//
// The returned cleanup always runs, including after Ctrl-C, and takes its own
// context — a run cancelled while a kind cluster is up must still delete the
// cluster, or the next run finds a name collision and a few gigabytes of
// containers.
func resolveCluster(ctx context.Context, o *options, runID string, progress io.Writer) (cfg *rest.Config, cleanup func(context.Context), err error) {
	switch o.cluster {
	case ClusterKind:
		return provisionKind(ctx, o, runID, progress)
	default:
		cfg, err := buildKubeConfig(o.kubeconfig)
		if err != nil {
			return nil, nil, fmt.Errorf("kubeconfig: %w", err)
		}
		return cfg, func(context.Context) {}, nil
	}
}

func provisionKind(ctx context.Context, o *options, runID string, progress io.Writer) (*rest.Config, func(context.Context), error) {
	provisionCtx, cancel := context.WithTimeout(ctx, kindProvisionTimeout)
	defer cancel()

	c, err := kindcluster.Provision(provisionCtx, kindcluster.Config{
		Name:       kindcluster.NamePrefix + "eval-" + runID,
		Image:      o.kindImage,
		ConfigFile: o.kindConfig,
		Out:        progress,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("kind cluster: %w", err)
	}

	cleanup := func(ctx context.Context) {
		if err := c.Delete(ctx); err != nil {
			fmt.Fprintf(progress, "simian-eval: deleting kind cluster %s: %v\n", c.Name, err)
		}
	}

	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: c.Kubeconfig},
		&clientcmd.ConfigOverrides{CurrentContext: c.Context},
	).ClientConfig()
	if err != nil {
		cleanup(context.WithoutCancel(ctx))
		return nil, nil, fmt.Errorf("kind kubeconfig: %w", err)
	}
	return cfg, cleanup, nil
}

// buildKubeConfig mirrors the controller's resolution order, so a kubeconfig
// that works for `simian serve` works here.
func buildKubeConfig(path string) (*rest.Config, error) {
	if path == "" {
		if cfg, err := rest.InClusterConfig(); err == nil {
			return cfg, nil
		}
	}
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if path != "" {
		loadingRules.ExplicitPath = path
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{}).ClientConfig()
}
