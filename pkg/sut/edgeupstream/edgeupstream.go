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

// Package edgeupstream is the smallest thing a dataplane fault can be aimed
// at: a caller, a callee, and a Service between them.
//
// # Why not Online Boutique
//
// Online Boutique is the SUT for anything that wants a realistic topology. It
// is the wrong one for a graded fixture. Twelve services take a couple of
// minutes to come up cold, some of them restart on their own on a small node
// pool, and every one of those restarts is a true observation a subject can
// report and be charged for inventing. A fixture's substrate should contribute
// nothing to the report except the thing the fault did to it.
//
// # Why two tiers and not one
//
// Because the interesting dataplane faults are about a *path*. "The upstream
// is slow" and "the upstream is saturated" are different diagnoses with the
// same symptom, and the symptom is only visible from the caller. One tier can
// be made slow; it takes two for slowness to be something a subject has to
// attribute.
//
// # What is deliberately absent
//
// No load generator. The efficacy probe drives the traffic it measures, which
// keeps the measurement and the load on the same clock — a background loadgen
// would put a second, unobserved traffic source in the namespace, and a
// scenario that fails its gate because the loadgen was between requests is a
// flake nobody can reproduce.
//
// No metrics stack either. Whether the subject can see CPU is a property of
// the subject's tools, not of the fixture, and a fixture that ships its own
// observability would be grading itself.
package edgeupstream

import (
	_ "embed"
	"time"

	"github.com/go-steer/simian-agent/pkg/sut"
)

// Name is the registry key, and the value a scenario's `substrate:` carries.
const Name = "edge-upstream"

// The workload names, exported because a scenario's expectations name them
// and a typo in a fixture should be a compile error somewhere.
const (
	// EdgeWorkload is the caller. Its readiness goes through the proxy, so
	// this is where a dataplane fault becomes visible to the API server.
	EdgeWorkload = "edge"

	// UpstreamWorkload is the callee, and the thing faults are aimed at.
	UpstreamWorkload = "upstream"
)

//go:embed manifests/edge-upstream.yaml
var manifestsYAML []byte

type edgeUpstream struct{}

func (e *edgeUpstream) Name() string { return Name }

func (e *edgeUpstream) Description() string {
	return "Two nginx tiers and a Service between them: the smallest substrate a dataplane fault can be aimed at"
}

func (e *edgeUpstream) Manifests() []byte {
	// A copy, so a caller cannot mutate the embedded buffer.
	out := make([]byte, len(manifestsYAML))
	copy(out, manifestsYAML)
	return out
}

func (e *edgeUpstream) ExpectedWorkloads() []sut.WorkloadRef {
	return []sut.WorkloadRef{
		{Kind: "Deployment", Name: UpstreamWorkload},
		{Kind: "Deployment", Name: EdgeWorkload},
	}
}

func (e *edgeUpstream) BaselineConfig() sut.BaselineConfig {
	cfg := sut.DefaultBaselineConfig()
	// Two nginx pods and an init container that waits on DNS. If this is not
	// up in two minutes the cluster has a problem the fixture cannot work
	// around, and a longer wait only delays finding out.
	cfg.ReadyTimeout = 2 * time.Minute
	return cfg
}

// Register adds the substrate to the package-level registry. Called for its
// side effect from the binary's main package.
func Register() { sut.Default.MustRegister(&edgeUpstream{}) }
