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
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/go-steer/simian-agent/pkg/arena"
)

func newArenaCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "arena",
		Short: "Manage chaos arenas (eligible namespaces + chaos-SA RoleBindings)",
		Long: `An "arena" is a Kubernetes namespace that is annotated as Simian-eligible
and bound to the chaos ServiceAccount. Faults can only be applied inside an
arena.

Use 'simian arena create' to mark an existing or new namespace as a chaos
target before deploying a SUT (with 'simian sut deploy', M2 Part B) or
applying faults (with 'simian chaos').`,
	}
	cmd.AddCommand(newArenaCreateCmd())
	cmd.AddCommand(newArenaDestroyCmd())
	cmd.AddCommand(newArenaDescribeCmd())
	return cmd
}

func newArenaCreateCmd() *cobra.Command {
	var (
		kubeconfig  string
		chaosSAName string
		chaosSANS   string
		annotations []string
		labelsArgs  []string
	)
	cmd := &cobra.Command{
		Use:   "create <namespace>",
		Short: "Create or update a chaos arena (namespace + chaos-SA RoleBinding)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()

			mgr, err := buildArenaManager(kubeconfig, chaosSAName, chaosSANS)
			if err != nil {
				return err
			}
			extraAnn, err := parseKVList(annotations)
			if err != nil {
				return fmt.Errorf("--annotation: %w", err)
			}
			extraLbl, err := parseKVList(labelsArgs)
			if err != nil {
				return fmt.Errorf("--label: %w", err)
			}
			spec := arena.Spec{
				Namespace:        args[0],
				ExtraAnnotations: extraAnn,
				ExtraLabels:      extraLbl,
			}
			if err := mgr.Create(ctx, spec); err != nil {
				return err
			}
			fmt.Printf("arena %q ready (chaos SA = %s/%s)\n", spec.Namespace, chaosSANS, chaosSAName)
			return nil
		},
	}
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig (default: in-cluster, then $KUBECONFIG, then ~/.kube/config)")
	cmd.Flags().StringVar(&chaosSAName, "chaos-sa", "simian-controller", "Chaos controller ServiceAccount name to bind in the arena")
	cmd.Flags().StringVar(&chaosSANS, "chaos-sa-namespace", "simian-system", "Namespace where the chaos controller SA lives")
	cmd.Flags().StringArrayVar(&annotations, "annotation", nil, "Extra namespace annotation key=value (repeatable; e.g. simian.chaos/exclude-workloads=loadgenerator)")
	cmd.Flags().StringArrayVar(&labelsArgs, "label", nil, "Extra namespace label key=value (repeatable)")
	return cmd
}

func newArenaDestroyCmd() *cobra.Command {
	var (
		kubeconfig  string
		chaosSAName string
		chaosSANS   string
		force       bool
	)
	cmd := &cobra.Command{
		Use:   "destroy <namespace>",
		Short: "Destroy a chaos arena (RoleBinding + namespace)",
		Long: `Destroys an arena by deleting its RoleBinding, Role, and namespace.

By default refuses to proceed if simian-managed chaos resources are still
active in the namespace. Pass --force to override (does NOT clear the chaos
resources first; just removes the namespace, which deletes everything in it).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 60*time.Second)
			defer cancel()

			mgr, err := buildArenaManager(kubeconfig, chaosSAName, chaosSANS)
			if err != nil {
				return err
			}
			if err := mgr.Destroy(ctx, args[0], force); err != nil {
				return err
			}
			fmt.Printf("arena %q destroyed\n", args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig")
	cmd.Flags().StringVar(&chaosSAName, "chaos-sa", "simian-controller", "Chaos controller ServiceAccount name")
	cmd.Flags().StringVar(&chaosSANS, "chaos-sa-namespace", "simian-system", "Namespace where the chaos controller SA lives")
	cmd.Flags().BoolVar(&force, "force", false, "Destroy even if simian-managed chaos resources are still active")
	return cmd
}

func newArenaDescribeCmd() *cobra.Command {
	var (
		kubeconfig  string
		chaosSAName string
		chaosSANS   string
	)
	cmd := &cobra.Command{
		Use:   "describe <namespace>",
		Short: "Show the current state of a chaos arena",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()

			mgr, err := buildArenaManager(kubeconfig, chaosSAName, chaosSANS)
			if err != nil {
				return err
			}
			st, err := mgr.Describe(ctx, args[0])
			if err != nil {
				return err
			}
			printArenaState(st)
			return nil
		},
	}
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig")
	cmd.Flags().StringVar(&chaosSAName, "chaos-sa", "simian-controller", "Chaos controller ServiceAccount name")
	cmd.Flags().StringVar(&chaosSANS, "chaos-sa-namespace", "simian-system", "Namespace where the chaos controller SA lives")
	return cmd
}

func buildArenaManager(kubeconfig, saName, saNS string) (*arena.Manager, error) {
	cfg, err := buildKubeConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}
	m := arena.New(clientset, saName, saNS)
	m.Dyn = dyn
	return m, nil
}

func parseKVList(in []string) (map[string]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(in))
	for _, kv := range in {
		idx := strings.IndexRune(kv, '=')
		if idx < 1 {
			return nil, fmt.Errorf("expected key=value, got %q", kv)
		}
		out[kv[:idx]] = kv[idx+1:]
	}
	return out, nil
}

func printArenaState(st arena.State) {
	fmt.Printf("namespace:       %s\n", st.Namespace)
	if !st.Exists {
		fmt.Println("status:          missing")
		return
	}
	fmt.Printf("status:          present\n")
	fmt.Printf("eligible:        %t\n", st.Eligible)
	fmt.Printf("rolebinding:     %t\n", st.RoleBindingExists)
	fmt.Printf("chaos-sa-bound:  %t\n", st.ChaosSubjectBound)
	if len(st.ExcludedWorkloads) > 0 {
		fmt.Printf("excluded:        %s\n", strings.Join(st.ExcludedWorkloads, ", "))
	}
	if st.SimianFaultCount > 0 {
		fmt.Printf("active-faults:   %d\n", st.SimianFaultCount)
	}
	if len(st.Annotations) > 0 {
		keys := make([]string, 0, len(st.Annotations))
		for k := range st.Annotations {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Println("annotations:")
		for _, k := range keys {
			fmt.Printf("  %s=%s\n", k, st.Annotations[k])
		}
	}
}
