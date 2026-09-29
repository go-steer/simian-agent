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

package planner

import (
	"strings"
	"testing"
	"time"

	"github.com/go-steer/simian-agent/pkg/executor"
	"github.com/go-steer/simian-agent/pkg/simian"
)

func refusal(kind, name, msg string, at time.Time) executor.RefusedFault {
	return executor.RefusedFault{
		Manifest: simian.FaultManifest{
			ResourceKind: kind,
			Targets:      []simian.TargetRef{{Namespace: "bank", Name: name}},
		},
		RefusedAt: at,
		Stage:     simian.StageSchema,
		Reason:    simian.ReasonSchemaInvalid,
		Error:     msg,
	}
}

// The trial's 220 identical HTTPChaos proposals: the planner has to be told
// why, once, with a count — not 220 lines, and not nothing.
func TestThePlannerIsToldWhatWasRefusedAndWhy(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	in := sampleInput()
	in.RecentRefusals = []executor.RefusedFault{
		refusal("HTTPChaos", "frontend", "spec.port: required", now),
		refusal("HTTPChaos", "frontend", "spec.port: required", now.Add(-time.Minute)),
		refusal("DNSChaos", "userservice", strings.Repeat("x", 400), now.Add(-2*time.Minute)),
	}

	prompt := buildPlanUserPrompt(in)
	if !strings.Contains(prompt, "## Recently refused") {
		t.Fatalf("no refusal section in prompt:\n%s", prompt)
	}
	want := "HTTPChaos on bank/frontend: schema-invalid: spec.port: required (x2, last 2026-09-29T12:00:00Z)"
	if !strings.Contains(prompt, want) {
		t.Errorf("prompt does not contain %q:\n%s", want, prompt)
	}
	if strings.Count(prompt, "spec.port: required") != 1 {
		t.Errorf("identical refusals were not folded into one line:\n%s", prompt)
	}
	if strings.Contains(prompt, strings.Repeat("x", refusalErrorLimit+1)) {
		t.Error("a long error reached the prompt untrimmed")
	}
	if strings.Index(prompt, "HTTPChaos on") > strings.Index(prompt, "DNSChaos on") {
		t.Error("refusals are not newest first")
	}
}

func TestNoRefusalsNoSection(t *testing.T) {
	if strings.Contains(buildPlanUserPrompt(sampleInput()), "Recently refused") {
		t.Error("refusal section rendered with nothing in it")
	}
}
