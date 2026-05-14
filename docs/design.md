# Simian Agent — Design

> **Status:** Draft, v1 scope.
> **Related:** [`requirements.md`](./requirements.md), [`roadmap.md`](./roadmap.md).
> Supersedes the design portion of [`simian-agent.md`](./simian-agent.md).
> Requirement IDs (`R-FOO-NN`) reference `requirements.md`.

## 1. Architectural Overview

Simian is a single Go binary that ships as two Kubernetes workloads sharing one image. All fault sources — autonomous-mode plans and directed-mode MCP calls alike — funnel through one Fault Executor before any chaos resource is applied.

```
                                 +---------------------------+
                                 |     LLM Provider          |
                                 |  (pluggable; Gemini v1)   |
                                 +-------------+-------------+
                                               ^
                            structured output  |  read-only context
                                               |
   +------------------------+    +-------------+-------------+    +-------------------------+
   |  Topology Discoverer   |--->|     Plan Generator        |    |   MCP Server (directed) |
   |  (read-only K8s + mesh)|    |  (autonomous mode loop)   |    |   - submit_fault        |
   +------------------------+    +-------------+-------------+    |   - clear_fault         |
                                               |                  |   - list_fault_catalog  |
                                               | AttackPlan       |   - list_active_faults  |
                                               v                  +-----------+-------------+
                                       +-------+--------+                     |
                                       |                |                     | FaultManifest
                                       |                v                     v
                                       |       +-----------------------------------+
                                       |       |       FAULT EXECUTOR              |
                                       |       |  (1) schema validate              |
                                       |       |  (2) safety validate              |
                                       |       |  (3) audit pre-apply              |
                                       |       |  (4) apply via driver             |
                                       |       |  (5) lease + lifecycle            |
                                       |       |  (6) audit post-apply             |
                                       |       +----------------+------------------+
                                       |                        |
                                       |          +-------------+-------------+
                                       |          v                           v
                                       |  +---------------+         +---------------+
                                       |  | Chaos Mesh    |         | Litmus driver |
                                       |  | driver        |         | (workflows +  |
                                       |  | (dynamic CRD) |         |  probes)      |
                                       |  +-------+-------+         +-------+-------+
                                       |          |                         |
                                       v          v                         v
                                +---------------------------------------------------------+
                                |                Eligible Target Namespaces               |
                                |   (annotated; chaos SA RBAC-bound; SUT may be Simian-   |
                                |    provisioned per provisioned posture)                 |
                                +---------------------------------------------------------+
                                                       |
                                                       | observable effects, probe results
                                                       v
                                       +---------------+---------------+
                                       |  Scenario Exporter            |
                                       |  + Red Phone (incident pages) |
                                       +---------------+---------------+
                                                       |
                                                       v
                                +---------------------------------------------+
                                |  External SRE agent / evaluation harness    |
                                +---------------------------------------------+
```

### 1.1 Component inventory

| Component             | Responsibility                                                                                           | Mode(s)               |
| --------------------- | -------------------------------------------------------------------------------------------------------- | --------------------- |
| LLM Provider          | Stateless completion API behind a pluggable interface; Gemini default                                    | Both                  |
| Topology Discoverer   | Read-only inspection of eligible namespaces                                                              | Autonomous            |
| Plan Generator        | LLM-driven `AttackPlan` synthesis; orchestrates the autonomous cycle                                     | Autonomous            |
| MCP Server            | Directed-mode tool surface and read-only context tools used by the LLM                                   | Both (caller-facing)  |
| **Fault Executor**    | Single chokepoint: validate → audit → apply → lease → audit. No bypass.                                  | Both                  |
| Chaos Mesh driver     | Dynamic-CRD apply for the full `chaos-mesh.org/v1alpha1` catalog                                         | Both                  |
| Litmus driver         | `ChaosEngine` / workflow apply; probe attachment; ChaosHub-sourced experiments                           | Both                  |
| Provisioner           | Cluster-scoped: creates eligible namespaces, deploys SUT, manages chaos SA RoleBindings                  | Provisioned posture   |
| Red Phone             | Best-effort outbound natural-language incident pages                                                     | Both, optional        |
| Scenario Exporter     | Stable structured records of inputs/outputs per cycle, for external evaluation                           | Both                  |
| Lease Reaper          | Background sweeper that clears any fault whose lease is stale or duration is exceeded                    | Both                  |

## 2. Operating Modes

### 2.1 Directed mode

```
External caller (human, agent, CI)
        |
        | MCP: submit_fault(intent, targets, options)
        v
   MCP Server  ──→  LLM.TranslateIntent(intent, catalog)  ──→  FaultManifest
        |                                                            |
        |                              <───── FaultManifest ─────────┘
        v
   Fault Executor (validate → audit → apply → lease)
        |
        v
   returns {planID, faultUIDs[], status}
```

