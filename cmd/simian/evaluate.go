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
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/go-steer/simian-agent/pkg/eval"
	"github.com/go-steer/simian-agent/pkg/scenario"
)

// evaluateOptions is the command's inputs, kept separate from the Cobra
// plumbing so the whole thing can be run in a test without a process.
type evaluateOptions struct {
	packRef     string
	auditPath   string
	reportPath  string
	format      string
	minEfficacy float64
}

func newEvaluateCmd() *cobra.Command {
	opts := evaluateOptions{format: "text", minEfficacy: eval.DefaultMinEfficacy}

	cmd := &cobra.Command{
		Use:   "evaluate",
		Short: "Score a finished run offline from its audit log and the subject's report",
		Long: `Reads an audit log and a subject report artifact and emits the scorecard.

No cluster is contacted and nothing is executed. Scoring is pure, so this
produces the same numbers the live harness produced for the same run — hours
later, on a different machine, from the artifacts alone. That is what makes a
scorecard usable somewhere like kube-agent-demo-e2e, where the clusters are
long-lived and nobody is going to stand up a kind harness to re-run anything.

The two artifacts join on the scenario ID that pkg/audit stamps onto every
event. The audit log is Simian's record of breaking things — which faults
landed, and when. The report is the subject's side — what it found, and when.

A scenario whose fault has no passing efficacy record is reported as NOT
SCORED rather than as a miss: the cluster was never broken, so a zero would
mean "nothing to find" while reading as "the agent missed it". If enough of
them pile up, the run is a harness failure and the command says so and exits
non-zero, because a scorecard from a suite that did not manifest is a
confident number about nothing.

Examples:
  simian evaluate --pack parity --audit run.log --report agent.json
  simian evaluate --pack ./packs/custom --audit run.log --report agent.json --format json
  simian evaluate --pack lookout --audit run.log --report agent.json --min-efficacy 1.0`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runEvaluate(opts, cmd.OutOrStdout())
		},
	}

	cmd.Flags().StringVar(&opts.packRef, "pack", "", "built-in pack name (parity, lookout) or a directory of scenario files, holding the ground truth (required)")
	cmd.Flags().StringVar(&opts.auditPath, "audit", "", "JSON audit log from the run, or - for stdin (required)")
	cmd.Flags().StringVar(&opts.reportPath, "report", "", "subject report artifact (required)")
	cmd.Flags().StringVar(&opts.format, "format", opts.format, "text|json")
	cmd.Flags().Float64Var(&opts.minEfficacy, "min-efficacy", opts.minEfficacy,
		"refuse to report below this fraction of scenarios manifesting; 0 to always report")

	return cmd
}

func runEvaluate(opts evaluateOptions, out io.Writer) error {
	switch {
	case opts.packRef == "":
		return fmt.Errorf("--pack is required: the scenarios are the ground truth, and there is nothing to score against without them")
	case opts.auditPath == "":
		return fmt.Errorf("--audit is required")
	case opts.reportPath == "":
		return fmt.Errorf("--report is required")
	}
	if opts.format != "text" && opts.format != "json" {
		return fmt.Errorf("--format %q: want text or json", opts.format)
	}

	pack, err := scenario.LoadRef(opts.packRef)
	if err != nil {
		return err
	}

	facts, err := readAudit(opts.auditPath)
	if err != nil {
		return err
	}

	reportFile, err := os.Open(opts.reportPath)
	if err != nil {
		return fmt.Errorf("open report: %w", err)
	}
	defer func() { _ = reportFile.Close() }()
	runFile, err := eval.ReadRunFile(reportFile)
	if err != nil {
		return err
	}

	runs, err := eval.Join(pack, facts, runFile)
	if err != nil {
		return err
	}

	summary, err := eval.Summarize(runFile.Subject, pack, runs)
	if err != nil {
		return err
	}

	if opts.format == "json" {
		if err := summary.WriteJSON(out); err != nil {
			return err
		}
	} else if err := summary.WriteText(out); err != nil {
		return err
	}

	// Reported first, refused second. An operator debugging a broken rig
	// needs to see which scenarios did not manifest, and exiting before
	// printing would hide exactly the rows that explain why.
	if summary.EfficacyRate < opts.minEfficacy {
		return fmt.Errorf("efficacy rate %.2f is below --min-efficacy %.2f: %d of %d scenarios failed to inject, so this scorecard measures the harness and not the subject",
			summary.EfficacyRate, opts.minEfficacy, summary.InjectFailures, summary.Scenarios)
	}
	return nil
}

// readAudit reads the audit log, from stdin when the path is "-".
func readAudit(path string) ([]eval.ScenarioFacts, error) {
	if path == "-" {
		return eval.ReadAudit(os.Stdin)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open audit log: %w", err)
	}
	defer func() { _ = f.Close() }()
	return eval.ReadAudit(f)
}
