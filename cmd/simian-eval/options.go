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
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/go-steer/simian-agent/pkg/eval"
	"github.com/go-steer/simian-agent/pkg/harness"
	"github.com/go-steer/simian-agent/pkg/scenario"
)

// Cluster modes for --cluster.
const (
	// ClusterCurrent uses the kubeconfig's current context and leaves the
	// cluster standing. This is the default because the cluster people
	// actually want to point this at is one they already have.
	ClusterCurrent = "current"

	// ClusterKind provisions a throwaway kind cluster for the run and deletes
	// it afterwards.
	ClusterKind = "kind"
)

// options is every knob, kept in one struct so the whole command can be run
// from a test without a process or a flag parse.
type options struct {
	packDirs []string
	subject  string
	only     []string
	out      string

	kubeconfig string
	cluster    string
	kindImage  string
	kindConfig string

	concurrency     int
	subjectTimeout  time.Duration
	subjectDir      string
	subjectEnv      []string
	remediationPoll time.Duration
	teardownTimeout time.Duration

	eligibleNS      []string
	chaosSA         string
	chaosSANS       string
	durationCap     time.Duration
	defaultProbes   bool
	reapInterval    time.Duration
	keepArenas      bool
	terminatingWait time.Duration
	skipDurationOK  bool

	format      string
	minEfficacy float64
	score       bool
}

func defaultOptions() *options {
	return &options{
		cluster:         ClusterCurrent,
		concurrency:     harness.DefaultConcurrency,
		subjectTimeout:  harness.DefaultSubjectTimeout,
		remediationPoll: harness.DefaultRemediationPoll,
		teardownTimeout: harness.DefaultTeardownTimeout,
		chaosSA:         "simian-controller",
		chaosSANS:       "simian-system",
		defaultProbes:   true,
		reapInterval:    30 * time.Second,
		terminatingWait: harness.DefaultTerminatingWait,
		format:          "text",
		minEfficacy:     eval.DefaultMinEfficacy,
		score:           true,
	}
}

