# **SIMIAN AGENT: Requirements and Technical Design Document**

> ## ⚠️ OBSOLETE — superseded
>
> This document has been split and substantially revised. **Do not use it as a source of truth.** Refer instead to:
>
> * **[`requirements.md`](./requirements.md)** — what Simian is, scope, operating modes, deployment postures, and the full requirements catalog (`R-*` IDs).
> * **[`design.md`](./design.md)** — architecture, the Fault Executor chokepoint, LLM Provider contract, chaos drivers, MCP surface, Red Phone, scenario export, deployment topology, failure modes.
> * **[`roadmap.md`](./roadmap.md)** — the six-week v1 phased plan.
>
> This file is retained for history only. Several decisions captured here have changed materially:
> * Two operating modes (directed + autonomous) sharing one Fault Executor.
> * LLM Provider is pluggable (Gemini default); ADK is **not** wrapped.
> * Full Chaos Mesh and Litmus catalogs via dynamic CRD application, not per-fault wrappers.
> * Blast-radius tier classification (`namespace` / `node` / `external`).
> * Simian does **not** grade SRE agents; it exports structured `ScenarioRecord`s for an external evaluation harness.
> * Single binary, two K8s workloads, two ServiceAccounts, with a `ValidatingAdmissionPolicy` backstop.

This document defines the software engineering requirements and technical specifications for **Simian Agent**—the open-source, adversarial, AI-native chaos engineering engine ("Chaos Monkey for AI").

# **Part 1: Requirements Document**

## **1.1 Objective & Scope**

The objective of **Simian Agent** is to act as an autonomous, intent-driven chaos engineering orchestrator designed specifically for modern cloud-native environments and AI-driven automation ecosystems. Instead of relying on rigid, pre-scheduled manual configurations, Simian Agent uses an intelligent control loop to discover system topologies, reason about structural vulnerabilities, and execute precise infrastructure, network, and application-level faults.

Simian Agent operates as a self-contained "Red Team" automation engine. While independent of any specific platform defender, it features a pluggable, outbound orchestration interface designed to optionally translate complex system failures into natural-language event streams and alert pages capable of triggering downstream automated SRE agents or notification sinks.

```
                    +----------------------------------------+
                    |           SIMIAN AGENT CORE            |
                    |                                        |
                    |   +--------------------------------+   |
                    |   |    ADK-Driven Topology Brain   |   |
                    |   +----------------+---------------+   |
                    |                    |                   |
                    |                    v Discovers & Plans |
                    |   +--------------------------------+   |
                    |   |  Multi-Engine Fault Driver     |   |
                    |   |  (Chaos Mesh & Litmus SDKs)    |   |
                    |   +----------------+---------------+   |
                    +--------------------|-------------------+
                                         | Injects Faults
                                         v
+------------------------------------------------------------------------------------+
|                          TARGET STAGING ARENA (GKE CLUSTER)                        |
|                                                                                    |
|   +---------------------------------+    +-------------------------------------+   |
|   |       Online Boutique SUT       |    |       GKE Managed MCP Server        |   |
|   |  (Target Microservice Topology) |    |  (Unified Observer & Audit Trail)   |   |
|   +---------------------------------+    +-------------------------------------+   |
|                                                                                    |
+------------------------------------------------------------------------------------+
                                         | Extracted Metrics / SLO Breaches
                                         v
                    +--------------------+-------------------+
                    |   Outbound Event Bridge (Red Phone)    |
                    |  (Optional Prompt Generation Engine)   |
                    +--------------------+-------------------+
                                         |
                                         | Emits Incident Pages as Prompts
                                         v
                    +--------------------+-------------------+
                    |        Downstream SRE Agent Pool       |
                    |      (Autonomous Blue Team Ecosystem)  |
                    +----------------------------------------+
```

## **1.2 Key Features & Core Requirements**

### **1.2.1 Target Environment Control & Autonomous Provisioning**

* **Req 1.1:** The agent must programmatically provision, map, and tear down target application topologies (defaulting to the polyglot **Online Boutique** microservices suite) inside isolated target cluster namespaces.
* **Req 1.2:** The provisioning sub-system must establish a verifiable steady-state baseline, blocking active fault injection until all targeted deployment nodes, stateful pods, and internal routes pass definitive liveness and readiness validations.

### **1.2.2 Autonomous Topology Discovery & Intent-Based Selection**

* **Req 2.1:** Simian Agent must autonomously map and reason about cluster microservice dependency graphs, evaluating parameters such as replica counts, traffic routing configurations, and cross-service calls via cluster manifest metadata and service mesh endpoints.
* **Req 2.2:** The agent must select targets based on heuristics rather than static configs—analyzing the topology graph to identify high-value paths (e.g., critical checkout flows) or architectural single points of failure (e.g., un-replicated deployments) to attack.

