# Simian Agent

AI-native chaos engineering orchestrator for Kubernetes. **Milestone 1 shipped** (directed-mode end-to-end on Chaos Mesh). **Milestone 2** adds the provisioner — `simian arena` for namespace eligibility and RBAC, and `simian sut` for deploying / verifying the System Under Test. **Milestone 3** adds autonomous mode — the planning loop that drafts and executes attack plans against a baseline-checked arena.

> **Design docs:** [`docs/requirements.md`](./docs/requirements.md) · [`docs/design.md`](./docs/design.md) · [`docs/roadmap.md`](./docs/roadmap.md)

## What works today

### Arena lifecycle (M2 Part A)
- **`simian arena create <ns>`** — annotates a namespace `simian.chaos/eligible="true"` and creates the chaos-SA `Role` + `RoleBinding` for it. Idempotent on re-run; refuses to overwrite a namespace someone else owns.
- **`simian arena destroy <ns>`** — removes RoleBinding + namespace. Refuses if simian-managed chaos resources are still active (override with `--force`).
- **`simian arena describe <ns>`** — eligibility annotation, exclusion list, RoleBinding state, active-fault count.
- **`ValidatingAdmissionPolicy` backstop** — even a buggy or compromised `simian-provisioner` SA cannot create non-eligible namespaces or grant the chaos SA into namespaces that aren't arenas.
- **Annotation-driven eligibility** in `simian serve` — when `--eligible-namespace` is omitted, the controller honors `simian.chaos/eligible="true"` live (no restart needed after `simian arena create`).

### SUT lifecycle (M2 Part B)
- **`simian sut list`** — show built-in SUTs from the registry. Online Boutique ships by default; the registry is pluggable for future SUTs.
- **`simian sut deploy --namespace <arena> [--sut online-boutique] [--create-arena]`** — apply the SUT manifests via server-side apply, wait for declared workloads to reach Ready, hold for the configured stability window, capture and cache the `Baseline`. With `--create-arena`, composes `arena create` first.
- **`simian sut destroy --namespace <arena> [--with-arena] [--force]`** — remove SUT resources; with `--with-arena`, also tear down the arena (RoleBinding + namespace).
- **`get_baseline` MCP tool** — read-only access to the controller's cached baseline; returns `{exists: false}` until M3 unifies the deploy + serve processes (today the deploy CLI's cache is local to the CLI).

