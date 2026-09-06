# Simian Agent

AI-native chaos engineering orchestrator for Kubernetes. **Milestone 1 shipped** (directed-mode end-to-end on Chaos Mesh). **Milestone 2** adds the provisioner — `simian arena` for namespace eligibility and RBAC, and `simian sut` for deploying / verifying the System Under Test. **Milestone 3** adds autonomous mode — the planning loop that drafts and executes attack plans against a baseline-checked arena.

> **Docs:** [Getting started](https://go-steer.github.io/simian-agent/docs/getting-started/) · [Design](https://go-steer.github.io/simian-agent/docs/design/) · [Requirements](https://go-steer.github.io/simian-agent/docs/requirements/) · [Roadmap](https://go-steer.github.io/simian-agent/docs/roadmap/)

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
- **`network-policy` engine** — partition chaos via standard `networking.k8s.io/v1` NetworkPolicy. Enforced by the CNI itself, so it works on GKE Dataplane V2 regardless of whether Chaos Mesh's NetworkChaos does. Partition only (deny ingress / egress / both for a labeled pod set); no delay / loss / jitter.
- **`envoy-fault` engine** — HTTP-layer delay + abort via an Envoy sidecar injected at SUT-deploy time. Two kinds: `EnvoyHttpDelay` and `EnvoyHttpAbort`. The driver pokes each pod's Envoy admin API (port 15000) to flip the fault filter on/off — no Kubernetes resources are created or destroyed at chaos-time.
- **Envoy injection** — `simian sut deploy` injects the Envoy sidecar + iptables init container ONLY when explicitly requested (chart default `sutInjection.envoyFaults: false`; CLI `--no-envoy-faults` is the inverted flag). Opt out per-workload at injection time with the `simian.chaos/no-envoy-injection: "true"` pod-template annotation. The topology snapshot flags injected workloads as `envoy=true` so the autonomous planner only proposes envoy-fault chaos against eligible workloads.
- **Background:** see [DPv2-compatible chaos engines](https://go-steer.github.io/simian-agent/docs/dpv2-chaos-engines/) for the full rationale (chaos-mesh#3302, cilium#19975) and design decisions.

### `kube-state` engine — declarative-state faults

The three engines above all perturb a **running dataplane**: traffic, processes,
resources. That is half the failure space, and not the half an SRE agent spends
most of its time in. A wedged rollout, an image that does not exist, a pod
nothing can schedule and a container that dies on startup are *states*, not
events, and none of them can be produced by delaying a packet.

`kube-state` produces the other half. In `synthesize` mode it applies a bundle
of objects into the arena that is born broken — nothing already running is
touched, so a baseline captured before the fault is still comparable afterwards.
Thirteen kinds, each with its own default efficacy gate:

| Kind | Produces | Gated on |
|---|---|---|
| `ImageUnresolvable` | image reference that resolves to no manifest | `ImagePullBackOff` |
| `ContainerExitLoop` | process exits non-zero on startup | `lastState` reason `Error` |
| `MemoryLimitSqueeze` | working set larger than the container's own limit | `OOMKilled` |
| `Unschedulable` | pod the scheduler cannot place | `Unschedulable` pod condition |
| `JobFailure` | Job whose pods exhaust its backoff limit | `BackoffLimitExceeded` |
| `SelectorDrift` | Service whose selector misses its own healthy pods | pods Ready **and** no endpoint addresses |
| `BackendCrashLoop` | crash-looping pods behind a Service that selects them correctly | `lastState` reason `Error` **and** every endpoint not ready |
| `UnboundClaim` | claim on a StorageClass the cluster does not have | claim `Pending` and the pod mounting it `Unschedulable` |
| `DependencyStall` | workload that serves fine and logs a failing call to something it needs | pods Ready **and** endpoints Ready **and** the line in the log |
| `PDBGridlock` | PodDisruptionBudget with no headroom, so no pod may be evicted | pods Ready **and** the budget reporting exactly `0` disruptions allowed |
| `RolloutStuck` | second revision that can never become ready, in front of one that works | `ProgressDeadlineExceeded` **and** every replica of the old revision still available |
| `CertExpiry` | mounted TLS Secret whose certificate expires in 48 hours | pods Ready **and** the certificate present in the Secret |
| `NoOp` | a workload with nothing wrong with it — the control | pods Ready |

The bundle kinds are why a fault is a *bundle* rather than one Deployment. A
Service in front of nothing, a claim that never binds and a budget that forbids
every eviction are relationships between objects, and the fault is the
relationship: `SelectorDrift` in particular is the shape that catches an agent
grading `kubectl get pods`, since every pod is Running and Ready and the traffic
is going nowhere.

`BackendCrashLoop` is `SelectorDrift` with the blame moved. The Service is
written correctly and its backends are crash-looping, so both kinds present as
one symptom — this Service is not serving — and are two different fixes. It is
also the only kind whose cause and consequence are separate objects in separate
states, which is what makes it the one a scoring run can use to ask whether a
report named the root or stopped at what a user would notice. Measured on GKE:
its EndpointSlice lists both pod addresses with `ready: false`, where
`SelectorDrift`'s lists no addresses at all, and a request to the ClusterIP is
refused rather than black-holed.

`DependencyStall` goes one further, and is the only kind where *no object is
wrong at all*. The Deployment is Available, the pods are Ready against a real
HTTP readiness probe, the Service has endpoints, and no event fired. The fault
exists only in what the workload says about itself, so the only subject that
finds it is one that read the log — which is what the `logs` probe type exists
to gate, and the discrimination this kind was added to make.

`PDBGridlock`, `RolloutStuck` and `CertExpiry` are the three where the cluster is
serving traffic correctly and something else is broken. A gridlocked budget is
invisible until someone drains a node; a wedged rollout is invisible until
someone asks which revision is live; an expiring certificate is invisible until
it expires. Each is gated on pods Ready *first*, so a gate that could pass
against a namespace where nothing came up is refused.

`RolloutStuck` is the only kind that is not born broken — a stuck rollout is a
relationship between a revision that works and one that does not, which no
single manifest expresses. Apply creates the healthy revision, waits for it to be
fully available, and only then patches in a revision that cannot start. If the
healthy revision never comes up, Apply rolls the bundle back and reports it
rather than wedging a rollout that had nothing to wedge.

`CertExpiry`'s gate is deliberately weaker than the fault. No probe type can
decode a certificate, so the gate proves the Secret landed and the pod mounted
it; that the certificate expires when the spec says is proved in the driver's
own tests against the generated DER, not in the cluster.

`NoOp` is the control, and it synthesizes a healthy workload rather than
applying nothing. An empty namespace is trivially distinguishable from a broken
one, so a control that applied nothing could be scored correctly by counting
objects instead of by diagnosing anything.

Every field of every spec is optional — `{}` produces the failure state. Needs
no Chaos Mesh and no sidecar; it does need `create` on `apps/deployments`,
`batch/jobs`, `services`, `persistentvolumeclaims`, `secrets` and
`policy/poddisruptionbudgets` in the arena Role, which `simian arena create` and
the Helm chart both grant.

```bash
simian chaos --engine kube-state --kind ImageUnresolvable --api-version apps/v1 \
  --namespace boutique-m3 --duration 5m
```

Verified on GKE 1.36.3-gke.1537000: the first four on 2026-09-04, reaching their
target state and passing their gate in 2–14s; the bundle kinds on 2026-09-05, in
0.1–2.2s except `JobFailure`, which needs 37s because the Job controller's retry
delay doubles and the Job does not admit defeat until it has run out of retries.

#### Using the new engines (deterministic-control mode)

All four engines accept `simian chaos --engine ... --kind ... --spec '<inline JSON>'`. Examples:

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

The `simian provision` command is deprecated; use `simian arena` and `simian sut` directly.

### Scoring a subject against the chaos

Simian breaks things; `pkg/eval` grades what an agent made of it.

```bash
simian evaluate --pack parity --audit run.log --report agent.json
```

- **Two artifacts, joined on the scenario ID** the audit sink stamps onto every event. The audit log is Simian's record of breaking things — which faults landed, and when. The report is the subject's side — what it found, and when. Neither half can be taken from the other.
- **Pure** — no cluster, no clock, no network. The same artifacts produce the same scorecard on any machine, hours later.
- **A vacuous pass is refused.** A scenario whose fault has no *passing* efficacy record prints as `NOT SCORED — <why>` rather than as a miss: the cluster was never broken, so a zero would mean "nothing to find" while reading as "the agent missed it". Below `--min-efficacy` (default `0.8`) the scorecard prints and the command then exits non-zero, because those numbers measure the harness and not the subject.

### Running a whole pack against a subject — `simian-eval`

`simian evaluate` scores artifacts that already exist. `simian-eval` is the
second binary that produces them: it drives a scenario pack against a subject
end to end. It is deliberately separate from `simian` — cluster provisioning,
subject processes and scoring have no business linking into the operator
binary that runs in-cluster with chaos RBAC.

```bash
simian-eval --pack parity --subject exec:./bin/lookout --out runs/
simian-eval --pack parity --subject noop: --concurrency 4       # the zero-score floor
```

Per scenario: provision an arena namespace, inject the faults **through the
normal executor path** — same validation, same safety stages, same leases, same
efficacy gates as a live run — hand the subject the prompt, collect its report,
then clear the chaos and put the namespace back.

- **Two files land in `--out`**, and they are the same two `simian evaluate`
  reads: `audit.log` is Simian's side, `run.json` is the subject's. The
  scorecard printed at the end comes from reading them back, so the run
  reproduces offline, exactly, with or without the cluster.
- **A harness failure is not a subject miss.** If an arena won't come up or an
  efficacy gate doesn't pass, the scenario records an `InjectError` and is
  `NOT SCORED`. A subject that crashes or times out, by contrast, scores a hard
  zero — a subject must not improve its mean by failing.
- **It destroys only what it created.** Namespaces the run creates are
  annotated with `simian.chaos/eval-run=<run id>` and torn down at the end,
  including on Ctrl-C. A namespace that already existed is annotated and left
  standing.
- **`--cluster kind`** stands up a throwaway cluster for the run and deletes it
  afterwards; the default `current` uses your kubeconfig and leaves it alone.

Full flag table in the [CLI reference](https://go-steer.github.io/simian-agent/docs/cli-reference/).

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
cmd/simian/        operator binary, cobra subcommands (serve, chaos, arena, sut, plan, evaluate)
cmd/simian-eval/   evaluation harness binary — runs a pack against a subject and scores it
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
pkg/eval/          offline scoring — scenario packs, measures, scorecard (no cluster, no clock)
pkg/harness/       the runner behind simian-eval: arena lifecycle, injection, artifacts
  subject/         subject adapters (exec:, noop:)
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

# Recommended starting point: layer the "fully-baked-defaults"
# overlay on top of the chart defaults. Pins a known-verified image
# tag, tightens the executor safety policy, leaves experimental
# features off. See examples/values-baked-defaults.yaml for what
# each value is doing and the maintenance contract.
helm upgrade --install simian deploy/helm/simian -n simian-system \
    --create-namespace \
    -f examples/values-baked-defaults.yaml
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

- **`NetworkChaos` on GKE Dataplane V2 (Cilium / `anetd`) is version-dependent — measure it.** Historically it did not work: Chaos Mesh installs a `netem` qdisc on the pod's `eth0`, which we verified was present at the kernel level, but Dataplane V2 routed pod-to-pod traffic through eBPF maps that bypassed the tc qdisc layer, so the latency / loss never got applied and the qdisc's `Sent ... pkt` counter stayed flat. **That no longer reproduces on current GKE** — measured 2026-09-04 on GKE 1.36.3-gke.1537000 / Cilium v1.19.4-gke.49 / Chaos Mesh v2.8.2, where both `delay` and `partition` landed. Since it can go either way per cluster, `NetworkChaos` carries a default efficacy gate: a fault the dataplane swallowed fails its probe and is rolled back rather than reported as applied. The `network-policy` (partition) and `envoy-fault` (HTTP delay + abort) engines work above the dataplane and are unaffected either way. For non-network chaos, `PodChaos` / `StressChaos` / `TimeChaos` / `IOChaos` / `JVMChaos` work fine on Dataplane V2. See [GKE bring-up](https://go-steer.github.io/simian-agent/docs/gke-bring-up/) and the [DPv2-compatible chaos engines](https://go-steer.github.io/simian-agent/docs/dpv2-chaos-engines/) doc.
- **Chaos Mesh on GKE Standard with Node Auto-Provisioning** needs (a) the `chaos-mesh` namespace to use the `cloud.google.com/default-compute-class-non-daemonset` label (not the bare `default-compute-class` one — it injects a `nodeSelector` into the chaos-daemon DaemonSet that contradicts the per-node-pod affinity), AND (b) the chaos-daemon DaemonSet template to tolerate `cloud.google.com/compute-class:NoSchedule` (operator: Exists) so it lands on every NAP-provisioned node. Without both, the daemon is missing on most nodes and `NetworkChaos`/`IOChaos` reconciliation fails with `cannot find daemonIP on node ...`.

## What's *not* shipped yet (deferred per the [roadmap](https://go-steer.github.io/simian-agent/docs/roadmap/))

- Red Phone outbound bridge (M4)
- Scenario data export, external harness driver (M5)
- Litmus driver, ChaosHub experiment catalog, probes, workflows (M6)
- Crash-recovery via `SimianLease` CR (in-memory registry today; orphan reaping on restart deferred)
- Full CRD OpenAPI schema validation (basic structural checks today; full validation lands once catalog discovery surfaces schemas)
- Live metrics provider for `get_metrics` (M3 ships a stub; Prometheus / Cloud Monitoring wiring deferred)
