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

// Command simian-eval drives a scenario pack against a subject on a real
// cluster and writes the two artifacts `simian evaluate` scores.
//
// It is a second binary on purpose. The operator binary runs in-cluster with
// chaos RBAC; a rig that provisions clusters, forks subject processes and
// grades them has no business linking into it. Keeping them apart also keeps
// the honest thing honest: simian-eval reaches the cluster only through the
// executor, which is the same path `simian chaos` uses, so the eval measures
// the product rather than a privileged shortcut built for the eval.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

var version = "0.1.0-dev"

func main() {
	if err := execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// execute exists so the signal handler is released before os.Exit, which
// never runs a deferred anything.
func execute() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return newRootCmd(defaultOptions()).ExecuteContext(ctx)
}

// newRootCmd takes the options it will fill rather than making them, so a test
// can parse an argv and read back what the flags actually bound to.
func newRootCmd(opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "simian-eval",
		Short: "Run a scenario pack against a subject and score what it found",
		Long: `Runs each scenario in a pack on a real cluster and grades the subject's answer.

Per scenario: provision an arena namespace, inject the faults through the
normal executor path, wait for their efficacy gates, hand the subject the
prompt, collect its report, then clear the chaos and put the namespace back.

Two artifacts land in --out. audit.log is Simian's side — which faults landed,
and when. run.json is the subject's side — what it said, and when. They join
on the scenario ID, and ` + "`simian evaluate`" + ` reads them back to produce exactly
the scorecard printed at the end of a run. Nothing about the score depends on
the cluster still existing.

A fault that does not manifest is an InjectError, reported apart from the
score and never charged to the subject: a cluster that was never broken has
nothing in it to find, and a zero there would read as a miss.

Examples:
  simian-eval --pack parity --subject exec:./bin/lookout --out runs/
  simian-eval --pack parity,lookout --subject exec:./bin/agent --concurrency 4
  simian-eval --pack ./packs/custom --subject noop: --cluster kind --out runs/floor
  simian-eval --pack parity --subject exec:./bin/agent --only parity-crash-loop`,
		Version:       version,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runEval(cmd.Context(), opts, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}

	bindFlags(cmd, opts)
	return cmd
}