### Autonomous mode (M3)
- **`simian plan --namespace <arena> [--hypothesis "..."]`** — generate an `AttackPlan` against a real arena (informer-backed topology snapshot, cached baseline, fault catalog, recent-fault history) and emit it as JSON. Default `--dry-run=true` does not apply.
- **`simian serve --autonomous --autonomous-namespace <arena> [--cycle-interval 5m]`** — run the planning loop. Each cycle: health gate (baseline cached, all baseline workloads Ready, no active simian-managed faults) → topology snapshot → `Generator.Generate` (Gemini structured output → `AttackPlan`) → bounded execution under per-cycle caps (`--max-faults-per-cycle`, `--max-severity-per-cycle`, the executor's existing `--duration-ceiling` / `--max-concurrent-faults` / `--min-cooldown`).
- **DAG-aware execution** — plan steps with `depends_on` are layered topologically; within a layer, fan-out is capped by `MaxConcurrentFaults` (set to 1 to fully serialize). Steps exceeding the severity cap are skipped with audit reason `severity-cap`.
- **LLM-down clean skip** — if the LLM is unreachable or returns schema-invalid output twice, the cycle emits `cycle.llm_unavailable` + `cycle.skipped` and applies nothing.
- **New read-only MCP tools** — `get_topology(ns)`, `get_recent_faults(ns, limit)`, `establish_baseline(ns, sut)`, plus `get_metrics` (stub until a metrics provider is wired in a later milestone).

### DPv2-compatible chaos engines (post-M3)
- **`network-policy` engine** — partition chaos via standard `networking.k8s.io/v1` NetworkPolicy. Works on GKE Dataplane V2, where Chaos Mesh's NetworkChaos is silently bypassed. Partition only (deny ingress / egress / both for a labeled pod set); no delay / loss / jitter.
- **`envoy-fault` engine** — HTTP-layer delay + abort via an Envoy sidecar injected at SUT-deploy time. Two kinds: `EnvoyHttpDelay` and `EnvoyHttpAbort`. The driver pokes each pod's Envoy admin API (port 15000) to flip the fault filter on/off — no Kubernetes resources are created or destroyed at chaos-time.
- **Envoy injection** — `simian sut deploy` injects the Envoy sidecar + iptables init container ONLY when explicitly requested (chart default `sutInjection.envoyFaults: false`; CLI `--no-envoy-faults` is the inverted flag). Opt out per-workload at injection time with the `simian.chaos/no-envoy-injection: "true"` pod-template annotation. The topology snapshot flags injected workloads as `envoy=true` so the autonomous planner only proposes envoy-fault chaos against eligible workloads.
- **Background:** see [docs/plan-dpv2-chaos-engines.md](docs/plan-dpv2-chaos-engines.md) for the full rationale (chaos-mesh#3302, cilium#19975) and design decisions.

#### Using the new engines (deterministic-control mode)

Both engines accept `simian chaos --engine ... --kind ... --spec '<inline JSON>'`. Examples:

```bash
# network-policy: 60s ingress+egress partition of cartservice
simian chaos --engine network-policy \
  --kind NetworkPolicy --api-version networking.k8s.io/v1 \
  --namespace boutique-m3 --workload cartservice --duration 60s \
  --spec '{"labelSelectors":{"app":"cartservice"},"directions":["ingress","egress"]}'

# envoy-fault: 60s 300ms delay on 100% of inbound HTTP/gRPC requests to frontend
# (requires the workload to have been deployed with --with-envoy-faults
# AND to be HTTP-probed or TCP-probed — see "Known limitation" below)
simian chaos --engine envoy-fault \
  --kind EnvoyHttpDelay --api-version simian.io/v1 \
  --namespace boutique-m3 --workload frontend --duration 60s \
  --spec '{"percentage":100,"fixed_delay_ms":300,"labelSelectors":{"app":"frontend"}}'

# envoy-fault: 60s 503 abort on 100% of inbound requests
simian chaos --engine envoy-fault \
  --kind EnvoyHttpAbort --api-version simian.io/v1 \
  --namespace boutique-m3 --workload frontend --duration 60s \
  --spec '{"percentage":100,"http_status":503,"labelSelectors":{"app":"frontend"}}'
```

For autonomous mode, the LLM has a strong bias toward Chaos Mesh's larger catalog. To exercise the new engines, pass an explicit hypothesis hint:

```bash
simian serve --autonomous --autonomous-namespace boutique-m3 \
  --hypothesis-hint "Verify alternative chaos engines work. Test network-policy
                     to partition a service, and envoy-fault for HTTP delay/abort
                     against any workload flagged envoy=true in topology."
```

#### Known limitation: Envoy injection breaks gRPC kubelet probes

**This is why the chart default is `sutInjection.envoyFaults: false`.** The current Envoy injection model intercepts ALL inbound TCP on the SUT-declared service ports via iptables PREROUTING REDIRECT to Envoy's listener (port 15006). Envoy speaks HTTP at the L7 layer; it does not understand gRPC health-probe payloads. So:

| Workload probe type | Behavior with Envoy injection |
|---|---|
| HTTP `httpGet` probes (e.g. Online Boutique `frontend`) | ✅ Works — Envoy responds to the probe |
| TCP `tcpSocket` probes (e.g. `redis-cart`) | ✅ Works — Envoy accepts the TCP handshake |
| gRPC `grpc:` probes on a redirected port (most Online Boutique services) | ❌ Probe fails → kubelet kills the container → `CrashLoopBackOff` |
| gRPC `grpc:` probes on a NON-redirected port | ✅ Works — no interception |

For Online Boutique specifically, `--with-envoy-faults` will leave 9 of 12 deployments crash-looping. Until probe rewriting (Istio's `pilot-agent` style) or an outbound-only redirect mode is implemented, only enable Envoy injection for SUTs whose probes you've audited as HTTP-only or TCP-only.

Workaround for testing envoy-fault against an arbitrary workload: deploy the SUT with `--no-envoy-faults`, then manually inject Envoy into a single test Deployment whose probes you control. See `acceptance-m3b-results.md` § "DPv2 chaos engines acceptance — round 3" for an end-to-end recipe.

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

`simian evaluate` ships as a stub until M5 (scenario data export). The `simian provision` command is deprecated; use `simian arena` and `simian sut` directly.

## Quick start

```bash
# Build and test
make all

# One-shot: create the arena, deploy Online Boutique, capture baseline.
bin/simian sut deploy --namespace boutique-1 --create-arena

# Start the controller. With no --eligible-namespace flag, it honors the
# annotation set by `arena create` (live, no restart needed).
source ~/scripts/gemini.sh
bin/simian serve

# In another shell — LLM-translated path against the freshly-deployed arena.
bin/simian chaos --intent "kill one paymentservice pod in boutique-1 for 30 seconds" \
                 --namespace boutique-1

# Deterministic-control path
bin/simian chaos --manifest examples/network-latency-manifest.json

# Tear down both layers (refuses if simian-managed faults are still leased;
# pass --force to override after clearing them via 'simian chaos --clear').
bin/simian sut destroy --namespace boutique-1 --with-arena
```

### Autonomous-mode quick start (M3)

```bash
# Set up arena + SUT, capture baseline IN the controller process so the
# autonomous loop can read it via get_baseline.
bin/simian sut deploy --namespace boutique-1 --create-arena --use-controller

# Dry-run plan: emit an AttackPlan as JSON, do NOT apply.
bin/simian plan --namespace boutique-1 --hypothesis "frontend tolerates one cartservice pod restart"

# Run the autonomous loop (every 90s; serializes at MaxConcurrentFaults=1).
bin/simian serve --autonomous --autonomous-namespace boutique-1 \
                 --cycle-interval 90s \
                 --max-faults-per-cycle 2 \
                 --max-severity-per-cycle namespace
```

For more granular control, `simian arena create/destroy/describe` and
`simian sut list/deploy/destroy` can be invoked independently.

## Project layout

```
cmd/simian/        single binary, cobra subcommands (serve, chaos, arena, sut, plan, evaluate)
pkg/simian/        core types and interfaces (FaultManifest, AttackPlan, ChaosDriver, LLMProvider, …)
pkg/arena/         arena CRUD (Manager) + annotation-driven eligibility checker (M2 Part A)
pkg/sut/           SUT lifecycle (Manager: apply manifests, wait for Ready, capture Baseline) (M2 Part B)
  onlineboutique/  built-in Online Boutique SUT (embedded manifests from upstream v0.10.2)
pkg/topology/      informer-backed read-only topology Discoverer (M3) — workloads, services, dep graph
pkg/executor/      Fault Executor — single chokepoint for all fault application + recent-faults ring (M3)
pkg/driver/
  chaosmesh/       generic dynamic-CRD driver for the full chaos-mesh.org/v1alpha1 catalog
  litmus/          (M6 placeholder)
pkg/llm/
  gemini/          Vertex AI + Gemini Developer API
  stub/            deterministic test double
pkg/planner/       LLM bridge: translate.go (intent → FaultManifest), generate.go (context → AttackPlan, M3)
pkg/loop/          autonomous-mode planning loop + health gate (M3)
pkg/mcp/           MCP server with directed-mode + autonomous-mode tools
pkg/lease/         in-memory ActiveFault registry + duration-based reaper (Reaper.OnExpire feeds M3 history)
pkg/audit/         structured event logger
pkg/catalog/       blast-radius tier classification (static map + per-spec re-classification)
internal/testutil/ fake driver + fake auditor for tests
deploy/
  manifests/       raw YAML for kubectl apply
  helm/simian/     Helm chart (chaos SA, provisioner SA + admission policy under provisioner.enabled)
examples/          example FaultManifest + spec JSON
docs/              requirements, design, roadmap
```

## Deploying to a cluster

The Helm chart in `deploy/helm/simian/` runs the controller in-cluster. It pulls the image from `ghcr.io/go-steer/simian-agent`, which is published automatically by `.github/workflows/release.yml` on each `v*` tag push.

```bash
# Default install (uses Chart.AppVersion as the image tag).
helm upgrade --install simian deploy/helm/simian -n simian-system --create-namespace

# Pin a specific published tag.
helm upgrade --install simian deploy/helm/simian -n simian-system \
    --set image.tag=v0.1.0-dev

# Enable the M3 in-controller SUT path (required for `simian sut deploy --use-controller`).
helm upgrade --install simian deploy/helm/simian -n simian-system \
    --set sutInController.enabled=true
```

For ad-hoc dev builds without cutting a release tag, push your own image:

```bash
# Build + push to your own ghcr.io path (overrides via env vars).
echo "$GITHUB_TOKEN" | docker login ghcr.io -u "$GITHUB_USER" --password-stdin
make image-push VERSION=mybranch IMAGE_NAME=myorg/simian-agent

helm upgrade --install simian deploy/helm/simian -n simian-system \
    --set image.repository=ghcr.io/myorg/simian-agent \
    --set image.tag=mybranch
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

- **GKE Dataplane V2 (Cilium / `anetd`) silently breaks `NetworkChaos`.** Chaos Mesh installs a `netem` qdisc on the pod's `eth0`, which we verified is present at the kernel level. But Dataplane V2 routes pod-to-pod traffic through eBPF maps that bypass the tc qdisc layer, so the latency / loss never gets applied. The `Sent ... pkt` counter on the qdisc stays flat. This is a Chaos Mesh + Cilium incompatibility, not a Simian bug. **Workarounds shipped:** the `network-policy` engine handles partition-style chaos (works on DPv2), and the `envoy-fault` engine handles HTTP-layer delay + abort via an injected Envoy sidecar (works on DPv2). For non-network chaos, `PodChaos` / `StressChaos` / `TimeChaos` / `IOChaos` / `JVMChaos` continue to work fine on Dataplane V2. See [docs/plan-dpv2-chaos-engines.md](docs/plan-dpv2-chaos-engines.md).
- **Chaos Mesh on GKE Standard with Node Auto-Provisioning** needs (a) the `chaos-mesh` namespace to use the `cloud.google.com/default-compute-class-non-daemonset` label (not the bare `default-compute-class` one — it injects a `nodeSelector` into the chaos-daemon DaemonSet that contradicts the per-node-pod affinity), AND (b) the chaos-daemon DaemonSet template to tolerate `cloud.google.com/compute-class:NoSchedule` (operator: Exists) so it lands on every NAP-provisioned node. Without both, the daemon is missing on most nodes and `NetworkChaos`/`IOChaos` reconciliation fails with `cannot find daemonIP on node ...`.

## What's *not* shipped yet (deferred per `docs/roadmap.md`)

- Red Phone outbound bridge (M4)
- Scenario data export, external harness driver (M5)
- Litmus driver, ChaosHub experiment catalog, probes, workflows (M6)
- Crash-recovery via `SimianLease` CR (in-memory registry today; orphan reaping on restart deferred)
- Full CRD OpenAPI schema validation (basic structural checks today; full validation lands once catalog discovery surfaces schemas)
- Live metrics provider for `get_metrics` (M3 ships a stub; Prometheus / Cloud Monitoring wiring deferred)
