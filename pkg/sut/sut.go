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

// Package sut owns the System-Under-Test half of the M2 provisioner work:
// applying SUT manifests into an arena, waiting for steady-state, capturing
// a baseline snapshot, and tearing the SUT back down.
//
// SUT definitions are pluggable via the Registry. Online Boutique is the
// built-in default; future SUTs (e.g. Bank of Anthos, a synthetic
// load-and-database stack) plug in as additional Registry entries.
package sut

import (
	"time"
)

// SUT describes a built-in System Under Test that Simian can deploy into an
// arena and verify.
type SUT interface {
	// Name is the registry key (e.g. "online-boutique").
	Name() string
	// Description is a human-readable one-liner shown in CLI listings.
	Description() string
	// Manifests returns the multi-document YAML to apply. Documents must be
	// separated by "---". The target namespace is injected per-document at
	// apply time.
	Manifests() []byte
	// ExpectedWorkloads is the set of (kind, name) pairs whose Ready status
	// EstablishBaseline waits on. Workloads not in this set are still applied
	// but do not gate baseline.
	ExpectedWorkloads() []WorkloadRef
	// BaselineConfig returns the timing parameters used during baseline
	// establishment for this SUT.
	BaselineConfig() BaselineConfig
}

// EnvoyFaultPortsProvider is an optional add-on interface a SUT may
// implement to declare the inbound TCP ports its services listen on. Used
// by the Envoy-fault injection path (pkg/sut/envoy) to install iptables
// REDIRECT rules that route those ports through the injected Envoy
// sidecar. SUTs that don't implement this interface fall back to the
// Manager's default port list, which may not match the workload's actual
// service ports — Envoy will be injected but won't intercept anything.
type EnvoyFaultPortsProvider interface {
	EnvoyFaultPorts() []int
}

// WorkloadRef identifies a workload by Kind + Name.
type WorkloadRef struct {
	Kind string `json:"kind"` // "Deployment" | "StatefulSet"
	Name string `json:"name"`
}

// BaselineConfig governs how long the deploy operation waits for things to
// stabilize.
type BaselineConfig struct {
	// ReadyTimeout is the maximum time to wait for all expected workloads to
	// reach Ready before giving up with a timeout error.
	ReadyTimeout time.Duration
	// StabilityWindow is the period the workloads must stay Ready continuously
	// after first reaching Ready before the baseline is declared.
	StabilityWindow time.Duration
	// PollInterval is how often to check workload status.
	PollInterval time.Duration
}

// DefaultBaselineConfig returns reasonable defaults for most SUTs.
func DefaultBaselineConfig() BaselineConfig {
	return BaselineConfig{
		ReadyTimeout:    5 * time.Minute,
		StabilityWindow: 30 * time.Second,
		PollInterval:    3 * time.Second,
	}
}

// Baseline is the snapshot captured by EstablishBaseline. It tells the
// autonomous-mode health gate (M3) what "healthy" looks like for this
// namespace + SUT combination.
type Baseline struct {
	Namespace       string           `json:"namespace"`
	SUT             string           `json:"sut"`
	EstablishedAt   time.Time        `json:"established_at"`
	StabilityWindow time.Duration    `json:"stability_window"`
	Workloads       []WorkloadStatus `json:"workloads"`
}

// WorkloadStatus is one entry in the Baseline's Workloads slice.
type WorkloadStatus struct {
	Kind            string `json:"kind"`
	Name            string `json:"name"`
	DesiredReplicas int32  `json:"desired_replicas"`
	ReadyReplicas   int32  `json:"ready_replicas"`
}

// Registry stores SUT implementations addressable by name.
type Registry interface {
	Get(name string) (SUT, bool)
	List() []SUT
}