func bindFlags(cmd *cobra.Command, o *options) {
	f := cmd.Flags()

	// The built-in names are spliced in rather than spelled out, so a new pack
	// cannot ship with help text that does not mention it.
	f.StringSliceVar(&o.packDirs, "pack", nil, "Scenario pack holding the ground truth: a built-in name ("+
		strings.Join(scenario.BuiltinPacks, ", ")+") or a directory. Repeatable or comma-separated; several packs run as one suite (required).")
	f.StringVar(&o.subject, "subject", "", "What to grade: exec:<command line>, lookout:<path to the lookout binary>, or noop: for the zero-score floor (required)")
	f.StringSliceVar(&o.only, "only", nil, "Run only these scenario IDs. Repeatable or comma-separated. An ID that is not in the pack is an error, not an empty run.")
	f.StringVar(&o.out, "out", "", "Directory to write audit.log and run.json into (default: a timestamped directory under ./runs)")

	f.StringVar(&o.kubeconfig, "kubeconfig", "", "Path to kubeconfig (default: in-cluster, then $KUBECONFIG, then ~/.kube/config)")
	f.StringVar(&o.cluster, "cluster", o.cluster, "current|kind. current runs against the kubeconfig's cluster and leaves it standing; kind provisions a throwaway cluster for the run and deletes it afterwards.")
	f.StringVar(&o.kindImage, "kind-image", "", "Node image for --cluster kind (default: whatever the installed kind uses)")
	f.StringVar(&o.kindConfig, "kind-config", "", "kind cluster config YAML for --cluster kind (see dev/kind/cluster.yaml)")

	f.IntVar(&o.concurrency, "concurrency", o.concurrency, "How many scenarios may be in flight. A ceiling, not a target: scenarios sharing a namespace are serialised regardless, and a control with no namespace takes the cluster to itself.")
	f.DurationVar(&o.subjectTimeout, "subject-timeout", o.subjectTimeout, "How long one investigation may take before the subject is killed and scored as a failure")
	f.StringVar(&o.subjectDir, "subject-dir", "", "Working directory for an exec: subject")
	f.StringSliceVar(&o.subjectEnv, "subject-env", nil, "Extra KEY=VALUE environment for an exec: subject. Repeatable.")
	f.DurationVar(&o.remediationPoll, "remediation-poll", o.remediationPoll, "How often to ask the cluster whether the fault is gone while the subject works, for time-to-remediate. 0 disables the watch.")
	f.DurationVar(&o.teardownTimeout, "teardown-timeout", o.teardownTimeout, "Bound on cleanup, which runs even after Ctrl-C")

	f.StringSliceVar(&o.eligibleNS, "eligible-namespace", nil, "Treat these namespaces as chaos-eligible instead of reading the simian.chaos/eligible annotation. Repeatable.")
	f.StringVar(&o.chaosSA, "chaos-sa", o.chaosSA, "ServiceAccount the arena RoleBinding grants chaos rights to")
	f.StringVar(&o.chaosSANS, "chaos-sa-namespace", o.chaosSANS, "Namespace of --chaos-sa")
	f.DurationVar(&o.durationCap, "duration-ceiling", 0, "Override the executor's fault duration ceiling (default 15m)")
	f.BoolVar(&o.defaultProbes, "default-efficacy-probes", o.defaultProbes, "Attach Simian's built-in efficacy probes to fault kinds that have one. Turning this off means a fault the cluster accepts but silently drops is scored as having landed.")
	f.DurationVar(&o.reapInterval, "reap-interval", o.reapInterval, "Lease reaper sweep interval. The reaper is the backstop that takes chaos out of a cluster this process failed to clean up.")
	f.DurationVar(&o.terminatingWait, "terminating-wait", o.terminatingWait, "How long to wait for a namespace left over from an earlier run to finish deleting before giving up on it. Namespace deletion is asynchronous, and running the same pack twice in a row would otherwise fail on the first run's teardown.")
	f.BoolVar(&o.keepArenas, "keep-arenas", false, "Leave arena namespaces standing after the run, for poking at a scenario that went wrong. Faults are still cleared.")
	f.BoolVar(&o.skipDurationOK, "allow-short-faults", false, "Permit faults whose duration is shorter than --subject-timeout. Off by default: a lease that expires mid-investigation is cleared by the reaper and looks exactly like the subject having remediated it.")

	f.StringVar(&o.format, "format", o.format, "text|json scorecard")
	f.Float64Var(&o.minEfficacy, "min-efficacy", o.minEfficacy, "Exit non-zero below this fraction of scenarios manifesting; 0 to always report")
	f.BoolVar(&o.score, "score", o.score, "Score the run from its own artifacts and print the scorecard. Off writes the artifacts and stops.")
}

func (o *options) validate() error {
	switch {
	case len(o.packDirs) == 0:
		return fmt.Errorf("--pack is required: the scenarios are the ground truth, and there is nothing to grade against without them (built-in packs: %s)",
			strings.Join(scenario.BuiltinPacks, ", "))
	case o.subject == "":
		return fmt.Errorf("--subject is required: name what to grade, e.g. exec:./bin/agent, or noop: for the zero-score floor")
	case o.format != "text" && o.format != "json":
		return fmt.Errorf("--format %q: want text or json", o.format)
	case o.cluster != ClusterCurrent && o.cluster != ClusterKind:
		return fmt.Errorf("--cluster %q: want %s or %s", o.cluster, ClusterCurrent, ClusterKind)
	case o.concurrency < 0:
		return fmt.Errorf("--concurrency %d: must not be negative", o.concurrency)
	case o.terminatingWait <= 0:
		// Not silently defaulted: a zero here would read as "do not wait",
		// and the field it feeds treats zero as "wait the default", so the
		// invocation would do the opposite of what it says.
		return fmt.Errorf("--terminating-wait %s: must be positive; pass something short to effectively not wait", o.terminatingWait)
	}
	return nil
}

// EvalRunAnnotation marks every arena a run touches with the run's own ID.
// The eligibility annotation says chaos is permitted here; this one says who
// permitted it, which is what tells an arena abandoned by a run that died from
// one somebody is still using.
const EvalRunAnnotation = "simian.chaos/eval-run"

func arenaAnnotations(runID string) map[string]string {
	return map[string]string{EvalRunAnnotation: runID}
}
