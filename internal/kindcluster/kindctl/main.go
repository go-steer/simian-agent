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

// Command kindctl stands the local e2e cluster up and tears it down.
//
// It is a thin shell over internal/kindcluster so that `make cluster`, the
// e2e workflow, and the Go test harness all drive the same code. A parallel
// shell implementation would drift, and the first symptom of drift would be
// CI and a developer's laptop disagreeing about whether a fault landed.
//
// Not part of the shipped binaries — this lives under internal/ on purpose.
//
// Usage:
//
//	kindctl up [--name N] [--kubeconfig PATH] [--config FILE] [--image REF]
//	kindctl down [--name N] [--kubeconfig PATH]
//	kindctl reap
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/go-steer/simian-agent/internal/kindcluster"
)

// defaultNodeImage pins the Kubernetes version. Left one minor behind kind's
// newest default: Chaos Mesh and Calico lag the newest Kubernetes release, and
// an eval rig that cannot install its own fault injector is worse than one a
// version behind.
const defaultNodeImage = "kindest/node:v1.34.8" +
	"@sha256:02722c2dedddcfc00febf5d27fbeb9b7b2c14294c82109ff4a85d89ac9ba3256"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "kindctl: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: kindctl <up|down|reap> [flags]")
	}
	// Ctrl-C during `up` must still tear down the half-built cluster; the
	// teardown paths in kindcluster all use context.WithoutCancel for exactly
	// this, so cancelling here is safe.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := os.Args[1]
	fs := flag.NewFlagSet("kindctl "+cmd, flag.ExitOnError)
	name := fs.String("name", kindcluster.DefaultName, "kind cluster name")
	kubeconfig := fs.String("kubeconfig", defaultKubeconfig(), "kubeconfig path to write")

	switch cmd {
	case "up":
		configFile := fs.String("config", defaultClusterConfig(), "kind cluster config YAML")
		image := fs.String("image", defaultNodeImage, "node image")
		if err := fs.Parse(os.Args[2:]); err != nil {
			return err
		}
		return up(ctx, *name, *kubeconfig, *configFile, *image)

	case "down":
		if err := fs.Parse(os.Args[2:]); err != nil {
			return err
		}
		return down(ctx, *name, *kubeconfig)

	case "reap":
		if err := fs.Parse(os.Args[2:]); err != nil {
			return err
		}
		reaped, err := kindcluster.Reap(ctx)
		for _, n := range reaped {
			fmt.Printf("▸ reaped %s\n", n)
		}
		if len(reaped) == 0 && err == nil {
			fmt.Println("▸ nothing to reap")
		}
		return err

	default:
		return fmt.Errorf("unknown command %q (want up, down, or reap)", cmd)
	}
}

func up(ctx context.Context, name, kubeconfig, configFile, image string) error {
	c, err := kindcluster.Provision(ctx, kindcluster.Config{
		Name:       name,
		Kubeconfig: kubeconfig,
		ConfigFile: configFile,
		Image:      image,
		Out:        os.Stdout,
	})
	if err != nil {
		return err
	}
	fmt.Printf("\nCluster is up. Point tools at it with:\n\n  export KUBECONFIG=%s\n  kubectl --context %s get nodes\n\nTear it down with `make cluster-down`.\n",
		c.Kubeconfig, c.Context)
	return nil
}

func down(ctx context.Context, name, kubeconfig string) error {
	c := &kindcluster.Cluster{
		Name:       name,
		Context:    kindcluster.ContextPrefix + name,
		Kubeconfig: kubeconfig,
	}
	if err := c.Delete(ctx); err != nil {
		return err
	}
	// Delete only removes a kubeconfig it created itself; this one was passed
	// in, so clearing it is the caller's job and "down" is the caller.
	// RemoveKubeconfig verifies the file is solely this cluster's first —
	// `--kubeconfig ~/.kube/config` must not be a way to delete real credentials.
	if err := c.RemoveKubeconfig(); err != nil {
		return err
	}
	fmt.Printf("▸ deleted %s\n", name)
	return nil
}

// defaultKubeconfig keeps credentials inside the work tree instead of
// ~/.kube/config. The point is that the file naming this cluster can never
// also name a real one.
func defaultKubeconfig() string {
	if v := os.Getenv("SIMIAN_E2E_KUBECONFIG"); v != "" {
		return v
	}
	return filepath.Join(repoRoot(), ".kube", "e2e.yaml")
}

func defaultClusterConfig() string {
	return filepath.Join(repoRoot(), "dev", "kind", "cluster.yaml")
}

// repoRoot resolves paths relative to the module root so the make targets work
// from any subdirectory. Falls back to the working directory.
func repoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "."
		}
		dir = parent
	}
}