### **1.2.3 Multi-Engine Deep Chaos Injection**

* **Req 3.1 (Low-Level System Chaos):** The agent must natively implement the **Chaos Mesh Go SDK** to orchestrate precise custom resources (`v1alpha1.Chaos`) capable of inducing kernel-level, filesystem I/O, network latency, packet loss, and localized time-skew faults.
* **Req 3.2 (High-Level Application Chaos):** The agent must interface with **LitmusChaos** APIs to coordinate stateful application playbooks, such as target resource configuration corruption, third-party API availability drops, and structured multi-stage database evictions.
* **Req 3.3 (Safety Lifespans):** All injected custom resources must be generated with mandatory, hardcoded duration bounds and lease heartbeats. If the primary Simian Agent process terminates, all active cluster faults must safely self-heal.

### **1.2.4 Outbound Event & Notification Bridge (The "Red Phone")**

* **Req 4.1 (Optional Alert Generation):** Simian Agent must include an optional notification wrapper that listens to target SLO compliance. Upon successful fault verification, it must compile technical metric anomalies into descriptive, natural-language incident reports.
* **Req 4.2 (Multi-Agent Dispatching):** The event bridge must be capable of dispatching these incident profiles as structured prompts via push webhooks or streaming queues to alert, wake up, and trigger one or more downstream automated troubleshooting agents.
* **Req 4.3 (Execution Auditing):** The agent must record the exact timestamps of fault injection, alert dispatching, and system stabilization, providing a clean cron-style timeline of the event lifecycle.

# **Part 2: Technical Design Document**

Simian Agent is designed as a standalone Go service built on top of the Google Agent Development Kit (ADK) and deployed directly within Google Kubernetes Engine (GKE).

## **2.1 System Components & Core Engine Design**

```
                                 +------------------------+
                                 |   Simian Agent Core    |
                                 |    (ADK-Based Brain)   |
                                 +-----------+------------+
                                             |
           +---------------------------------+--------------------------------+
           |                                 |                                |
           v                                 v                                v
+------------------------+       +--------------------------+       +------------------------+
|    SUT Provisioner     |       | Multi-Chaos Orchestrator |       | Outbound Event Bridge  |
| (K8s client-go Engine) |       |  (Chaos Mesh & Litmus)   |       |   (Red Phone Prompt)   |
+-----------+------------+       +-----------+--------------+       +-----------+------------+
            |                                |                                 |
            | Deploys/Manages SUT            | Manipulates CRDs                | Emits Prompt Alerts
            v                                v                                 v
+------------------------------------------------------------------------------------------+
|                                  GKE Target Cluster Node                                 |
|                                                                                          |
|    +-----------------------------+               +----------------------------------+    |
|    |      Online Boutique        |               |      GKE Managed MCP Server      |    |
|    |    Target Microservices     |               |    (Cluster Observer Tooling)    |    |
|    +-----------------------------+               +----------------------------------+    |
+------------------------------------------------------------------------------------------+
```

## **2.2 Core Architectural Go Interface Patterns**

The internal Go package structure isolates data structures, target discovery logic, and fault injection modules from the low-level platform APIs.

```go
package simian

import (
	"context"
	"time"
)

// TargetTopology captures the structural dependencies discovered within a cluster
type TargetTopology struct {
	Namespace       string              `json:"namespace"`
	Services        []string            `json:"services"`
	DependencyGraph map[string][]string `json:"dependency_graph"`
	ReplicaMap      map[string]int32    `json:"replica_map"`
}

// FaultManifest defines a structured, engine-agnostic configuration of an attack
type FaultManifest struct {
	UID        string            `json:"uid"`
	Engine     string            `json:"engine"`   // "chaos-mesh" or "litmus"
	Category   string            `json:"category"` // "network", "io", "stress", "pod"
	Action     string            `json:"action"`   // e.g., "delay", "corrupt", "kill"
	Duration   time.Duration     `json:"duration"`
	Selector   map[string]string `json:"selector"`
	Attributes map[string]string `json:"attributes"`
}

// IncidentNotification encapsulates the schema used to broadcast alert prompts out-of-band
type IncidentNotification struct {
	IncidentID   string    `json:"incident_id"`
	SourceFault  string    `json:"source_fault_uid"`
	PromptPage   string    `json:"prompt_page"`
	UrgencyScore int       `json:"urgency_score"`
	Timestamp    time.Time `json:"timestamp"`
}

// TopologyDiscoverer scans active GKE namespaces to build a target graph
type TopologyDiscoverer interface {
	Discover(ctx context.Context, namespace string) (*TargetTopology, error)
	EvaluateVulnerabilities(ctx context.Context, topo *TargetTopology) ([]FaultManifest, error)
}

// ChaosAutomationEngine standardizes operations across the SDK providers
type ChaosAutomationEngine interface {
	ApplyFault(ctx context.Context, manifest FaultManifest) (string, error)
	ClearFault(ctx context.Context, faultUID string) error
	IsSystemHealthy(ctx context.Context, namespace string) (bool, error)
}

// EventDispatcher manages optional outbound signaling loops to downstream agents
type EventDispatcher interface {
	BroadcastIncident(ctx context.Context, notification IncidentNotification) error
}
```

