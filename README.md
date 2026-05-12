# Simian Agent

AI-native chaos engineering orchestrator for Kubernetes. **Milestone 1 shipped** (directed-mode end-to-end on Chaos Mesh). **Milestone 2 Part A** adds arena management — the `simian arena` CLI plus a `ValidatingAdmissionPolicy` defense-in-depth backstop on the provisioner SA.

> **Design docs:** [`docs/requirements.md`](./docs/requirements.md) · [`docs/design.md`](./docs/design.md) · [`docs/roadmap.md`](./docs/roadmap.md)

## What works today

### Arena lifecycle (M2 Part A)
- **`simian arena create <ns>`** — annotates a namespace `simian.chaos/eligible="true"` and creates the chaos-SA `Role` + `RoleBinding` for it. Idempotent on re-run; refuses to overwrite a namespace someone else owns.
- **`simian arena destroy <ns>`** — removes RoleBinding + namespace. Refuses if simian-managed chaos resources are still active (override with `--force`).
- **`simian arena describe <ns>`** — eligibility annotation, exclusion list, RoleBinding state, active-fault count.
- **`ValidatingAdmissionPolicy` backstop** — even a buggy or compromised `simian-provisioner` SA cannot create non-eligible namespaces or grant the chaos SA into namespaces that aren't arenas.
- **Annotation-driven eligibility** in `simian serve` — when `--eligible-namespace` is omitted, the controller honors `simian.chaos/eligible="true"` live (no restart needed after `simian arena create`).

### Directed-mode chaos (M1)
- **`simian serve`** — runs the Fault Executor + MCP server on port 8081 (default).
- **`simian chaos --intent "..."`** — plain-text intent → Gemini translates to a `FaultManifest` → executor validates and applies.
- **`simian chaos --kind ... --spec ...`** — deterministic-control path; bypasses LLM, builds a manifest from CLI flags.
- **`simian chaos --manifest <file>`** — submit a fully-formed manifest verbatim.
- **`simian chaos --list-active` / `--list-catalog` / `--clear <uid>`** — inspect and manage.
- **Lease + reaper** — every applied fault has a hard duration cap (default 15 min); the in-process reaper sweeps expired leases and clears the underlying CRD.
- **Safety stages** — namespace-eligibility (annotation + RBAC AND), workload exclusions, blast-radius tier policy (default permits `namespace` + `node`; `external` opt-in), duration ceiling, concurrency budget.
- **Pluggable LLM** — Gemini default (Vertex/ADC and API key both supported); stub provider for tests.
- **Audit log** — structured events at every pipeline stage, JSON-formatted via `slog`.

Five M1 components are implemented as stubs returning a clear "not implemented in M1" error: `simian plan` (M4), `simian provision deploy/cleanup` (M3), `simian evaluate` (M6).

## Quick start

```bash
# Build and test
make all

# Mark a namespace as a chaos arena (creates ns + chaos-SA RoleBinding).
bin/simian arena create chaos-arena-1
bin/simian arena describe chaos-arena-1

# Start the controller. With no --eligible-namespace flag, it honors the
# annotation set by `arena create` (live, no restart needed).
source ~/scripts/gemini.sh
bin/simian serve

# In another shell — LLM-translated path
bin/simian chaos --intent "kill one paymentservice pod in chaos-arena-1 for 30 seconds" \
                 --namespace chaos-arena-1

# Deterministic-control path
bin/simian chaos --manifest examples/network-latency-manifest.json

# Tear down the arena when done. Refuses if active faults are still present.
bin/simian arena destroy chaos-arena-1
```

## Project layout

