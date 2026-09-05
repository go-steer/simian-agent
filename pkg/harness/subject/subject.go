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

// Package subject adapts a thing that can be asked about a cluster into
// eval.Subject.
//
// Simian does not import the subject. Not mast, not core-agent, not
// core-sre-agent, in either direction. An adversary that shares a prompt
// template, a model client or a Kubernetes client version with the subject can
// fail in a correlated way and produce an eval that passes for the wrong
// reason. Everything here talks to the subject the way an operator would —
// across a process boundary, over a stable text interface — so the only thing
// the two sides agree on is the shape of the answer.
package subject

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-steer/simian-agent/pkg/eval"
	"github.com/go-steer/simian-agent/pkg/scenario"
)

// Options are the settings a spec cannot carry.
type Options struct {
	// Timeout bounds one investigation. Zero means no bound, which is almost
	// never what an unattended suite wants.
	Timeout time.Duration

	// Dir is the working directory for exec: subjects. Empty inherits.
	Dir string

	// Env is extra environment for exec: subjects, in KEY=VALUE form,
	// appended to the parent's.
	Env []string
}

// Parse turns a --subject spec into a Subject.
//
//	exec:<command line>  run a binary and read a JSON report on its stdout
//	noop:                report nothing at all
//
// The scheme is required rather than inferred. `http:` and `mcp:` subjects are
// coming, and a bare path that silently means one of them today would mean
// something else tomorrow.
func Parse(spec string, opts Options) (eval.Subject, error) {
	scheme, rest, ok := strings.Cut(spec, ":")
	if !ok {
		return nil, fmt.Errorf("subject %q has no scheme; want exec:<command line>, or noop: for the null subject", spec)
	}

	switch scheme {
	case "exec":
		argv, err := splitArgs(rest)
		if err != nil {
			return nil, fmt.Errorf("subject %q: %w", spec, err)
		}
		if len(argv) == 0 {
			return nil, fmt.Errorf("subject %q names no command", spec)
		}
		return &Exec{Argv: argv, Timeout: opts.Timeout, Dir: opts.Dir, Env: opts.Env}, nil

	case "noop":
		if rest != "" {
			return nil, fmt.Errorf("subject %q: noop takes no argument", spec)
		}
		return Noop{}, nil

	case "http", "mcp":
		return nil, fmt.Errorf("subject %q: the %s adapter is not implemented yet; use an exec: subject", spec, scheme)

	default:
		return nil, fmt.Errorf("subject %q: unknown scheme %q; want an exec: or noop: subject", spec, scheme)
	}
}

// Noop reports nothing.
//
// It is the floor of every measure, and that is what makes it useful: a suite
// where the noop subject does not score zero recall is a suite measuring
// something other than the subject. It is also the only subject that needs no
// binary, so it is what a rig is smoke-tested with.
type Noop struct{}

// Name implements eval.Subject.
func (Noop) Name() string { return "noop" }

// Investigate implements eval.Subject. The report is empty and non-nil: the
// subject answered, and what it said was "nothing is wrong". That is a real
// answer, wrong on every scenario and right on every control, and it is not
// the same as failing to answer.
func (Noop) Investigate(_ context.Context, _ string) (eval.Report, error) {
	return eval.Report{Findings: []scenario.Finding{}}, nil
}