The caller can poll/stream status via `get_fault_status(planID)` or watch the Red Phone webhook for any pages emitted during the fault window. Directed mode is the integration path for upstream agents (Claude Code, ADK agents, internal tooling) and CI jobs.

### 2.2 Autonomous mode

```
Tick (configurable interval)
        |
        v
   Health gate: cluster baseline OK?  ──no──→ skip cycle, log
        | yes
        v
   Topology Discoverer: snapshot eligible namespaces (read-only)
        |
        v
   Plan Generator → LLM.GeneratePlan(topology, catalog, budget, history)
        |
        v
   AttackPlan {hypothesis, ordered steps, probes}
        |
        v
   For each step (under budget caps):
        |  → Fault Executor (validate → audit → apply → lease → audit)
        |  → optional Red Phone dispatch
        |  → emit step record to Scenario Exporter
        v
   Cycle record finalized → Scenario Exporter emits final document
```

Plans are always emitted *before* execution and audit-logged. `simian plan` runs the cycle in plan-only mode (no apply), giving a free dry-run knob.

### 2.3 Shared substrate

Both modes converge at the `FaultManifest` layer. The Fault Executor does not know or care which mode produced a manifest — the validation, audit, lease, and lifecycle path is identical. That symmetry means budget caps (max concurrent faults, cooldowns) apply across modes, so a directed-mode submission can't sidestep an autonomous-mode budget and vice versa.

## 3. Fault Executor

The Fault Executor is the most safety-critical component. It is the **only** code path that calls into a chaos driver. Every other component speaks `FaultManifest` to it.

### 3.1 The `FaultManifest` type

```go
type FaultManifest struct {
    UID             string                 // generated; opaque
    Source          ManifestSource         // "directed" or "autonomous"
    Engine          string                 // "chaos-mesh" or "litmus"
    APIVersion      string                 // e.g. "chaos-mesh.org/v1alpha1"
    ResourceKind    string                 // e.g. "NetworkChaos", "ChaosEngine"
    Spec            map[string]any         // engine-native spec, validated against CRD OpenAPI
    Targets         []TargetRef            // namespace/workload(s) — denormalized for safety checks
    Duration        time.Duration          // hard cap; ≤ installation ceiling
    BlastRadiusTier BlastRadiusTier        // "namespace" | "node" | "external"
    Probes          []ProbeSpec            // optional, Litmus only
    Rationale       string                 // LLM-supplied; opaque to executor
    PlanID          string                 // optional; ties step to AttackPlan
}
```

`Spec` deliberately uses `map[string]any` rather than typed Go structs per fault type. Per `R-FAULT-01` and `R-FAULT-02`, Simian integrates at the CRD layer; typed wrappers would defeat the "full catalog" requirement and break the day a new Chaos Mesh resource ships. Schema integrity comes from validating `Spec` against the live CRD OpenAPI schema fetched from the cluster, not from Go's type system.

### 3.2 Pipeline

```
   FaultManifest in
        |
        v
  +---------------------------------------------------+
  | 1. Schema validation                              |
  |    - GVK present in cluster?                      |
  |    - Spec validates against CRD OpenAPI schema?   |
  +---------------------------------------------------+
        |
        v
  +---------------------------------------------------+
  | 2. Safety validation                              |
  |    - Targets in eligible namespace? (annotation)  |
  |    - Targets not in exclude-workloads list?       |
  |    - chaos SA RBAC permits the GVK in the NS?     |
  |    - BlastRadiusTier permitted by config?         |
  |    - Per-spec re-classification (DNSChaos /       |
  |      NetworkChaos with external CIDRs etc.)       |
  |    - Duration ≤ installation ceiling?             |
  |    - Budget allows? (concurrency, cooldown,       |
  |      cycle count, severity tier)                  |
  +---------------------------------------------------+
        |
        v
  +---------------------------------------------------+
  | 3. Audit: pre-apply record                        |
  +---------------------------------------------------+
        |
        v
  +---------------------------------------------------+
  | 4. Driver.Apply(spec) → engine-side resource UID  |
  +---------------------------------------------------+
        |
        v
  +---------------------------------------------------+
  | 5. Lease registration                             |
  |    - in-memory active-fault registry              |
  |    - lease CR (for crash recovery)                |
  |    - heartbeat goroutine                          |
  |    - deadline = now + Duration                    |
  +---------------------------------------------------+
        |
        v
  +---------------------------------------------------+
  | 6. Audit: post-apply record (success or failure)  |
  +---------------------------------------------------+
        |
        v
   FaultUID out
```