```
cmd/simian/        single binary, cobra subcommands (serve, chaos, arena, sut, plan, evaluate)
pkg/simian/        core types and interfaces (FaultManifest, ChaosDriver, LLMProvider, …)
pkg/arena/         arena CRUD (Manager) + annotation-driven eligibility checker (M2 Part A)
pkg/executor/      Fault Executor — single chokepoint for all fault application
pkg/driver/
  chaosmesh/       generic dynamic-CRD driver for the full chaos-mesh.org/v1alpha1 catalog
  litmus/          (M6 placeholder)
pkg/llm/
  gemini/          Vertex AI + Gemini Developer API
  stub/            deterministic test double
pkg/planner/       intent translator (LLM → FaultManifest)
pkg/mcp/           MCP server with directed-mode tools
pkg/lease/         in-memory ActiveFault registry + duration-based reaper
pkg/audit/         structured event logger
pkg/catalog/       blast-radius tier classification (static map + per-spec re-classification)
internal/testutil/ fake driver + fake auditor for tests
deploy/
  manifests/       raw YAML for kubectl apply
  helm/simian/     Helm chart (chaos SA, provisioner SA + admission policy under provisioner.enabled)
examples/          example FaultManifest + spec JSON
docs/              requirements, design, roadmap
```

## Tests

```bash
# Unit tests (fast, no external deps)
go test ./...

# Gemini integration (requires Vertex/ADC or GEMINI_API_KEY)
source ~/scripts/gemini.sh
go test -tags=integration ./pkg/llm/gemini/...
```

## Verified manually

- Vertex/ADC end-to-end against `gemini-2.5-pro`: plain text + JSON structured output both pass on the integration tests.
- Binary builds + `--help` for every subcommand renders.
- Unit tests cover: blast-tier classification + per-spec escalation, lease registry + expiry, executor pipeline (happy path + 4 rejection types), translator (happy path + schema-invalid retry).
- Real-cluster smoke against GKE Standard + Chaos Mesh: catalog discovery (14 user-facing fault types), deterministic-control path round-trips a `NetworkChaos` apply (kernel-level `tc -s qdisc` confirmed `netem delay 250ms` installed on `paymentservice` eth0), explicit `--clear` and `lease.expired` reaper paths both fire. `PodChaos pod-kill` independently observable via pod rotation (`AGE=5s`, `RESTARTS=0` on the new pod).

## Known cluster-side gotchas

These bit us during M1 verification. Documenting so the next person doesn't lose 30 minutes:

- **GKE Dataplane V2 (Cilium / `anetd`) silently breaks `NetworkChaos`.** Chaos Mesh installs a `netem` qdisc on the pod's `eth0`, which we verified is present at the kernel level. But Dataplane V2 routes pod-to-pod traffic through eBPF maps that bypass the tc qdisc layer, so the latency / loss never gets applied. The `Sent ... pkt` counter on the qdisc stays flat. This is a Chaos Mesh + Cilium incompatibility, not a Simian bug. **For demos requiring observable network chaos on GKE, either provision a non-Dataplane-V2 cluster, or use `PodChaos` / `StressChaos` / `TimeChaos` / `IOChaos` / `JVMChaos` — those work fine on Dataplane V2.**
- **Chaos Mesh on GKE Standard with Node Auto-Provisioning** needs (a) the `chaos-mesh` namespace to use the `cloud.google.com/default-compute-class-non-daemonset` label (not the bare `default-compute-class` one — it injects a `nodeSelector` into the chaos-daemon DaemonSet that contradicts the per-node-pod affinity), AND (b) the chaos-daemon DaemonSet template to tolerate `cloud.google.com/compute-class:NoSchedule` (operator: Exists) so it lands on every NAP-provisioned node. Without both, the daemon is missing on most nodes and `NetworkChaos`/`IOChaos` reconciliation fails with `cannot find daemonIP on node ...`.

## What's *not* shipped yet (deferred per `docs/roadmap.md`)

- SUT lifecycle: deploy / verify / teardown of a target workload + baseline (M2 Part B)
- Autonomous mode, Plan Generator, AttackPlan flow, budget enforcement (M3)
- Red Phone outbound bridge (M4)
- Scenario data export, external harness driver (M5)
- Litmus driver, ChaosHub experiment catalog, probes, workflows (M6)
- Crash-recovery via `SimianLease` CR (M1 uses in-memory registry; orphan reaping on restart deferred)
- Full CRD OpenAPI schema validation (M1 does basic structural checks; full validation lands once catalog discovery surfaces schemas)
