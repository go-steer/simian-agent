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