Failure at any stage emits an audit record with the rejection reason. Stages 1–2 produce no side effects; stages 3–6 are durably logged.

### 3.3 The Go interface

```go
type FaultExecutor interface {
    // Apply runs the full pipeline. Returns the engine-side UID on success
    // or a typed error describing which stage rejected the manifest.
    Apply(ctx context.Context, m FaultManifest) (faultUID string, err error)

    // Clear removes an active fault before its lease expires.
    Clear(ctx context.Context, faultUID string) error

    // ListActive returns the current set of leased faults.
    ListActive(ctx context.Context) ([]ActiveFault, error)
}

type ChaosDriver interface {
    Engine() string                                   // "chaos-mesh" | "litmus"
    Apply(ctx context.Context, m FaultManifest) (engineUID string, err error)
    Clear(ctx context.Context, engineUID string) error
    Catalog(ctx context.Context) ([]CatalogEntry, error)  // discovered fault types
}
```

The `ChaosDriver` interface stays thin. All policy lives in the executor.

### 3.4 Lease & lifecycle

Every applied fault is tracked in two places: an in-memory `ActiveFault` registry (fast-path for the reaper) and a `SimianLease` Custom Resource per fault (durable, for crash recovery).

```
Fault applied → ActiveFault registered → SimianLease CR created
                       |                            |
                       v                            v
              Heartbeat goroutine            Lease holder = pod ID
              refreshes lease every          (uses K8s leader election
              N seconds                      for ownership)
                       |
                       v
              On Duration expiry OR
              heartbeat stopped:
                  Reaper.Sweep() →
                      Driver.Clear(engineUID)
                      Delete SimianLease CR
                      Remove from registry
                      Emit audit record
```

On controller startup, the executor scans all `SimianLease` CRs. Any whose holder is no longer alive (or whose deadline has passed) is reaped. This satisfies `R-FAULT-05`: crash safety has no dependency on graceful shutdown.

### 3.5 Crash & restart semantics

| Event                              | Behavior                                                                                |
| ---------------------------------- | --------------------------------------------------------------------------------------- |
| Graceful shutdown (SIGTERM)        | Reaper clears all owned faults; lease CRs deleted; exit                                 |
| Crash (SIGKILL / OOM)              | Faults remain applied until lease expiry; new pod adopts/clears via lease scan          |
| Cluster API outage during apply    | Audit records the failure; manifest may be retried by caller; no partial-applied state  |
| Driver returns success but no UID  | Treated as failure; safety net manifest scan reaps any orphaned chaos resource at boot  |
| Lease CR write fails post-apply    | Fault is force-cleared via driver; surfaces as apply failure to caller                  |

## 4. LLM Provider

### 4.1 Pluggable interface (`R-LLM-01`)

```go
type LLMProvider interface {
    Name() string  // "gemini" | "claude" | "openai" | ...

    // Complete is a single completion call with optional structured output and tool calling.
    // Streaming and multi-turn agent loops are layered above this in package planner.
    Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error)
}

type CompletionRequest struct {
    System        string
    Messages      []Message
    Tools         []ToolDef          // read-only MCP tools the model may call
    ResponseSchema *jsonschema.Schema // for structured output (AttackPlan, FaultManifest)
    Temperature   float32
    MaxTokens     int
}

type CompletionResponse struct {
    Text       string                // empty when ResponseSchema is set
    Structured json.RawMessage       // populated when ResponseSchema is set
    ToolCalls  []ToolCall            // any tool calls the model wants made
    Usage      TokenUsage
}
```

The interface is intentionally low-level: a single completion with tool calls and structured output. Higher-level operations (`GeneratePlan`, `TranslateIntent`, `GenerateIncidentPage`) compose this in the `planner` package — they're not part of the provider contract, so swapping models doesn't require reimplementing business logic.

### 4.2 Gemini implementation

The `gemini` package wraps the official Vertex AI Go SDK (`cloud.google.com/go/vertexai/genai`) and implements `LLMProvider`. Authentication via Workload Identity. Default model: `gemini-2.5-pro` for planning, `gemini-2.5-flash` for incident page generation (cheaper, latency-sensitive). Both are configurable.

### 4.3 ADK relationship

ADK (Google Agent Development Kit) is convenient for Gemini but is Gemini-shaped. Rather than wrap ADK and impair pluggability, Simian writes its own thin agent loop on top of `LLMProvider`. The loop is small enough — gather context, call complete, validate output, optionally execute tool calls, repeat to a bounded iteration cap — that owning it is cheaper than fighting ADK's assumptions when implementing non-Gemini providers later.

