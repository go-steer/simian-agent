# Simian Agent — Phased Development Roadmap

> **Status:** Draft, v1 plan.
> **Related:** [`requirements.md`](./requirements.md), [`design.md`](./design.md).
> Supersedes the roadmap portion of [`simian-agent.md`](./simian-agent.md).

This roadmap lays out v1 in six milestones. Each milestone has a focused deliverable, a small set of public Go entrypoints, and a concrete acceptance demo. Milestones are sequenced as a vertical slice first (Milestone 1) then breadth and depth — every milestone produces a demoable system on top of the previous one. Cross-cutting work (observability, Helm chart) is interleaved — see the closing section.

## Milestone 1: Directed Mode End-to-End on Chaos Mesh

* **Goal:** A user submits a fault request — either as plain-text intent *or* by picking from the catalog — and Chaos Mesh executes it inside an existing eligible namespace.
* **Assumptions:** Eligible namespaces and target workloads already exist in the cluster, annotated `simian.chaos/eligible="true"`, with the chaos SA RoleBinding pre-installed by an operator. (Provisioner work lands in Milestone 3.)
* **What ships (the foundational vertical slice):**
  * **`LLMProvider` interface** + Gemini implementation (`gemini-2.5-pro` for translation) per `design.md §4`.
  * **`FaultExecutor`** — skeletal but functional: schema validate, safety validate (namespace eligibility, blast-radius tier, duration cap), audit log, apply via driver, lease + reaper.
  * **Chaos Mesh driver** — full generic CRD apply via dynamic client; catalog discovery enumerates installed `chaos-mesh.org/v1alpha1` CRDs and applies the static blast-radius tier map (R-FAULT-01, R-FAULT-06, R-FAULT-07).
  * **MCP server** in `simian serve` exposing the minimum tool set: `submit_fault(intent | manifest)`, `list_fault_catalog()`, `list_active_faults()`, `clear_fault()`, `get_fault_status()`.
  * **`simian chaos` CLI** — directed-mode client that calls the MCP server. Supports two input modes:
    * `--intent "add 250ms latency to paymentservice for 2 minutes"` → LLM translates → `FaultManifest` → executor.
    * `--kind NetworkChaos --target paymentservice --spec @file.json` → bypass LLM, build manifest directly from catalog selection.
  * **Lease + crash recovery** via `SimianLease` CRs — controller restart adopts or reaps active faults.
  * **Minimal Helm chart** with chaos-SA, `Role`, and `RoleBinding` for an operator-supplied list of eligible namespaces.
* **Acceptance demo:**
  * Operator labels `online-boutique` namespace as eligible and binds the chaos SA into it (manual one-liner).
  * `simian chaos --intent "add 250ms latency to paymentservice for 2 minutes"` — fault appears; an external `curl` against the service shows added latency.
  * `simian chaos --kind NetworkChaos --target paymentservice --spec @latency.json` — same outcome via catalog path; the deterministic-control path that CI jobs and power users will lean on alongside the LLM-translated path.
  * Lease holds for the configured duration; fault self-clears on schedule.
  * Kill the `simian-controller` pod mid-fault; new pod's reaper clears the resource within the lease ceiling.
  * Submit `--intent "delete kube-system pods"` — executor rejects on the safety stage with `namespace-not-eligible`; LLM is never asked to translate it (or if it does, the rejection is clean).
  * Submit a `KernelChaos` manifest with the default policy (`namespace`-only) — executor rejects with `tier-not-permitted`.

## Milestone 2: Litmus Driver Parity

