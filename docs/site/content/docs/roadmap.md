---
title: "Roadmap"
linkTitle: "Roadmap"
weight: 40
description: "Phased development roadmap — what's shipped, what's next."
---


> **Status:** Draft, v1 plan. M1 shipped 2026-05-12 (PR #1). M2 shipped 2026-05-12/14 (PRs #2, #5). M3 shipped 2026-05-14 (PRs #7, #8).
> **Related:** [`requirements.md`]({{< relref "requirements.md" >}}), [`design.md`]({{< relref "design.md" >}}).
> Supersedes the roadmap portion of `simian-agent.md`.

This roadmap lays out v1 in six milestones. Each milestone has a focused deliverable, a small set of public Go entrypoints, and a concrete acceptance demo. Milestones are sequenced as a vertical slice first (Milestone 1) then breadth and depth — every milestone produces a demoable system on top of the previous one. Cross-cutting work (observability, Helm chart) is interleaved — see the closing section.

> **Re-sequencing note (2026-05-12):** The original M2 (Litmus parity) was moved to M6, and the original M3 (Provisioner) was promoted to M2 and split into Arena (Part A) and SUT lifecycle (Part B). Rationale: Chaos Mesh's catalog already covers ~95% of useful primitives, so the headline value through M5 is provisioner → autonomous → page → export. Litmus stays on the roadmap as parity polish, not a critical-path engine.

## Milestone 1 — Directed Mode End-to-End on Chaos Mesh ✅ shipped

* **Goal:** A user submits a fault request — either as plain-text intent *or* by picking from the catalog — and Chaos Mesh executes it inside an existing eligible namespace.
* **Assumptions:** Eligible namespaces and target workloads already exist in the cluster, annotated `simian.chaos/eligible="true"`, with the chaos SA RoleBinding pre-installed by an operator. (Arena setup lands in Milestone 2.)
* **What shipped (the foundational vertical slice):**
  * **`LLMProvider` interface** + Gemini implementation (`gemini-2.5-pro` for translation) per `design.md §4`.
  * **`FaultExecutor`** — skeletal but functional: schema validate, safety validate (namespace eligibility, blast-radius tier, duration cap), audit log, apply via driver, lease + reaper.
  * **Chaos Mesh driver** — full generic CRD apply via dynamic client; catalog discovery enumerates installed `chaos-mesh.org/v1alpha1` CRDs and applies the static blast-radius tier map (R-FAULT-01, R-FAULT-06, R-FAULT-07).
  * **MCP server** in `simian serve` exposing the minimum tool set: `submit_fault(intent | manifest)`, `list_fault_catalog()`, `list_active_faults()`, `clear_fault()`, `get_fault_status()`.
  * **`simian chaos` CLI** — directed-mode client with both `--intent` (LLM-translated) and `--kind/--spec` (deterministic-control) input modes.
  * **In-memory `ActiveFault` registry + duration-based reaper.** Full `SimianLease` CR + crash-recovery semantics deferred.
  * **Minimal Helm chart** with chaos-SA, `Role`, and `RoleBinding` for an operator-supplied list of eligible namespaces.
* **Verified end-to-end** against a real GKE Standard cluster with Chaos Mesh + Online Boutique. Five LLM-path tests passed (PodChaos, NetworkChaos, StressChaos, namespace-not-eligible safety reject, duration-over-ceiling safety reject); kernel-level `tc -s qdisc` confirmed the netem rule was actually installed; PodChaos pod-rotation independently observable. NetworkChaos effect bypassed by GKE Dataplane V2 (Cilium) — documented as a known cluster-side caveat in `README.md`.

## Milestone 2 — Provisioner: Arena + SUT Lifecycle ✅ shipped

* **Goal:** Simian owns the target-namespace lifecycle. Drop M1's "operator pre-creates the namespace" assumption. Ships in two PRs that compose: arena setup standalone (universally useful, including v2 external posture), then SUT lifecycle on top.

### Part A — Arena setup (PR 1)

* **What ships:**
  * **`simian arena create <ns>`** — creates the namespace, applies `simian.chaos/eligible="true"` annotation, accepts optional `--annotation key=val` repeats (e.g. `simian.chaos/exclude-workloads=loadgenerator`), creates the chaos-SA `Role` + `RoleBinding` for that namespace.
  * **`simian arena destroy <ns>`** — removes the RoleBinding and the namespace, after refusing if any active Simian-managed faults are still leased there.
  * **`simian arena describe <ns>`** — read-only summary: eligibility annotation, exclusion list, RoleBinding presence, active-fault count.
  * **`ValidatingAdmissionPolicy`** backstop installed by Helm — rejects any provisioner-SA-originated namespace creation lacking the eligibility annotation, and any RoleBinding granting the chaos SA access into a non-eligible namespace.
  * **Provisioner ServiceAccount** with cluster-scoped permissions narrowly bounded to: namespaces (create/delete/get/list/watch), RoleBindings + Roles (create/delete/get/list), the admission-policy-binding for self-enforcement.
  * **Helm chart additions** — provisioner SA + ClusterRole + binding, the admission policy, and a `provisionerEnabled` value so installations that don't want it (external-posture v2 setups) can disable.
* **Acceptance demo:**
  * `simian arena create chaos-arena-1` creates an annotated namespace + chaos-SA RoleBinding scoped to it. `simian chaos --list-active --namespace chaos-arena-1` succeeds.
  * Manually attempt `kubectl create namespace foo` under the provisioner SA without the eligibility annotation — admission policy rejects.
  * Manually attempt to create a RoleBinding granting chaos SA into `kube-system` under the provisioner SA — admission policy rejects.
  * `simian arena destroy chaos-arena-1` cleans up. Re-running on a non-existent arena is idempotent.
  * `simian arena destroy` refuses if active faults are present; `--force` overrides after clearing them via the executor's `Clear`.

### Part B — SUT lifecycle (PR 2)

* **Composes Part A.** Default behavior: error if the arena doesn't exist; pass `--create-arena` to compose Part A inline.
* **What ships:**
  * **`simian sut deploy --namespace <ns> [--sut online-boutique] [--create-arena]`** — applies the SUT manifests into an existing arena (or creates the arena first if `--create-arena`), waits for steady-state, captures baseline snapshot.
  * **`simian sut destroy --namespace <ns> [--with-arena]`** — removes the SUT workloads, leaves the arena intact unless `--with-arena` (in which case it composes `arena destroy` after).
  * **`SUT registry`** — small package describing built-in SUTs by name (Online Boutique first; pluggable for future). Each SUT defines: a manifest bundle, the workload labels for baseline checking, the load-generator workload (if any), and baseline thresholds.
  * **`EstablishBaseline(ctx, namespace, sut)`** — blocks until all declared workload pods report Ready, the load generator is producing requests, and configured baseline metrics hold across a baseline window (default: error rate < 1%, p99 stable, 60s window). Emits a `Baseline` snapshot that `get_baseline()` (M3) consumes.
  * **`get_baseline()` MCP tool** — read-only, returns the cached baseline for a namespace; returns `{exists: false}` if no SUT has been deployed there.
* **Acceptance demo:**
  * `simian sut deploy --namespace chaos-arena-1 --sut online-boutique` (after `arena create` was run) — all 11 microservice pods Ready, baseline captured. `get_baseline` returns the snapshot.
  * `simian sut deploy --namespace fresh-ns --sut online-boutique --create-arena` — single command does Part A + Part B end-to-end.
  * Inject a bad image in the SUT manifest bundle; `sut deploy` returns a clean `BaselineUnstable` error within the configured timeout.
  * `simian sut destroy --namespace chaos-arena-1` removes Online Boutique workloads; arena (and its RoleBinding) remain. `arena describe` confirms.
  * `simian sut destroy --namespace fresh-ns --with-arena` removes both.

## Milestone 3 — Autonomous Mode ✅ shipped

* **Goal:** Simian can be pointed at a set of eligible namespaces and run a planning loop that drafts and executes attack plans under a budget. Plans are always emitted before execution.
* **What ships:**
  * **Topology Discoverer** — informer-backed read-only inspection per `design.md §6`.
  * **Plan Generator** — orchestrates the autonomous cycle: gather context, call `LLMProvider.Complete()` with the `AttackPlan` JSON Schema, validate, hand to executor.
  * **`AttackPlan` flow** — ordered steps with `DependsOn`, hypothesis text, per-step rationale.
  * **`simian plan` subcommand** — runs a cycle in dry-run mode (plan emitted, no apply); writes the plan as JSON for review.
  * **Budget enforcement in the executor** — max concurrent active faults, min cooldown between faults per namespace, max faults per cycle, max severity tier per cycle (R-NFR-05).
  * **Health gate** — pre-cycle baseline verification (M2 produces baseline; M3 enforces it).
  * **Read-only context MCP tools the LLM uses** — `get_topology`, `get_metrics`, `get_recent_faults` (alongside `get_baseline` from M2).
* **Acceptance demo:**
  * `simian plan --namespace chaos-arena-1` emits a JSON `AttackPlan` with rationale and hypothesis; nothing applied to the cluster.
  * `simian serve` running in autonomous mode against the same namespace executes the plan step-by-step; each step appears in the audit log with the executor's validation outcome.
  * Configure `maxConcurrentFaults=1`; plan with three independent steps respects the cap (steps serialize even if `DependsOn` graph allows parallelism).
  * Drop the LLM provider's credentials; cycle skips with a clean `LLMUnavailable` log entry; nothing applied.

## Milestone 4 — Red Phone (Outbound Event Bridge)

* **Goal:** Optional natural-language incident pages dispatched after each fault, with bidirectional listening for downstream agent responses.
* **What ships:**
  * **`LLM.GenerateIncidentPage(ctx, faultOutcome, style)`** — separate, lightweight LLM call (default `gemini-2.5-flash`).
  * **`RedPhoneDispatcher.Dispatch(ctx, page)`** — HTTP POST with `X-Simian-Signature` HMAC, bounded retry, exponential backoff.
  * **`agent_responses` MCP listener** — accepts inbound status updates from downstream agents; pipes them into the audit log and (in M5) the scenario record.
  * **Linguistic style toggle** — `direct` and `symptoms-only` configurable per cycle / per webhook.
* **Acceptance demo:**
  * Trigger any fault from M1 with Red Phone enabled; receiving mock SRE agent gets a randomized natural-language page; HMAC signature verifies.
  * Switch style to `symptoms-only`; same fault produces user-perspective framing instead of technical telemetry framing.
  * Mock agent posts a status update back; it appears in the audit log with timestamps.
  * Take the webhook endpoint offline; dispatch fails after backoff, the failure is logged + counted in metrics, but the fault remains applied (`R-PAGE-04`).

## Milestone 5 — Scenario Data Export & Evaluation Substrate

* **Goal:** Ship two halves of the evaluation regime: (A) the data contract and sinks an external harness uses to grade SRE agent behavior, and (B) the synthetic-cluster substrate (vCluster + KWOK) that lets evaluations run cheaply at scale and in isolation. Simian still does **not** grade.
* **Re-scope note (2026-05-14):** Originally scoped to just the export contract. Added the vCluster + KWOK substrate after observing that KWOK pods don't actually break — but the *signal* that they "broke" is exactly what an SRE-agent-under-test responds to, and vCluster's per-arena boundary lets multiple evaluations run in parallel without real-cluster contention. Ships in two PRs that compose: data contract first, virtual-arena substrate on top.

### Part A — Scenario record export (PR 1)

* **What ships:**
  * **`ScenarioRecord` Go type** + JSON schema published as `docs/scenario-record-schema.json` with a `SchemaVersion` field.
  * **Sinks** — `filesystem`, `gcs`, `webhook`, selectable and combinable via Helm values.
  * **Streaming feed** — `stream_scenario_events` MCP tool plus optional webhook firehose for in-flight evaluators.
  * **`simian evaluate` driver subcommand** — locates scenario records and invokes a configured external harness command against them; surfaces exit code.
  * **Reference consumer** — a small example harness (one file) that reads the schema and computes a sample metric, used to validate the export contract.
* **Acceptance demo:**
  * Run an autonomous cycle end-to-end (M3) with the filesystem sink configured; one `${scenarioID}.json` is written containing planned faults, applied faults, baseline snapshot, agent responses, and time-to-recovery if observed.
  * Stream the same cycle in real time via `stream_scenario_events`; verify each event arrives at its expected lifecycle phase.
  * Run `simian evaluate --records ./out --harness ./external-harness.sh`; the harness consumes the records, emits its own scoring artifact, and the exit code propagates.
  * Bump `SchemaVersion` to a deliberately-incompatible value; reference consumer fails closed with a clear version-mismatch error.

### Part B — Virtual-arena substrate (PR 2)

* **Composes Part A.** The `ScenarioRecord` gains an `Environment` block fingerprinting the arena (virtual flag, pod backend, KWOK node count if applicable) so downstream graders can interpret the absence of kernel-level signals correctly and normalize across runs.
* **Design boundary:** Simian does NOT take over vCluster lifecycle as a peer to its own arena CRUD. It shells out to the upstream `vcluster` CLI / Helm chart and *recognizes + exploits* the boundary. Pure-runtime use (point Simian at any vCluster's kubeconfig and it just works) remains supported with no agent code path needed.
* **What ships:**
  * **`pkg/vcluster`** — thin wrapper around `vcluster create/delete`. `simian arena create --virtual [--with-kwok] [--kwok-nodes N]` provisions a vCluster and optionally installs KWOK + a configurable fake-node count inside it; symmetric `--virtual` on `arena destroy` tears it down.
  * **`TargetTopology.Environment`** — new fields `Virtual bool`, `Backend string` (`real` | `kwok` | `kwok-in-vcluster`), `KWOKNodes int`, surfaced via `get_topology` and the planner system prompt so the LLM knows when scale plans are cheap and when kernel-level signals won't be observable.
  * **Virtual-aware tier policy** — new executor config `PermitHigherTiersWhenVirtual bool`. When set, a virtual arena's `PermittedTiers` may include `node` (and optionally `external`) without operator hand-wringing about real-cluster blast radius. The agent enforces the gate; the LLM is told *why* a higher tier is permitted here when it isn't elsewhere.
  * **`ScenarioRecord.Environment`** — the fingerprint above propagated into the exported record so harnesses can bucket runs by substrate.
  * **Reference KWOK SUT** — a synthetic SUT in `pkg/sut/kwok-microservice/` emulating a ~50-pod microservice topology with declared dependencies. Used by the Part A reference harness as a deterministic baseline that exercises Part B end-to-end without real workload cost.
* **Acceptance demo:**
  * `simian arena create eval-arena-1 --virtual --with-kwok --kwok-nodes 10` creates a vCluster with 10 KWOK nodes; `kubectl --kubeconfig <vcluster-kubeconfig> get nodes` shows them all Ready.
  * `simian sut deploy --namespace eval-arena-1 --sut kwok-microservice --use-controller` deploys ~50 fake pods (no real containers); baseline captured in seconds, not minutes.
  * `simian serve --autonomous --autonomous-namespace eval-arena-1 --max-severity-per-cycle node` runs autonomous cycles where the LLM picks node-tier actions (KernelChaos, severe StressChaos) it would never be permitted in a real arena.
  * The scenario record's `Environment` block correctly reports `Virtual: true, Backend: "kwok-in-vcluster", KWOKNodes: 10`; the reference harness reads it and tags scores accordingly.
  * `simian arena destroy eval-arena-1 --virtual` cleanly tears down the vCluster + everything inside it. Pre-existing `--virtual=false` arena CRUD is unchanged.

## Milestone 6 — Litmus Driver Parity (parity polish)

* **Goal:** Round out the chaos engine surface — same end-to-end flow, but the user can also choose `--engine litmus` and tap Litmus's distinctive primitives (workflows, probes, ChaosHub).
* **Status:** Demoted from its original M2 slot. Chaos Mesh's catalog covers the headline use cases through M5; Litmus is parity / power-user polish.
* **What ships:**
  * **Litmus driver** implementing `ChaosDriver`: generic `ChaosEngine` + workflow apply via the dynamic client.
  * **ChaosHub integration** — installed experiments enumerated from configured Litmus hubs; surfaced through `list_fault_catalog()` alongside Chaos Mesh entries (R-FAULT-10).
  * **Probe attachment** — `ProbeSpec` entries on a manifest become Litmus probe definitions; results harvested from `ChaosResult` CRs and pushed into the audit log + `ScenarioRecord` (R-FAULT-09).
  * **Workflow materialization** — when an `AttackPlan` contains multi-step Litmus sequences, the driver emits a single workflow CRD whose graph mirrors `DependsOn` (R-FAULT-08). Single-step requests use a plain `ChaosEngine`.
* **Acceptance demo:**
  * `simian chaos --engine litmus --experiment pod-delete --target redis-cart` triggers the experiment; Litmus operator spins up the runner pod; eviction observed.
  * A two-step plan with two ordered Litmus experiments materializes as one workflow CRD (verified via `kubectl get workflow`).
  * A Prometheus probe attached to a latency experiment fails when the predicted symptom doesn't appear; the failure is in the audit log + the scenario record's `ProbeResults`.

## Cross-cutting work (interleaved across milestones)

* **Observability** — Prometheus metric names from `design.md §12.1` are introduced as each component lands. A checkpoint at the end of M2 confirms metric stability before any external dashboards or alerts depend on them.
* **Helm chart + RBAC manifests** — minimal chart shipped in M1 (chaos SA + manual binding); provisioner SA + admission policy in M2 Part A; values surface for Red Phone, sinks, and budget caps as those land.
* **Audit log + structured logging** — basic in M1, extended with each new component's events. Single audit-log schema across all milestones.
* **MCP tool surface** — minimum set in M1; `get_baseline` in M2 Part B; the rest of the read-only context tools (`get_topology`, `get_metrics`, `get_recent_faults`) in M3.
* **Crash-recovery via `SimianLease` CR** — deferred from M1; lands as part of the M2 work (the Helm chart additions for the provisioner are a natural place to introduce the CRD too).

## Out of v1 (deferred)

These appear in `requirements.md` and `design.md` as out-of-v1 or open questions and are explicitly **not** on this roadmap:

* External-workload posture (real staging/production targets) — M2 Part A's arena code is the architectural enabler.
* Approval gates / change-window calendars.
* Cross-cluster orchestration.
* `ChaosArena` declarative CRD (the imperative `simian arena` CLI in M2 Part A is the v1 substitute).
* Persistent fault genealogy / cross-cycle learning.
