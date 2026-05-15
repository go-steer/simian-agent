// Package topology provides read-only, informer-backed inspection of the
// workloads, services, and inferred dependency graph inside an arena
// namespace. It is consumed by the autonomous-mode plan generator (M3) so the
// LLM has cluster context when drafting AttackPlans, and exposed via the MCP
// `get_topology` tool so external agents and humans can inspect the same
// view without direct cluster credentials.
//
// Discovery is strictly read-only (R-DISC-02): no mutations, no chaos
// applications. Mesh telemetry (Istio/Linkerd) and OpenTelemetry trace
// collectors are deferred — M3 ships NetworkPolicy and env-var heuristics.
package topology

import "time"

// TargetTopology is a point-in-time snapshot of an arena's shape, suitable
// for handing to the LLM as context for plan generation.
type TargetTopology struct {
	Namespace       string                  `json:"namespace"`
	DiscoveredAt    time.Time               `json:"discovered_at"`
	Workloads       []Workload              `json:"workloads"`
	Services        []Service               `json:"services"`
	DependencyGraph map[string][]string     `json:"dependency_graph"`
	ReplicaMap      map[string]int32        `json:"replica_map"`
	PodStatus       map[string][]PodSummary `json:"pod_status"`
	RecentEvents    []EventSummary          `json:"recent_events"`
	// EdgeProvenance records, per directed edge "src->dst", which heuristic
	// produced it ("networkpolicy" or "envvar"). Lets the LLM judge confidence.
	EdgeProvenance map[string][]string `json:"edge_provenance"`
}

// Workload is a Deployment / StatefulSet / DaemonSet summary. Container
// details are folded down to what the planner cares about: name, image,
// and the list of env vars whose value names a service host (used by the
// dep-graph heuristic; surfaced for transparency).
type Workload struct {
	Kind            string             `json:"kind"`
	Name            string             `json:"name"`
	Labels          map[string]string  `json:"labels"`
	DesiredReplicas int32              `json:"desired_replicas"`
	Containers      []ContainerSummary `json:"containers"`
}

// ContainerSummary captures the dependency-relevant slice of a container spec.
type ContainerSummary struct {
	Name    string          `json:"name"`
	Image   string          `json:"image"`
	EnvRefs []EnvServiceRef `json:"env_refs"`
}

// EnvServiceRef is an env-var entry whose value parses as `<service>:<port>`
// or `<service>.<ns>:<port>` and therefore plausibly names a downstream
// dependency.
type EnvServiceRef struct {
	EnvName string `json:"env_name"`
	Service string `json:"service"`
	Port    string `json:"port"`
}

// Service is a Kubernetes Service summary.
type Service struct {
	Name     string            `json:"name"`
	Selector map[string]string `json:"selector"`
	Ports    []ServicePort     `json:"ports"`
}

// ServicePort is a single Service port entry.
type ServicePort struct {
	Name string `json:"name"`
	Port int32  `json:"port"`
}

// PodSummary captures the planner-relevant slice of a Pod's status.
type PodSummary struct {
	Name     string `json:"name"`
	Phase    string `json:"phase"`
	Ready    bool   `json:"ready"`
	Restarts int32  `json:"restarts"`
	NodeName string `json:"node_name"`
	AgeSec   int64  `json:"age_sec"`
}

// EventSummary is a flattened Kubernetes Event suitable for the planner.
type EventSummary struct {
	Time           time.Time `json:"time"`
	Type           string    `json:"type"` // Normal | Warning
	Reason         string    `json:"reason"`
	Message        string    `json:"message"`
	InvolvedObject string    `json:"involved_object"` // "Kind/Name"
}
