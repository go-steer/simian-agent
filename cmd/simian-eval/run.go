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
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/go-steer/simian-agent/pkg/audit"
	"github.com/go-steer/simian-agent/pkg/eval"
	"github.com/go-steer/simian-agent/pkg/harness"
	"github.com/go-steer/simian-agent/pkg/harness/subject"
	"github.com/go-steer/simian-agent/pkg/scenario"
)

// runEval is the whole command: load, provision, run, write, score.
//
// out is where the scorecard goes; progress is where everything else goes.
// Split so that `simian-eval --format json > scorecard.json` produces a file
// jq can read, with the narration still on the terminal.
func runEval(ctx context.Context, o *options, out, progress io.Writer) error {
	if err := o.validate(); err != nil {
		return err
	}

	pack, err := loadPacks(o.packDirs)
	if err != nil {
		return err
	}
	// Resolved here rather than from the Runner below, so an --only typo and a
	// subject timeout that cannot fit are both refused before anything
	// connects to a cluster, let alone writes to one.
	selected, err := harness.Select(pack, o.only)
	if err != nil {
		return err
	}
	if !o.skipDurationOK {
		if err := checkFaultDurations(selected, o.subjectTimeout); err != nil {
			return err
		}
	}

	runID := newRunID()
	outDir := o.out
	if outDir == "" {
		outDir = filepath.Join("runs", runID)
	}

	// The output directory's *name* is resolved before the subject is parsed,
	// so the subject can be told where to leave its own evidence. A scorecard
	// says what a subject answered and never why, which is enough for a
	// detector and not enough for an agent: the interesting question about a
	// 0.00 is which tools it called.
	//
	// The directory itself is created after, because an unusable spec should
	// leave nothing behind — a runs/ full of empty directories from typos is
	// how a reader loses track of which runs happened.
	subj, err := subject.Parse(o.subject, subject.Options{
		Timeout:     o.subjectTimeout,
		Dir:         o.subjectDir,
		Env:         o.subjectEnv,
		ArtifactDir: outDir,
	})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("output directory: %w", err)
	}

	// Opened before the cluster is touched. Every audit event from here on is
	// evidence, and an event emitted while the log is still a plan is an event
	// nobody can score.
	auditPath := filepath.Join(outDir, harness.AuditFileName)
	auditFile, err := os.Create(auditPath)
	if err != nil {
		return fmt.Errorf("audit log: %w", err)
	}
	defer func() { _ = auditFile.Close() }()

	logger := slog.New(slog.NewTextHandler(progress, &slog.HandlerOptions{Level: slog.LevelInfo}))
	auditor := audit.New(slog.New(slog.NewJSONHandler(auditFile, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfg, releaseCluster, err := resolveCluster(ctx, o, runID, progress)
	if err != nil {
		return err
	}
	// A cluster the run stood up is deleted even on Ctrl-C. WithoutCancel
	// rather than the run's context: cleanup that gives up because the run was
	// cancelled is cleanup that leaves the cluster behind.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), kindProvisionTimeout)
		defer cancel()
		releaseCluster(cleanupCtx)
	}()

	p, err := buildPlane(cfg, o, auditor, logger)
	if err != nil {
		return err
	}
	serverVersion, err := checkReachable(p.k8s)
	if err != nil {
		return err
	}

	// The reaper is the backstop, not the mechanism: the runner clears every
	// fault it applied. This catches the case where the process is killed
	// hard enough that it does not, and the leases outlive their deadlines.
	reaperCtx, stopReaper := context.WithCancel(ctx)
	defer stopReaper()
	go p.reaper.Run(reaperCtx)

	runner := &harness.Runner{
		Pack:            pack,
		Subject:         subj,
		Arena:           newArena(o, p, runID, logger),
		Substrates:      p.substrates,
		Injector:        p.executor,
		Auditor:         auditor,
		Only:            o.only,
		Concurrency:     o.concurrency,
		RemediationPoll: o.remediationPoll,
		TeardownTimeout: o.teardownTimeout,
		Logger:          logger,
	}

	logger.Info("simian-eval: starting",
		slog.String("run", runID),
		slog.String("subject", subj.Name()),
		slog.String("kubernetes", serverVersion),
		slog.String("plan", harness.Describe(pack, selected)),
		slog.String("out", outDir))

	// A cancelled suite still returns the scenarios that finished: Run stops
	// starting new ones, and every scenario that got as far as the subject
	// holds evidence that cost cluster time to produce. Throwing it away
	// because the operator changed their mind is throwing away the run.
	runs, err := runner.Run(ctx)
	if err != nil {
		return err
	}

	runPath, err := harness.WriteRunFileTo(outDir, harness.RunFile(subj.Name(), runs))
	if err != nil {
		return err
	}
	logger.Info("simian-eval: artifacts written",
		slog.String("audit", auditPath),
		slog.String("run", runPath))

	if !o.score {
		return nil
	}

	// Scored from the files, not from the runs in memory. Going through the
	// artifacts is what proves they are scoreable: if `simian evaluate` could
	// not reproduce this number tomorrow, the run finds out now rather than
	// after the cluster is gone.
	if err := auditFile.Sync(); err != nil {
		return fmt.Errorf("audit log: %w", err)
	}
	return scoreArtifacts(o, pack, auditPath, runPath, out)
}

// scoreArtifacts reads the two files back and prints the scorecard.
func scoreArtifacts(o *options, pack scenario.Pack, auditPath, runPath string, out io.Writer) error {
	auditFile, err := os.Open(auditPath)
	if err != nil {
		return fmt.Errorf("open audit log: %w", err)
	}
	defer func() { _ = auditFile.Close() }()
	facts, err := eval.ReadAudit(auditFile)
	if err != nil {
		return err
	}

	reportFile, err := os.Open(runPath)
	if err != nil {
		return fmt.Errorf("open run file: %w", err)
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

	if o.format == "json" {
		err = summary.WriteJSON(out)
	} else {
		err = summary.WriteText(out)
	}
	if err != nil {
		return err
	}

	// Printed first, refused second: the rows that explain a low efficacy rate
	// are the ones an operator needs to see, and exiting before printing them
	// hides exactly the evidence.
	if summary.EfficacyRate < o.minEfficacy {
		return fmt.Errorf("efficacy rate %.2f is below --min-efficacy %.2f: %d of %d scenarios failed to inject, so this scorecard measures the harness and not the subject",
			summary.EfficacyRate, o.minEfficacy, summary.InjectFailures, summary.Scenarios)
	}
	return nil
}

// newArena builds the arena the run provisions its namespaces through. A
// function rather than a literal inline in run() so a test can read back what
// the flags bound to without standing up a cluster.
func newArena(o *options, p *plane, runID string, logger *slog.Logger) *harness.KubeArena {
	return &harness.KubeArena{
		Manager:     p.arenas,
		K8s:         p.k8s,
		Logger:      logger,
		Annotations: arenaAnnotations(runID),

		KeepArenas:      o.keepArenas,
		TerminatingWait: o.terminatingWait,
	}
}

// newRunID is a sortable, filesystem-safe, human-readable stamp. It names the
// output directory, the kind cluster and the arena annotation, so a run
// abandoned halfway can be traced from any one of them back to the other two.
func newRunID() string { return time.Now().UTC().Format("20060102-150405") }