## **2.3 The Intent Loop & Core Control Automation**

Simian Agent operates a classic control loop modified for adversarial operations. The engine continuously runs a sync cycle mapping to the pipeline below:

```go
package manager

import (
	"context"
	"fmt"
	"time"

	"simian-agent/pkg/simian"
)

type SimianController struct {
	Discoverer simian.TopologyDiscoverer
	Chaos      simian.ChaosAutomationEngine
	Dispatcher simian.EventDispatcher
	Namespace  string
}

func (sc *SimianController) ExecuteAutonomousCycle(ctx context.Context) error {
	// 1. Check system stability baseline prior to fault loop
	healthy, err := sc.Chaos.IsSystemHealthy(ctx, sc.Namespace)
	if err != nil || !healthy {
		return fmt.Errorf("cluster state unstable, aborting iteration: %w", err)
	}

	// 2. Discover live topology layout
	topology, err := sc.Discoverer.Discover(ctx, sc.Namespace)
	if err != nil {
		return fmt.Errorf("failed topology discovery mapping: %w", err)
	}

	// 3. Evaluate architecture and select the ideal attack vectors
	vulnerabilities, err := sc.Discoverer.EvaluateVulnerabilities(ctx, topology)
	if err != nil || len(vulnerabilities) == 0 {
		return fmt.Errorf("no target vulnerabilities calculated: %w", err)
	}
	primeTarget := vulnerabilities[0] // Select highest ranked adversarial target

	// 4. Inject structural failure via active engine
	faultUID, err := sc.Chaos.ApplyFault(ctx, primeTarget)
	if err != nil {
		return fmt.Errorf("critical failure applying chaos mutation: %w", err)
	}

	// 5. Optional Outbound Event Dispatches (Red Phone)
	// Give chaos a small window to manifest in metrics tracking
	time.Sleep(15 * time.Second)
	if sc.Dispatcher != nil {
		notification := sc.generateIncidentPrompt(primeTarget, faultUID)
		_ = sc.Dispatcher.BroadcastIncident(ctx, notification)
	}

	return nil
}

func (sc *SimianController) generateIncidentPrompt(fm simian.FaultManifest, uid string) simian.IncidentNotification {
	return simian.IncidentNotification{
		IncidentID:   fmt.Sprintf("inc-%s", uid),
		SourceFault:  uid,
		PromptPage:   fmt.Sprintf("ALERT CONTEXT: Automated failure injected on target resource %s. Action match: %s. Verify downstream recovery systems.", fm.Selector["app"], fm.Action),
		UrgencyScore: 5,
		Timestamp:    time.Now(),
	}
}
```

## **2.4 GKE Managed MCP Server & Observation Architecture**

Simian Agent leverages the **GKE Managed Model Context Protocol (MCP) Server** as its standardized data abstraction plane.

```
+----------------------+                   +------------------------+
|  Simian Agent Core   |                   | GKE Managed MCP Server |
|  (Adversarial Brain) | =================>| (Standardized Tooling) |
+----------------------+   Queries State   +-----------+------------+
                                                       |
                                                       | Exposes JSON-RPC
                                                       v
                                           +------------------------+
                                           | GKE Cluster Operations |
                                           |   Pods, Logs, Metrics  |
                                           +------------------------+
```

Instead of managing internal custom resource definitions for basic actions, Simian Agent uses standard MCP tools to perform non-intrusive operations:

* `list_pods` / `kube_get`: Scans target app deployment namespaces to trace operational topology mappings.
* `get_pod_logs`: Validates whether the injected chaos successfully tripped expected error signatures within application code layers.

## **2.5 Outbound Event Specification ("The Red Phone Prompt Engine")**

When configured for downstream coordination, the outbound payload transforms machine telemetry into an actionable context string.