* **Goal:** Same end-to-end flow, but the user can also choose `--engine litmus`. Litmus's distinctive primitives — workflows, probes, ChaosHub — work end-to-end.
* **What ships:**
  * **Litmus driver** implementing `ChaosDriver`: generic `ChaosEngine` + workflow apply via the dynamic client.
  * **ChaosHub integration** — installed experiments enumerated from configured Litmus hubs; surfaced through `list_fault_catalog()` alongside Chaos Mesh entries (R-FAULT-10).
  * **Probe attachment** — `ProbeSpec` entries on a manifest become Litmus probe definitions; results harvested from `ChaosResult` CRs and pushed into the audit log (R-FAULT-09).
  * **Workflow materialization** — when a user submits a multi-step `submit_plan(plan)` whose steps target Litmus, the driver emits a single workflow CRD whose graph mirrors `DependsOn` (R-FAULT-08). Single-step requests use a plain `ChaosEngine`.
* **Acceptance demo:**
  * `simian chaos --engine litmus --experiment pod-delete --target redis-cart` triggers the experiment; Litmus operator spins up the runner pod; eviction observed.
  * A two-step plan with two ordered Litmus experiments materializes as one workflow CRD (verified via `kubectl get workflow`).
  * A Prometheus probe attached to a latency experiment fails when the predicted symptom doesn't appear; the failure is in the audit log with the probe's reason string.

## Milestone 3: Provisioner & Eligibility Lifecycle

* **Goal:** Simian can also create eligible namespaces, deploy SUTs into them, manage RBAC, and tear them down. The Milestone 1 assumption (operator pre-creates the namespace) becomes optional rather than required.
* **What ships:**
  * **`simian provision` subcommand** — creates the eligible namespace (with annotation), deploys SUT (default Online Boutique), creates the chaos-SA RoleBinding for the namespace, optionally sets exclude-workload annotations.
  * **`EstablishBaseline(ctx, namespace)`** — blocks until pods Ready and synthetic load is stable; emits a `Baseline` snapshot consumed by `get_baseline()` and the autonomous-mode health gate.
  * **`ValidatingAdmissionPolicy`** backstop — rejects any provisioner-originated namespace creation lacking the eligibility annotation, and any RoleBinding granting the chaos SA access into a non-eligible namespace (defense-in-depth on the provisioner SA).
  * **`TeardownNamespace(ctx, namespace)`** — removes namespace, RoleBinding, and any leased faults cleanly.
* **Acceptance demo:**
  * `simian provision deploy` creates a fresh `online-boutique-eval` namespace; all 11 microservice pods Ready; baseline captured.
  * Inject a bad image deployment to break baseline; provisioner returns a clean `BaselineUnstable` error within timeout.
  * `simian provision cleanup` removes namespace and RoleBinding cleanly.
  * Manual attempt to create a non-eligible-flagged namespace under the provisioner SA — admission policy rejects.

## Milestone 4: Autonomous Mode

* **Goal:** Simian can be pointed at a set of eligible namespaces and run a planning loop that drafts and executes attack plans under a budget. Plans are always emitted before execution.
* **What ships:**
  * **Topology Discoverer** — informer-backed read-only inspection per `design.md §6`.
  * **Plan Generator** — orchestrates the autonomous cycle: gather context, call `LLMProvider.Complete()` with structured-output schema, validate against `AttackPlan` JSON Schema, hand to executor.
  * **`AttackPlan` flow** — ordered steps with `DependsOn`, per-step probes (Litmus), hypothesis text.
  * **`simian plan` subcommand** — runs a cycle in dry-run mode (plan emitted, no apply); writes the plan as JSON for review.
  * **Budget enforcement in the executor** — max concurrent active faults, min cooldown between faults per namespace, max faults per cycle, max severity tier per cycle (R-NFR-05).
  * **Health gate** — pre-cycle baseline verification (Milestone 3 produces baseline; Milestone 4 enforces it).
* **Acceptance demo:**
  * `simian plan --namespace online-boutique-eval` emits a JSON `AttackPlan` with rationale and hypothesis; nothing applied to the cluster.
  * `simian serve` running in autonomous mode against the same namespace executes the plan step-by-step; each step appears in the audit log with the executor's validation outcome.
  * Configure `maxConcurrentFaults=1`; plan with three independent steps respects the cap (steps serialize even if `DependsOn` graph allows parallelism).
  * Drop the LLM provider's credentials; cycle skips with a clean `LLMUnavailable` log entry; nothing applied.

