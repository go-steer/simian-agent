// Package simian holds the core data types and interfaces shared across all
// Simian Agent subsystems. The package contains no business logic — it is the
// contract layer that lets executor, drivers, planner, MCP, audit, and lease
// packages depend on a single shared vocabulary.
package simian

import (
	"encoding/json"
	"fmt"
	"time"
)

// BlastRadiusTier classifies how far a fault can affect resources beyond its
// declared targets. See requirements.md R-FAULT-06 and design.md §5.4.
type BlastRadiusTier string

const (
	TierNamespace BlastRadiusTier = "namespace"
	TierNode      BlastRadiusTier = "node"
	TierExternal  BlastRadiusTier = "external"
)

// ManifestSource identifies which mode produced a FaultManifest. The executor
// is mode-agnostic, but the source flows into audit records and metrics labels.
type ManifestSource string

const (
	SourceDirected   ManifestSource = "directed"
	SourceAutonomous ManifestSource = "autonomous"
)

// Engine identifies which chaos engine implements a manifest.
type Engine string

const (
	EngineChaosMesh Engine = "chaos-mesh"
	EngineLitmus    Engine = "litmus"
)

// TargetRef denormalizes the namespace/workload information from the engine's
// native selector so the safety stage can validate without parsing the spec.
type TargetRef struct {
	Namespace string `json:"namespace"`
	Kind      string `json:"kind,omitempty"`
	Name      string `json:"name,omitempty"`
	// Labels is the engine-side label selector, captured for audit only.
	Labels map[string]string `json:"labels,omitempty"`
}

// ProbeSpec describes a Litmus probe attached to a fault. Empty for Chaos Mesh
// manifests in M1; expanded in M2.
type ProbeSpec struct {
	Name string         `json:"name"`
	Type string         `json:"type"` // cmd | http | k8s | prometheus
	Mode string         `json:"mode"` // SOT | EOT | Edge | Continuous | OnChaos
	Spec map[string]any `json:"spec"`
}

// FaultManifest is the engine-agnostic, mode-agnostic description of one
// chaos action. It is the only thing the Fault Executor accepts.
//
// Spec is intentionally a generic map: per requirements R-FAULT-01/02, Simian
// must support the full Chaos Mesh and Litmus catalogs without per-fault Go
// wrappers. Schema integrity comes from validating Spec against the live CRD
// OpenAPI schema fetched from the cluster, not from Go's type system.
type FaultManifest struct {
	UID             string            `json:"uid"`
	Source          ManifestSource    `json:"source"`
	Engine          Engine            `json:"engine"`
	APIVersion      string            `json:"api_version"`
	ResourceKind    string            `json:"resource_kind"`
	Spec            map[string]any    `json:"spec"`
	Targets         []TargetRef       `json:"targets"`
	Duration        time.Duration     `json:"duration"`
	BlastRadiusTier BlastRadiusTier   `json:"blast_radius_tier"`
	Probes          []ProbeSpec       `json:"probes,omitempty"`
	Rationale       string            `json:"rationale,omitempty"`
	PlanID          string            `json:"plan_id,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`
}

// UnmarshalJSON accepts "duration" as either a Go duration string ("2m",
// "30s") or an integer nanosecond count, so external callers and the LLM can
// write the human form without needing to compute nanoseconds.
func (m *FaultManifest) UnmarshalJSON(data []byte) error {
	type alias FaultManifest
	tmp := &struct {
		Duration any `json:"duration"`
		*alias
	}{
		alias: (*alias)(m),
	}
	if err := json.Unmarshal(data, tmp); err != nil {
		return err
	}
	switch v := tmp.Duration.(type) {
	case nil:
		m.Duration = 0
	case string:
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("FaultManifest.duration: %w", err)
		}
		m.Duration = d
	case float64:
		m.Duration = time.Duration(int64(v))
	case int:
		m.Duration = time.Duration(int64(v))
	case int64:
		m.Duration = time.Duration(v)
	default:
		return fmt.Errorf("FaultManifest.duration: unsupported type %T", v)
	}
	return nil
}

// MarshalJSON renders Duration as a Go duration string ("2m", "30s") so the
// on-the-wire format is symmetric with UnmarshalJSON.
func (m FaultManifest) MarshalJSON() ([]byte, error) {
	type alias FaultManifest
	return json.Marshal(&struct {
		Duration string `json:"duration"`
		alias
	}{
		Duration: m.Duration.String(),
		alias:    alias(m),
	})
}

// PlanStep is one ordered fault application within an AttackPlan.
type PlanStep struct {
	Order     int           `json:"order"`
	Manifest  FaultManifest `json:"manifest"`
	Rationale string        `json:"rationale"`
	DependsOn []int         `json:"depends_on,omitempty"`
}

// PlanBudget is the LLM-declared budget for an AttackPlan; the executor
// enforces installation-wide caps on top.
type PlanBudget struct {
	MaxConcurrentFaults int             `json:"max_concurrent_faults,omitempty"`
	MinCooldown         time.Duration   `json:"min_cooldown,omitempty"`
	MaxSeverityTier     BlastRadiusTier `json:"max_severity_tier,omitempty"`
}

// AttackPlan is the autonomous-mode response schema. Always emitted before any
// fault is applied so the cycle is auditable end-to-end.
type AttackPlan struct {
	PlanID     string     `json:"plan_id"`
	Hypothesis string     `json:"hypothesis"`
	Steps      []PlanStep `json:"steps"`
	Budget     PlanBudget `json:"budget,omitempty"`
}

// ActiveFault is what the executor's lease registry tracks for each applied
// fault until it is cleared (either by lease expiry, explicit Clear, or
// crash-recovery reaper).
type ActiveFault struct {
	FaultUID  string        `json:"fault_uid"`
	EngineUID string        `json:"engine_uid"`
	Manifest  FaultManifest `json:"manifest"`
	AppliedAt time.Time     `json:"applied_at"`
	Deadline  time.Time     `json:"deadline"`
	Holder    string        `json:"holder"` // controller pod ID
	LastBeat  time.Time     `json:"last_beat"`
}

// CatalogEntry describes one fault type the executor will accept. Discovered
// dynamically from the cluster (Chaos Mesh CRDs, Litmus experiments installed)
// and exposed to the LLM so it never proposes faults that don't exist or
// aren't permitted under current policy.
type CatalogEntry struct {
	Engine          Engine          `json:"engine"`
	APIVersion      string          `json:"api_version"`
	ResourceKind    string          `json:"resource_kind"`
	BlastRadiusTier BlastRadiusTier `json:"blast_radius_tier"`
	Description     string          `json:"description,omitempty"`
	// SchemaJSON is the CRD's OpenAPI schema for the spec field, JSON-encoded.
	// Optional in M1; required for full schema validation.
	SchemaJSON []byte `json:"schema_json,omitempty"`
}

// IncidentNotification is the Red Phone outbound payload schema (M5).
// Defined here so packages can reference it without importing redphone.
type IncidentNotification struct {
	IncidentID      string         `json:"incident_id"`
	SourceFault     string         `json:"source_fault_uid"`
	PlanID          string         `json:"plan_id,omitempty"`
	PromptPage      string         `json:"prompt_page"`
	LinguisticStyle string         `json:"linguistic_style"`
	Telemetry       map[string]any `json:"telemetry_context"`
	Timestamp       time.Time      `json:"timestamp"`
}