### **Outbound Webhook JSON Schema**

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "SimianIncidentNotification",
  "type": "object",
  "properties": {
    "incident_id": { "type": "string" },
    "source_fault_uid": { "type": "string" },
    "prompt_page": {
      "type": "string",
      "description": "Natural language context block passed directly to target agent input pools."
    },
    "telemetry_context": {
      "type": "object",
      "properties": {
        "namespace": { "type": "string" },
        "impacted_service": { "type": "string" },
        "observed_anomaly": { "type": "string" }
      },
      "required": ["namespace", "impacted_service", "observed_anomaly"]
    },
    "timestamp": { "type": "string", "format": "date-time" }
  },
  "required": ["incident_id", "source_fault_uid", "prompt_page", "telemetry_context", "timestamp"]
}
```

### **Prompt Construction Matrix**

Simian Agent compiles prompts using varying descriptive styles depending on configuration to stress-test target consumer interpretation:

* **Linguistic Style: Direct (Deterministic)**
  * *Output:* "CRITICAL ALARM: p99 latency on service 'paymentservice' breached baseline limits, reporting 450ms processing lag. Immediate intervention required."
* **Linguistic Style: Symptoms-Only (Exploratory)**
  * *Output:* "USER INCIDENT PAGE: Customers report checkout requests hanging endlessly at the final validation screen. Cart cache services appear online, trace downstream workflows."

## **2.6 Security & Blast-Radius Defenses**

Simian Agent enforces strict zero-trust operational safety patterns within the execution workspace:

```
                          MUTATING ATTACK VECTOR

 [ Simian Agent Core ] ----> Generates Manifests
                                   |
                                   v Applies Configuration
                      [ Dynamic Client API Constraints ]
                                   |
                  +----------------+----------------+
                  |                                 |
                  v                                 v
        Target Namespace Allowed?           System Boundary Violation?
         (e.g., `online-boutique`)          (e.g., `kube-system`)
                  |                                 |
                  v                                 v
         [ Execute Fault Injection ]       [ Operation Terminated & Logged ]