## Milestone 5: Red Phone (Outbound Event Bridge)

* **Goal:** Optional natural-language incident pages dispatched after each fault, with bidirectional listening for downstream agent responses.
* **What ships:**
  * **`LLM.GenerateIncidentPage(ctx, faultOutcome, style)`** — separate, lightweight LLM call (default `gemini-2.5-flash`).
  * **`RedPhoneDispatcher.Dispatch(ctx, page)`** — HTTP POST with `X-Simian-Signature` HMAC, bounded retry, exponential backoff.
  * **`agent_responses` MCP listener** — accepts inbound status updates from downstream agents; pipes them into the audit log and (in Milestone 6) the scenario record.
  * **Linguistic style toggle** — `direct` and `symptoms-only` configurable per cycle / per webhook.
* **Acceptance demo:**
  * Trigger any fault from Milestone 1 with Red Phone enabled; receiving mock SRE agent gets a randomized natural-language page; HMAC signature verifies.
  * Switch style to `symptoms-only`; same fault produces user-perspective framing instead of technical telemetry framing.
  * Mock agent posts a status update back; it appears in the audit log with timestamps.
  * Take the webhook endpoint offline; dispatch fails after backoff, the failure is logged + counted in metrics, but the fault remains applied (`R-PAGE-04`).

## Milestone 6: Scenario Data Export & External Harness Integration

* **Goal:** Expose the structured inputs/outputs of each chaos cycle so an external evaluation harness can grade SRE agent behavior. Simian does **not** grade.
* **What ships:**
  * **`ScenarioRecord` Go type** + JSON schema published as `docs/scenario-record-schema.json` with a `SchemaVersion` field.
  * **Sinks** — `filesystem`, `gcs`, `webhook`, selectable and combinable via Helm values.
  * **Streaming feed** — `stream_scenario_events` MCP tool plus optional webhook firehose for in-flight evaluators.
  * **`simian evaluate` driver subcommand** — locates scenario records and invokes a configured external harness command against them; surfaces exit code.
  * **Reference consumer** — a small example harness (one file) that reads the schema and computes a sample metric, used to validate the export contract.
* **Acceptance demo:**
  * Run an autonomous cycle end-to-end (Milestone 4) with the filesystem sink configured; one `${scenarioID}.json` is written containing planned faults, applied faults, baseline snapshot, probe results, agent responses, and time-to-recovery if observed.
  * Stream the same cycle in real time via `stream_scenario_events`; verify each event arrives at its expected lifecycle phase.
  * Run `simian evaluate --records ./out --harness ./external-harness.sh`; the harness consumes the records, emits its own scoring artifact, and the exit code propagates.
  * Bump `SchemaVersion` to a deliberately-incompatible value; reference consumer fails closed with a clear version-mismatch error.

## Cross-cutting work (interleaved across milestones)

* **Observability** — Prometheus metric names from `design.md §12.1` are introduced as each component lands. A checkpoint at the end of Milestone 2 confirms metric stability before any external dashboards or alerts depend on them.
* **Helm chart + RBAC manifests** — minimal chart in Milestone 1 (chaos SA + manual binding); provisioner SA + admission policy in Milestone 3; values surface for Red Phone, sinks, and budget caps as those land.
* **Audit log + structured logging** — basic in Milestone 1, extended with each new component's events. Single audit-log schema across all milestones.
* **MCP tool surface** — minimum set in Milestone 1; read-only context tools (`get_topology`, `get_metrics`, `get_baseline`, `list_recent_faults`) added as the corresponding subsystems land in Milestones 3–4.

## Out of v1 (deferred)

These appear in `requirements.md` and `design.md` as out-of-v1 or open questions and are explicitly **not** on this roadmap:

* External-workload posture (real staging/production targets).
* Approval gates / change-window calendars.
* Cross-cluster orchestration.
* `ChaosArena` declarative CRD.
* Persistent fault genealogy / cross-cycle learning.
