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
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/go-steer/simian-agent/pkg/eval"
	"github.com/go-steer/simian-agent/pkg/harness"
	"github.com/go-steer/simian-agent/pkg/scenario"
)

// reportOptions are `simian-eval report`'s inputs.
type reportOptions struct {
	runs        []string
	out         string
	packs       []string
	floor       []string
	floorSet    bool
	minEfficacy float64
	now         func() time.Time
}

func newReportCmd() *cobra.Command {
	o := &reportOptions{minEfficacy: eval.DefaultMinEfficacy, now: time.Now}
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Combine run directories into one scorecard document the web UI renders",
		Long: `Reads one or more run directories (each holding the audit.log and run.json a
simian-eval run wrote), scores each exactly as the run's own scorecard did, and
writes one self-describing JSON document: the scorecard grid, scenarios by
subjects, with every scenario's fault, ground truth and each subject's report.

Open it in any Simian web UI (Scorecard in the HUD, or ?scorecard=<url>), in
` + "`simian web`" + `, or by dropping the file on the page. It needs no server.

Each run is scored against the pack its audit log records (built-in packs by
name); pass --pack for a pack that is a directory. Runs of different packs may
be combined: the grid groups their scenarios by pack.

The floor is the deterministic detector, k8s-lookout: by default any run whose
subject is named lookout or k8s-lookout (with or without a version suffix, as
a lookout: subject is named after its binary). A subject scoring below the
floor on a scenario is flagged. --floor names the floor subjects instead;
--floor "" for none.

Examples:
  simian-eval report --run runs/dataplane-lookout --run runs/dataplane-agent --out scorecard.json
  simian-eval report --run runs/a --run runs/b --pack ./packs/custom > scorecard.json
  simian-eval report --run runs/a --run runs/b --floor my-detector`,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o.floorSet = cmd.Flags().Changed("floor")
			return runReport(o, cmd.OutOrStdout())
		},
	}
	f := cmd.Flags()
	f.StringArrayVar(&o.runs, "run", nil, "A run directory holding audit.log and run.json. Repeatable; each is a column of the grid (required).")
	f.StringVar(&o.out, "out", "", "File to write the scorecard document to (default: stdout)")
	f.StringSliceVar(&o.packs, "pack", nil, "Packs to score against, by built-in name or directory, matched to each run by the pack name its audit log records. Built-in packs need not be listed.")
	f.StringSliceVar(&o.floor, "floor", nil, "Subjects that are the floor (the deterministic detector). Default: subjects named lookout or k8s-lookout. --floor \"\" for none.")
	f.Float64Var(&o.minEfficacy, "min-efficacy", o.minEfficacy, "Efficacy rate below which a run is flagged as measuring the harness, not the subject")
	return cmd
}

func runReport(o *reportOptions, stdout io.Writer) error {
	if len(o.runs) == 0 {
		return errors.New("--run is required: name at least one run directory")
	}
	given := map[string]scenario.Pack{}
	for _, ref := range o.packs {
		p, err := scenario.LoadRef(ref)
		if err != nil {
			return err
		}
		given[p.Name] = p
	}

	inputs := make([]eval.ArchiveInput, 0, len(o.runs))
	for _, dir := range o.runs {
		in, err := readRunDir(dir, given)
		if err != nil {
			return err
		}
		inputs = append(inputs, in)
	}

	opts := eval.ArchiveOptions{
		MinEfficacy: o.minEfficacy,
		GeneratedAt: o.now(),
		Generator:   "simian-eval " + version,
	}
	if o.floorSet {
		opts.Floor = []string{}
		for _, s := range o.floor {
			if s = strings.TrimSpace(s); s != "" {
				opts.Floor = append(opts.Floor, s)
			}
		}
	}
	doc, err := eval.BuildArchive(inputs, opts)
	if err != nil {
		return err
	}

	if o.out == "" {
		return doc.WriteJSON(stdout)
	}
	var buf bytes.Buffer
	if err := doc.WriteJSON(&buf); err != nil {
		return err
	}
	if err := os.WriteFile(o.out, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write scorecard: %w", err)
	}
	return nil
}

// readRunDir reads a run directory's two artifacts and resolves its pack.
func readRunDir(dir string, given map[string]scenario.Pack) (eval.ArchiveInput, error) {
	auditBytes, err := os.ReadFile(filepath.Join(dir, harness.AuditFileName))
	if err != nil {
		return eval.ArchiveInput{}, fmt.Errorf("run %s: %w", dir, err)
	}
	runBytes, err := os.ReadFile(filepath.Join(dir, harness.RunFileName))
	if err != nil {
		return eval.ArchiveInput{}, fmt.Errorf("run %s: %w", dir, err)
	}
	meta, err := eval.ReadRunMeta(bytes.NewReader(auditBytes))
	if err != nil {
		return eval.ArchiveInput{}, fmt.Errorf("run %s: %w", dir, err)
	}
	pack, err := resolveRunPack(meta.Pack, given)
	if err != nil {
		return eval.ArchiveInput{}, fmt.Errorf("run %s: %w", dir, err)
	}

	var evidence []string
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			name := e.Name()
			if e.Type().IsRegular() && name != harness.AuditFileName && name != harness.RunFileName {
				evidence = append(evidence, name)
			}
		}
	}
	sort.Strings(evidence)

	return eval.ArchiveInput{
		Label:    filepath.Base(filepath.Clean(dir)),
		Source:   dir,
		Pack:     pack,
		Audit:    auditBytes,
		RunFile:  runBytes,
		Evidence: evidence,
	}, nil
}

// resolveRunPack finds the pack a run recorded. Several packs run as one
// suite record their names joined with "+", the way loadPacks names them.
func resolveRunPack(recorded string, given map[string]scenario.Pack) (scenario.Pack, error) {
	if recorded == "" {
		if len(given) == 1 {
			for _, p := range given {
				return p, nil
			}
		}
		return scenario.Pack{}, errors.New("its audit log records no pack (an older run?); pass exactly one --pack to score it against")
	}
	names := strings.Split(recorded, "+")
	packs := make([]scenario.Pack, 0, len(names))
	for _, name := range names {
		if p, ok := given[name]; ok {
			packs = append(packs, p)
			continue
		}
		p, err := scenario.LoadRef(name)
		if err != nil {
			return scenario.Pack{}, fmt.Errorf("pack %q, which its audit log records, is not built in; pass it with --pack: %w", name, err)
		}
		packs = append(packs, p)
	}
	if len(packs) == 1 {
		return packs[0], nil
	}
	merged := scenario.Pack{Name: recorded}
	for _, p := range packs {
		merged.Scenarios = append(merged.Scenarios, p.Scenarios...)
	}
	if err := merged.Validate(); err != nil {
		return scenario.Pack{}, fmt.Errorf("packs %s do not compose: %w", recorded, err)
	}
	return merged, nil
}