```

* **Lease Immutability:** Every Chaos Mesh resource deployed via the dynamic driver contains an unalterable duration limit (max 15 minutes) coupled with a garbage-collection manager.
* **Target Scope Jailing:** The dynamic client configuration maps exclusively to target white-listed namespaces, strictly preventing any out-of-bounds mutation commands targeting foundational elements like `kube-system` or structural control plane components.

# **Part 3: Phased Weekly Development Plan**

The updated implementation roadmap details verifiable functional milestones, validation checks, and specific capability tracking blocks for each cycle.

## **Week 1: Automated Target System Provisioner**

* **Focus:** Core infrastructure lifecycle management and clean-room target generation.
* **What Works & Functions Available:**
  * `NewGKEProvisioner(kubeconfig)`: Initializes the cluster client configuration.
  * `ProvisionNamespace(ctx, namespace)`: Creates isolated evaluation environments.
  * `VerifyReady(ctx, namespace)`: Performs status evaluations on app workloads.
  * `ProbeEndpoint(url)`: Direct HTTP availability checking of the target system.
  * `TeardownNamespace(ctx, namespace)`: Resource destruction and cleanup.
* **What to Show / How to Test:**
  * Run `simian-provisioner --action deploy`. Show a fresh, isolated `online-boutique-eval` namespace being spun up in GKE.
  * Demonstrate the orchestrator blocking until all 11 core microservice pods pass their native Kubernetes readiness gates.
  * Simulate a network drop or bad image deployment and demonstrate the provisioner throwing a clean timeout error when the application baseline is unstable.
  * Run `simian-provisioner --action cleanup` and show the namespace being completely pruned from the cluster.

## **Week 2: Chaos Mesh Deep System Driver**

* **Focus:** Native integration with low-level kernel, time, and network packet fault planes.
* **What Works & Functions Available:**
  * `NewChaosMeshDriver(config)`: Hooks into the cluster's custom dynamic resource interfaces.
  * `InjectNetworkLatency(ctx, spec)`: Instantiates precise latency objects (e.g., `v1alpha1.NetworkChaos`).
  * `InjectPacketLoss(ctx, spec)`: Simulates corrupted gray-failure environments.
  * `TerminateNetworkLatency(ctx, id)`: Drops active experiments instantly.
* **What to Show / How to Test:**
  * Execute `simian-chaos --engine mesh --fault network-delay --target paymentservice --value 250ms`.
  * Open an independent terminal and execute a `ping` or `curl` trace against the `paymentservice` cluster IP. Show that the network latency precisely shifts by 200–250ms.
  * Verify via `kubectl get networkchaos -n online-boutique-eval` that the custom resource matches the configurations generated by the Go SDK.
  * Kill the CLI process and show that the duration ceiling safety rail automatically clears the latency object after the configured threshold.

## **Week 3: LitmusChaos Application Workflow Driver**

* **Focus:** Enterprise-grade workflow scheduling and complex orchestration.
* **What Works & Functions Available:**
  * `NewLitmusDriver(config)`: Accesses stateful workflow execution paths.
  * `TriggerEngine(ctx, experiment)`: Schedules pre-defined plays directly from the Litmus Chaos Hub.
  * `TrackChaosResult(ctx, engineID)`: Pulls real-time pass/fail evaluation data from custom status fields.
* **What to Show / How to Test:**
  * Execute `simian-chaos --engine litmus --fault pod-delete --target redis-cart`.
  * Watch the Litmus ChaosEngine operator automatically spin up an ephemeral runner pod to execute a targeted eviction against the stateless data cache.
  * Show that the platform handles complex target identification (e.g., choosing a single replica out of a multi-replica deployment based on specific app label queries).

## **Week 4: GKE Managed MCP Server & Model Armor Setup**

* **Focus:** Exposing cluster observations to the SRE Agent through a secure proxy interface.
* **What Works & Functions Available:**
  * `NewModelArmorClient(endpoint, token)`: Starts the safety screening layer.
  * `ScreenPayload(ctx, command)`: Intercepts and parses commands before execution.
  * `GetJailedKubeConfig(sa)`: Outputs custom credentials bound to the restricted target namespace.
* **What to Show / How to Test:**
  * Run the mock SRE agent workspace. Execute a tool call using the GKE Managed MCP Server (e.g., `list_pods`). Show that the server returns standard JSON structure describing the `online-boutique-eval` environment.
  * Test the security guardrails: Pass a malicious command through the prompt interface (e.g., trying to execute `kubectl delete namespace kube-system` or bypassing rules to read Chaos Mesh manifests).
  * Show **Model Armor** catching the threat inline, blocking the tool call, and generating an alert payload to Cloud Logging.

## **Week 5: "Red Phone" Standardized Pager System**

* **Focus:** Bidirectional alerting mechanics supporting natural-language prompt injection and response monitoring.
* **What Works & Functions Available:**
  * `NewPagerSystem(webhook)`: Establishes communication tunnels to the SRE agent endpoint.
  * `DispatchPage(ctx, incidentID, template)`: Generates and posts the natural language challenge prompt.
  * `ListenForResponses(port)`: Actively traps incoming status update webhooks or final reports from the agent.
* **What to Show / How to Test:**
  * Trigger a simulated failure. Show the pager generating a randomized, natural-language incident page (e.g., *"URGENT: checkoutservice is dropping transactions. Latency is spiking above 2s..."*).
  * Demonstrate the **Push Flow**: Watch the Simian Agent successfully POST this text directly to the SRE Agent's intake API, waking up the agentic loop.
  * Demonstrate the **Pull Flow**: Show the SRE Agent successfully querying the cluster state via the MCP interface to identify anomalies matching the page text.
  * Have a mock agent send a status update back (e.g., *"Investigating database pods"*), and demonstrate the Simian Controller successfully logging and timestamping the message.

## **Week 6: End-to-End Evaluation Harness & Dashboard**

* **Focus:** Automated verification loops, Chain-of-Thought parsing, and score tracking.
* **What Works & Functions Available:**
  * `NewSREEvaluator(namespace)`: Orchestrates the comprehensive analytics engine.
  * `AuditChainOfThought(logs, expectedCause)`: Parses textual logs to match identified anomalies with actual injected chaos.
  * `GradeExecution(ctx, executionContext)`: Applies the formula weights to outputs to generate a score.
  * `RenderScorecard(result)`: Compiles the final evaluation details into a Markdown report.
* **What to Show / How to Test:**
  * Run a complete end-to-end task cycle automatically across a parallel matrix of environments (K=3) to demonstrate **Pass ∧ K** enforcement.
  * Simulate a **Lucky Fix** scenario: Have a mock agent resolve a network problem by blindly restarting the node, but stating in its reasoning that it suspected a disk space error. Show the grading engine rewarding the technical fix points but issuing a 0 for RCA accuracy.
  * Simulate a **Perfect Fix** scenario: Show the engine awarding a full 100/100 score, confirming that the SLO returned to healthy parameters and the agent's textual logic matched the injected error.
  * Verify the engine outputs a clean, parseable `scorecard.md` summary dashboard detailing metric splits, response timelines, and final pass/fail results.