This is a deliberate departure from the original `simian-agent.md` framing of "ADK-driven brain." The agent is *LLM-driven*; the LLM happens to be Gemini in v1.

### 4.4 Tool surface available to the LLM (`R-LLM-04`)

Read-only only:

| Tool                       | Returns                                                              |
| -------------------------- | -------------------------------------------------------------------- |
| `list_pods(ns, selector?)` | Pods, status, restart counts, node assignments                       |
| `get_pod_logs(ns, pod, …)` | Tail of structured logs                                              |
| `describe_workload(ns, w)` | Deployment/StatefulSet spec, replica counts, current rollout status  |
| `get_topology(ns)`         | Cached `TargetTopology` (services, dep graph, replica map)           |
| `get_metrics(query)`       | Range-query against configured Prometheus / Cloud Monitoring         |
| `get_baseline(ns)`         | Last-established baseline snapshot for the namespace                 |
| `list_fault_catalog()`     | All fault types installed and permitted, with CRD schema + tier      |
| `list_recent_faults(ns)`   | Recently-applied faults (so the LLM can avoid pointless repetition)  |

There is **no** `apply_fault` tool the LLM can invoke. The only way to cause a fault is to emit a structured `FaultManifest` (directed) or `AttackPlan` (autonomous) as the response. The Fault Executor is the only consumer of those structures.

### 4.5 Output contracts

```go
// AttackPlan — autonomous mode response schema
type AttackPlan struct {
    PlanID     string       // server-generated
    Hypothesis string       // "what we expect to see if this works"
    Steps      []PlanStep
    Budget     PlanBudget   // declared by LLM; further capped by executor config
}

type PlanStep struct {
    Order     int
    Manifest  FaultManifest
    Rationale string
    DependsOn []int  // step ordering (parallel by default, serial via deps)
}

// Directed mode response schema is just FaultManifest.
```

Both schemas are JSON-Schema-validated before reaching the executor (`R-LLM-05`). On schema-invalid output, the planner retries once with the validation error fed back as a correction prompt; second failure fails the cycle (autonomous) or the call (directed).

### 4.6 Failure handling (`R-LLM-06`)

| Failure                         | Directed mode                  | Autonomous mode             |
| ------------------------------- | ------------------------------ | --------------------------- |
| Provider unreachable / timeout  | Return error to caller         | Skip cycle, log, next tick  |
| Schema-invalid output           | Retry once; then error         | Retry once; then skip       |
| Tool call refers to unknown ns  | Reject with explanatory error  | Reject; counts toward budget|
| Token budget exhausted          | Return error to caller         | Skip cycle, log             |

There is **no** rule-based fallback. Autonomous mode without a working LLM is intentionally a no-op.

## 5. Chaos Drivers

### 5.1 Chaos Mesh driver

Implements `ChaosDriver` over Chaos Mesh's CRDs via `client-go`'s dynamic client. No typed wrappers per fault type — the driver applies whatever `Spec` arrives in the manifest, after the executor has validated it against the CRD's OpenAPI schema.

```go
func (d *chaosMeshDriver) Apply(ctx context.Context, m FaultManifest) (string, error) {
    obj := &unstructured.Unstructured{}
    obj.SetAPIVersion(m.APIVersion)
    obj.SetKind(m.ResourceKind)
    obj.SetGenerateName("simian-")
    obj.SetNamespace(m.Targets[0].Namespace)
    if err := unstructured.SetNestedMap(obj.Object, m.Spec, "spec"); err != nil {
        return "", err
    }
    // Inject duration field — every Chaos Mesh resource accepts a `duration` string.
    unstructured.SetNestedField(obj.Object, m.Duration.String(), "spec", "duration")
    created, err := d.client.Resource(d.gvrFor(m)).Namespace(obj.GetNamespace()).Create(ctx, obj, metav1.CreateOptions{})
    if err != nil { return "", err }
    return string(created.GetUID()), nil
}
```

`Catalog()` enumerates every `chaos-mesh.org/v1alpha1` CRD installed in the cluster, fetches each one's OpenAPI schema, classifies its blast-radius tier, and returns the entries.

### 5.2 Litmus driver

Same generic pattern — `ChaosEngine` and workflow CRDs applied via the dynamic client. Distinct from Chaos Mesh in three ways:

- **Workflows (`R-FAULT-08`):** When an `AttackPlan` contains multiple steps targeting Litmus, the driver materializes them as a single Litmus workflow CRD. The plan's `DependsOn` graph maps to workflow step dependencies. The executor still tracks each leaf step as its own `ActiveFault` entry, but lifecycle (apply, clear, status) is delegated to Litmus's workflow controller.
- **Probes (`R-FAULT-09`):** `ProbeSpec` entries on a step become Litmus probe definitions attached to the underlying `ChaosEngine`. Probe results (pass/fail with reason) are pulled from `ChaosResult` CRs and pushed into the audit log and the scenario record.
- **ChaosHub (`R-FAULT-10`):** Experiment definitions are sourced from configured hubs at install time (hub URLs are Helm values). The catalog discovery surfaces installed experiments as available fault types; LLM proposals reference them by name.

### 5.3 Catalog discovery (`R-FAULT-07`)

On startup and every `catalog.refreshInterval` (default 5 min), each driver enumerates its installed surface:

```go
type CatalogEntry struct {
    Engine          string           // "chaos-mesh" | "litmus"
    APIVersion      string
    ResourceKind    string           // CRD Kind, or Litmus experiment name
    Schema          *jsonschema.Schema  // for validating Spec
    BlastRadiusTier BlastRadiusTier  // base classification (may be refined per-spec)
    Description     string           // human-readable, sourced from CRD or experiment metadata
}
```

The merged catalog is exposed to the LLM via `list_fault_catalog()`. This is how the LLM "knows what's available" — never a hardcoded list.

### 5.4 Blast-radius classification (`R-FAULT-06`)

A static map gives each CRD Kind its baseline tier (e.g. `KernelChaos → node`, `AWSChaos → external`). For fault types whose tier depends on the spec (`DNSChaos`, `NetworkChaos`), the executor performs a per-spec re-classification at validation time:

- `NetworkChaos` whose target IPs/CIDRs include any address outside the cluster's pod/service CIDRs → escalate to `external`.
- `DNSChaos` configured against in-cluster CoreDNS → `namespace` (or `node` if it acts via host-net rules); against external resolvers → `external`.

Default policy permits `namespace` and `node`; `external` is opt-in via Helm values.

## 6. Topology Discoverer & Health Model

### 6.1 Topology data model

```go
type TargetTopology struct {
    Namespace       string
    DiscoveredAt    time.Time
    Workloads       []Workload                // Deployments, StatefulSets, DaemonSets
    Services        []Service
    DependencyGraph map[string][]string       // service → callees
    ReplicaMap      map[string]int32          // workload → desired replicas
    PodStatus       map[string][]PodSummary   // workload → pods (Ready, restarts, age)
    RecentEvents    []EventSummary            // last N K8s Events
}
```

Discovery is read-only and uses informers (cached, watch-driven) so the LLM context tools are cheap.

### 6.2 Dependency graph sources

In priority order:

1. Service mesh telemetry (Istio, Linkerd) when present.
2. NetworkPolicy declarations.
3. Workload env vars referencing service DNS names (heuristic).
4. OpenTelemetry trace collector if scraped.

Absence of all four yields a topology with services-but-no-edges. The LLM is told this in its prompt context so it doesn't hallucinate dependencies.

### 6.3 Baseline establishment (provisioned posture)

After provisioning a SUT into an eligible namespace, the provisioner blocks until:

1. All declared workload pods report `Ready`.
2. The SUT's load generator (Online Boutique includes one) is producing requests.
3. Configured baseline metrics (default: error rate < 1%, p99 latency stable for 60s) hold across a baseline window (default: 2 min).

The resulting `Baseline` snapshot — pod readiness map, metric values, replica counts, snapshot time — is cached and exposed via `get_baseline()`. Chaos cycles will not begin until a baseline exists for the namespace.

### 6.4 Health gate

Before each autonomous cycle, the loop checks:

- All baseline pods still `Ready` (allowing for the post-prior-cycle recovery window).
- No active Simian fault still leased in the namespace.
- Metric drift from baseline within a generous tolerance (the chaos cycle will produce drift; here we only care that we're starting from a non-broken state).

A failed gate skips the cycle (audit `cycle.health_gate_failed` + `cycle.skipped`) and moves on without applying anything.

> **M3 v1 scope (2026-05-14):** The shipped health gate (`pkg/loop.BaselineHealthGate`) checks pod-Ready + no-active-faults via the topology snapshot. The metric-drift check is gated on `get_metrics` having a real backend; the M3 stub returns `{configured:false}`, so adding a metric-drift signal is deferred to whichever milestone wires Prometheus / Cloud Monitoring. The gate's interface accepts new checks without breaking callers.

### 6.5 Vulnerability ranking

The original doc described a `EvaluateVulnerabilities` step that returns a ranked `[]FaultManifest`. In this design, the ranking *is* the LLM's `AttackPlan` — the LLM consumes topology + catalog + budget + recent history and emits an ordered plan. There is no separate scoring algorithm. This keeps the "intent-driven" framing honest: the rules supply context, the LLM supplies intent.

Per-installation hard rules (e.g. "never target the load generator," "never run two NetworkChaos in the same NS") are enforced by the executor's safety stage, not by pre-filtering the catalog the LLM sees. This way the LLM can be told *why* a proposal was rejected.

## 7. MCP Server & Tool Surface

The MCP server runs in-process inside `simian serve`. Two distinct tool sets:

### 7.1 Directed-mode tools (caller-facing)

| Tool                                  | Purpose                                                                  |
| ------------------------------------- | ------------------------------------------------------------------------ |
| `submit_fault(intent, targets, opts)` | Translate intent → manifest → executor; return `{planID, faultUIDs}`     |
| `submit_plan(plan)`                   | Bypass LLM translation; submit a fully-formed `AttackPlan` (CI use case) |
| `clear_fault(faultUID)`               | Force-clear a leased fault                                               |
| `get_fault_status(planID)`            | Current status of all faults from a plan                                 |
| `list_active_faults(ns?)`             | All currently leased faults                                              |
| `list_fault_catalog()`                | Available fault types and their tiers                                    |

### 7.2 Read-only context tools (LLM-facing)

Same set as §4.4. These tools are also exposed over MCP so external agents (and humans via Claude Code) can inspect the cluster through Simian's lens without needing direct cluster credentials.

### 7.3 Auth & transport

MCP over HTTP+SSE on a configurable port. Authentication via short-lived bearer tokens issued to clients (Helm value or external secret). Connections from inside the cluster can use the chaos SA token automatically. All inbound traffic is logged.

## 8. Red Phone (Outbound Event Bridge)

### 8.1 Page generation

After each fault apply, if `redphone.enabled` is true, the planner asynchronously calls `LLM.GenerateIncidentPage(faultOutcome, style)` — a separate, lightweight LLM call distinct from planning. Output is the natural-language `prompt_page` plus structured `telemetry_context`.

Two linguistic styles supported in v1:

- **Direct** — explicit telemetry framing ("p99 latency on `paymentservice` breached baseline, reporting 450ms").
- **Symptoms-only** — user-perspective framing ("Customers report checkout requests hanging at the final validation screen"). Tests downstream agents' ability to discover the technical cause.

### 8.2 Webhook dispatch

```go
type RedPhoneDispatcher interface {
    Dispatch(ctx context.Context, page IncidentPage) error
}
```

HTTP POST with an HMAC-SHA256 signature header (`X-Simian-Signature`) keyed by a per-webhook secret. Bounded retry with exponential backoff (default 3 attempts). A failed dispatch is logged and counted in metrics; it never rolls back, aborts, or blocks an applied fault (`R-PAGE-04`).

### 8.3 Schema

```json
{
  "incident_id":     "string",
  "source_fault_uid":"string",
  "plan_id":         "string|null",
  "prompt_page":     "string",
  "linguistic_style":"direct|symptoms-only",
  "telemetry_context": {
    "namespace":         "string",
    "impacted_workload": "string",
    "fault_kind":        "string",
    "blast_radius_tier": "namespace|node|external",
    "observed_anomaly":  "string"
  },
  "timestamp":       "RFC3339"
}
```

## 9. Scenario Data Export

### 9.1 The `ScenarioRecord`

```go
type ScenarioRecord struct {
    SchemaVersion string              // semver; bump on breaking changes
    ScenarioID    string              // ulid
    Mode          string              // "directed" | "autonomous"
    StartedAt     time.Time
    EndedAt       time.Time

    Inputs  ScenarioInputs
    Outputs ScenarioOutputs
}

type ScenarioInputs struct {
    PlanID         string
    Hypothesis     string             // LLM's stated expectation
    AppliedFaults  []AppliedFault     // full Spec, target, blast tier, rationale
    BaselineSnapshot Baseline
    PageDispatched *IncidentPage      // nil if Red Phone disabled
}

type ScenarioOutputs struct {
    ProbeResults     []ProbeResult     // Litmus probes
    MetricDeltas     []MetricSeries    // values during fault window
    LeaseEvents      []LeaseEvent      // applied, refreshed, cleared
    AgentResponses   []AgentResponse   // inbound from Red Phone listener
    TimeToRecovery   *time.Duration    // nil if not observed
    EngineErrors     []EngineError
}
```

Every field is JSON-serializable, every name is stable, the schema is versioned. External harnesses consume this without Simian-specific code.

### 9.2 Sinks (`R-EXPORT-03`)

Configurable per installation:

- **Filesystem** — one JSON file per scenario at `${path}/${scenarioID}.json`
- **Object storage** — write to GCS/S3 bucket
- **Webhook** — POST the record to a configured endpoint

Multiple sinks may be enabled simultaneously.

### 9.3 Streaming feed

In addition to final records, the exporter publishes incremental events on an internal pub/sub during the cycle:

```
scenario.started → fault.planned → fault.applied → page.dispatched →
  probe.result → metric.snapshot → agent.response → fault.cleared → scenario.ended
```

External harnesses subscribe via a streaming MCP tool (`stream_scenario_events`) or a webhook. Useful for harnesses that grade in-flight rather than post-hoc.

## 10. Deployment Topology

### 10.1 Single binary, multiple subcommands

```
simian
├── serve       long-running controller; Plan Generator + MCP server + Red Phone + Exporter
├── provision   namespace/SUT lifecycle; RoleBinding management
├── chaos       directed-mode CLI client (talks to controller's MCP)
├── plan        autonomous dry-run / plan-only
└── evaluate    helper for invoking external evaluation harnesses against scenario records
```

One Go module, one container image. Subcommands give operational ergonomics without multiplying the supply chain. The `evaluate` subcommand is a *driver* for external harnesses, not a harness itself — it locates scenario records, invokes the configured external harness command, and surfaces results.

### 10.2 Two Kubernetes workloads, two ServiceAccounts

```
+-----------------------------------------+    +-----------------------------------------+
|  simian-controller   (Deployment)       |    |  simian-provisioner  (Job/CronJob)      |
|  cmd:  simian serve                     |    |  cmd:  simian provision …               |
|  SA:   chaos-sa                         |    |  SA:   provisioner-sa                   |
|  RBAC: per-NS Role bindings created     |    |  RBAC: cluster Role for ns + RB create, |
|        by provisioner; can mutate       |    |        annotation-filtered by webhook   |
|        chaos-mesh.org & litmuschaos.io  |    |        admission                        |
|        CRDs in eligible namespaces only |    |  Plus: per-NS Role bindings for the     |
|                                         |    |        chaos SA                          |
+-----------------------------------------+    +-----------------------------------------+
            |                                                |
            +------------------ Eligible NSes ---------------+
                              (annotation: simian.chaos/eligible="true")
```

The provisioner is the only privileged actor. It has cluster-scoped power but only over namespaces and RoleBindings, gated by an annotation filter enforced through a `ValidatingAdmissionPolicy` so even a buggy provisioner can't create non-eligible-flagged namespaces or grant the chaos SA access elsewhere.

### 10.3 RBAC manifests (sketch)

```yaml
# chaos-sa: namespace-scoped, created per-eligible-NS by provisioner
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: simian-chaos
  namespace: ${eligible-ns}
rules:
  - apiGroups: ["chaos-mesh.org"]
    resources: ["*"]
    verbs: ["create", "get", "list", "watch", "patch", "delete"]
  - apiGroups: ["litmuschaos.io"]
    resources: ["chaosengines", "chaosresults", "chaosschedules"]
    verbs: ["create", "get", "list", "watch", "patch", "delete"]
  - apiGroups: [""]
    resources: ["pods", "pods/log", "events", "configmaps", "services"]
    verbs: ["get", "list", "watch"]
```

```yaml
# provisioner-sa: cluster-scoped but tightly bounded
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: simian-provisioner
rules:
  - apiGroups: [""]
    resources: ["namespaces"]
    verbs: ["create", "get", "list", "watch", "delete", "patch"]
  - apiGroups: ["rbac.authorization.k8s.io"]
    resources: ["roles", "rolebindings"]
    verbs: ["create", "get", "list", "delete"]
  # Plus permissions to deploy SUT manifests (e.g. apps/Deployments) into eligible NSes —
  # constrained by the admission policy below.
```

A `ValidatingAdmissionPolicy` rejects any provisioner-originated namespace creation that lacks `simian.chaos/eligible="true"`, and any RoleBinding that grants the chaos SA access into a non-eligible namespace. This is the defense-in-depth backstop on RBAC.

## 11. Configuration Model

Three layers, each appropriate for different reload semantics:

| Layer                | Reload semantics      | Example values                                                          |
| -------------------- | --------------------- | ----------------------------------------------------------------------- |
| Helm values          | Restart-only          | LLM provider creds, RBAC, SA names, sinks, blast tier opt-ins, hub URLs |
| `Simian` ConfigMap   | Hot-reload via watch  | Budget caps, cooldowns, baseline tolerances, page styles, log verbosity |
| CLI flags / env vars | Per-invocation        | `simian chaos`/`plan`/`evaluate` operations                             |

A future iteration may introduce a `ChaosArena` CRD for declarative SUT/eligibility setup, but for v1 namespace + RBAC + annotations are managed by the provisioner.

## 12. Observability

### 12.1 Prometheus metrics

```
simian_cycles_total{result="ok|skipped|errored"}
simian_faults_applied_total{engine, kind, blast_tier}
simian_faults_rejected_total{stage, reason}
simian_pages_dispatched_total{result}
simian_llm_calls_total{provider, op, result}
simian_llm_call_seconds{provider, op}    # histogram
simian_active_faults{namespace}          # gauge
simian_lease_reaper_actions_total{reason}
simian_scenario_records_emitted_total{sink}
```

### 12.2 Structured logs

JSON. Reserved fields: `ts`, `level`, `component`, `scenario_id`, `plan_id`, `fault_uid`, `mode`, `error`. Free-form fields prefixed with the component name.

LLM prompt/response payloads are captured under `component="planner"` only when `log.llmPayloads.enabled=true` (default: false). When enabled, secrets/tokens scrub through a deny-list before write.

### 12.3 Audit timeline

The audit log is a separate append-only stream (default sink: structured log; optional sink: durable storage). Indexed by `fault_uid` and `scenario_id` so any incident can be reconstructed via a single query. Schema:

```
{ts, event, fault_uid?, scenario_id?, plan_id?, payload}
```

Events: `plan.generated`, `executor.received`, `executor.validated`, `executor.rejected`, `driver.applied`, `driver.failed`, `lease.heartbeat`, `lease.expired`, `lease.cleared`, `page.dispatched`, `page.failed`, `agent.response_received`.

## 13. Failure Modes & Recovery

| Surface                       | Failure                              | Response                                                                          |
| ----------------------------- | ------------------------------------ | --------------------------------------------------------------------------------- |
| LLM Provider                  | Unreachable / timeout                | Directed: error to caller. Autonomous: skip cycle, log, next tick.                |
| LLM Provider                  | Returns schema-invalid output        | Retry once with validation error fed back; fail on second invalid output.         |
| LLM Provider                  | Refuses / safety-blocks the prompt   | Audit; surface as a soft-failure of the cycle; do not auto-retry with new prompt. |
| Fault Executor — schema stage | CRD schema validation fails          | Reject; audit `executor.rejected`; counts toward LLM failure budget.              |
| Fault Executor — safety stage | Out-of-scope namespace               | Reject; audit; never reach driver.                                                |
| Fault Executor — safety stage | Blast tier above policy              | Reject; audit; surface clear "tier not enabled" reason.                           |
| Fault Executor — safety stage | Budget exceeded                      | Reject; audit; LLM may receive feedback in next cycle's context.                  |
| Driver                        | Apply fails                          | Audit `driver.failed`; do not retry; surface to caller.                           |
| Driver                        | Apply succeeds, no UID returned      | Force-clear via reaper's bootstrap scan; treat as failure.                        |
| Lease                         | Heartbeat fails N times              | Reaper clears the fault; audit `lease.cleared(reason=heartbeat-stalled)`.        |
| Lease                         | Controller crashes                   | New pod scans `SimianLease` CRs, adopts or reaps based on holder/deadline.        |
| Red Phone                     | Webhook 5xx / timeout                | Bounded retry with backoff; final failure logged + metric; never blocks fault.    |
| Scenario Exporter             | Sink write fails                     | Buffer in memory up to N records; on persistent failure, drop oldest + emit metric.|
| MCP Server                    | Inbound tool call malformed          | Reject with structured error; never partial-applied state.                        |
| Cluster API                   | Transient outage                     | client-go backoff/retry; cycles skipped during outage; audit captures gap.       |
| Provisioner                   | Cannot establish baseline            | Mark namespace unhealthy; chaos blocked until human re-provisions or restarts.    |

## 14. Open Questions

These are deliberately deferred for post-v1 iteration:

- **Multi-cluster orchestration.** The current design is single-cluster. Cross-cluster fault choreography (one Simian, many clusters) needs a separate control plane.
- **External-posture baseline ingestion.** v2 will need an adapter that reads SLO baselines from external observability stacks rather than synthesizing them from Simian-deployed SUT load.
- **`ChaosArena` CRD.** A declarative API for "this namespace is an eligible Simian arena" with embedded SUT manifests. v2 candidate.
- **Fault genealogy.** Treating successful faults as a corpus the LLM learns from across cycles. Needs persistent state beyond audit log.
- **Approval gates.** Required for the external posture. Probably a `PendingFault` CR plus a webhook for human/automated approval, but out of v1.
