---
title: "DPv2-compatible chaos engines"
linkTitle: "DPv2 chaos engines"
weight: 50
description: "Why we ship network-policy + envoy-fault engines for GKE Dataplane V2 clusters."
---


## Context

GKE Dataplane V2 (eBPF/Cilium) bypasses Chaos Mesh's NetworkChaos because chaos-daemon installs tc/netem qdiscs that the eBPF datapath never traverses (refs: chaos-mesh#3302, cilium#19975 — both open since 2022, no fix in sight). All netem-family actions plus `partition` are silently no-ops on DPv2 nodes. This means our M3 acceptance bullet "autonomous loop generates and applies a NetworkChaos delay" can succeed at the API level but produce zero real network impact — the LLM and the audit log say "applied", but the SUT's metrics never twitch.

We can't fix Chaos Mesh's NetworkChaos on DPv2 (no upstream fix; custom eBPF is forbidden on GKE DPv2 nodes). We can route around it by adding two new chaos engines that work *above* the dataplane:

1. **NetworkPolicy** (L3/L4 partition only) — uses standard `networking.k8s.io/v1` NetworkPolicy that GKE DPv2 enforces natively via Cilium. Free, fast to add, only covers the partition case.
2. **EnvoyFault** (L7 HTTP delay + abort) — injects an Envoy sidecar at SUT-deploy time with the `envoy.filters.http.fault` filter pre-wired. A new driver pokes each pod's Envoy admin API to enable/disable faults at runtime. Works on DPv2 because it operates in the pod's user-space, above the dataplane.

Adding two new engines in succession also surfaces a real friction point: today the planner system prompts (`pkg/planner/translate.go` and `pkg/planner/generate.go`) hard-code spec templates for each Chaos Mesh CRD inline. Engine discovery is already data-driven via `srv.GatherCatalog` (`pkg/mcp/server.go:329`), but the spec shapes the LLM needs to see are not. Before adding two more inline template blocks (and inevitably a third), we'll add a `SpecTemplate` field to `simian.CatalogEntry` and have the prompts iterate over the catalog to render templates dynamically. Each driver becomes the source of truth for its own spec shapes.

**Intended outcome.** After this change, an autonomous cycle on a DPv2 cluster can choose between PodChaos (works), StressChaos (works), IOChaos (works), NetworkPolicy partition (new — partition only, but actually fires), and EnvoyHttpDelay/EnvoyHttpAbort (new — L7 latency/error injection that actually fires). The planner sees all five engines via the same catalog mechanism. NetworkChaos remains in the catalog but the DPv2 caveat is documented; the planner can be steered toward the alternatives via the existing tier policy or by removing NetworkChaos from the permitted list per-installation.

---

## Phase 1 — SpecTemplate refactor (~half day)

**Why first.** Both new engines need to put spec shapes in front of the LLM. Doing this refactor now means each new driver ships its own templates and the planner stays single-source. Doing it later means three inline-template patches, then a refactor that has to retrofit four engines.

**Files modified:**
- `pkg/simian/types.go:223` — add `SpecTemplate string` field to `CatalogEntry` (raw prompt-ready text, optional; empty means no template shown).
- `pkg/driver/chaosmesh/driver.go:126` (`Catalog()` method) — populate `SpecTemplate` for each CRD using the existing canonical shapes already in `pkg/planner/translate.go:147-208`. Move those template strings into a private `chaosMeshSpecTemplates` map keyed by `ResourceKind`.
- `pkg/planner/translate.go:132` (`buildSystemPrompt`) — replace the hardcoded "Canonical Chaos Mesh spec templates" block with a loop that renders `cat[i].SpecTemplate` for each catalog entry that has one. Keep the rules block (1-9) intact.
- `pkg/planner/generate.go:199` (`buildPlanSystemPrompt`) — same change. Today this prompt has its own inlined templates (added in PR #16); they move to driver-provided.
- `pkg/planner/translate_test.go` — existing tests assert on rule wording, not template content (confirmed by exploration). No assertion changes; add one new test that confirms templates from the catalog appear in the rendered prompt.

**Key reused mechanism:** the catalog plumbing already works end-to-end. `cmd/simian/serve.go:97` registers drivers in a map; `pkg/mcp/server.go:329` (`GatherCatalog`) iterates the map and aggregates entries; both planner prompts already iterate `cat` for the engine/kind list. Adding `SpecTemplate` rendering reuses the same loop.

**Test additions:**
- `pkg/planner/translate_test.go` — assert that a stub catalog entry with `SpecTemplate: "FOO_TEMPLATE_MARKER"` causes that string to appear in the rendered system prompt.
- `pkg/driver/chaosmesh/driver_test.go` — assert that `Catalog(ctx)` returns entries with non-empty `SpecTemplate` for each known CRD.

**Backward compat:** `SpecTemplate` is optional. Drivers that don't set it (e.g. anything we forget to update) result in the prompt omitting the template section for that entry — same as if the entry didn't exist before. Litmus driver is currently catalog-empty, no migration needed.

---

## Phase 2 — NetworkPolicy partition driver (~1-2 days)

**Why second.** Smallest of the new engines, fully self-contained, validates the new-engine pattern (engine constant → driver → catalog → tier registration → serve.go wiring) before tackling the bigger Envoy work.

**Files created:**
- `pkg/driver/networkpolicy/driver.go` — implements `simian.ChaosDriver`. Apply creates a `networking.k8s.io/v1` NetworkPolicy that denies all ingress + egress to/from pods matching the manifest's target labelSelector for the manifest's duration. EngineUID = the policy's name (UUID-suffixed under `simian-np-` prefix). Clear deletes the NetworkPolicy by name. Catalog returns one entry: `kind: "NetworkPolicy"`, tier `namespace`, with a SpecTemplate documenting `{labelSelectors: {app: "<workload>"}, directions: ["ingress","egress"]}` shape.
- `pkg/driver/networkpolicy/driver_test.go` — Apply/Clear roundtrip against fake clientset; verify the generated NetworkPolicy denies all traffic to the labeled pods.

**Files modified:**
- `pkg/simian/types.go:50` — add `EngineNetworkPolicy Engine = "network-policy"` constant (hyphen-lowercase to mirror existing `chaos-mesh`).
- `pkg/catalog/tiers.go:50` (after the chaosMesh map) — add a base-tier registration for `network-policy:NetworkPolicy → TierNamespace`. Update `Classify()` to recognize the new engine.
- `cmd/simian/serve.go:97` — instantiate the new driver and add to the drivers map.

**Spec contract (what the LLM emits):**
```json
{
  "engine": "network-policy",
  "api_version": "networking.k8s.io/v1",
  "resource_kind": "NetworkPolicy",
  "spec": {
    "labelSelectors": {"app": "frontend"},
    "directions": ["ingress", "egress"]
  },
  "targets": [{"namespace": "boutique-m3", "name": "frontend"}],
  "duration": "30s"
}
```
The driver translates this into a real K8s NetworkPolicy at apply time (the LLM sees the simplified shape via the SpecTemplate; the driver hides the verbosity of `podSelector`/`policyTypes`/empty `egress: []` etc).

**Test additions:**
- Driver-level: Apply creates the policy; Clear deletes it; Catalog returns expected entry with SpecTemplate.
- Integration: `pkg/loop/loop_test.go` already exercises plans with multiple steps; add one variant whose plan steps include `EngineNetworkPolicy` and verify it lands in driver.applied.

---

## Phase 3 — Envoy fault sidecar + driver (~1 week)

**Decisions confirmed with user:**
- **Traffic capture:** iptables init container (Istio-style). NET_ADMIN required on init container; redirects outbound TCP on configured ports through Envoy.
- **Opt-out:** SUT-level `--no-envoy-faults` flag (default on) plus per-workload `simian.chaos/no-envoy-injection=true` annotation.
- **Topology awareness:** add `envoy_injected: true|false` per-workload field; planner sees it.

### 3a — SUT injection pipeline (~2 days)

**Files created:**
- `pkg/sut/envoy/inject.go` — sidecar + init-container templates (Go structs marshaled to corev1.Container). Bootstrap Envoy config has the `envoy.filters.http.fault` filter present but disabled (`abort.percentage = 0`, `delay.percentage = 0`); driver will toggle via runtime overrides in 3b. Admin API listens on port 15000 (configurable). iptables init container redirects outbound TCP on a SUT-configured port list (default `[80, 8080, 5000-9000]`) to Envoy on port 15001.
- `pkg/sut/envoy/inject_test.go` — given a Deployment unstructured, returns a mutated Deployment with sidecar + init container + `simian.chaos/envoy-injected: true` annotation on the pod template.

**Files modified:**
- `pkg/sut/manager.go:109-113` (`Deploy`) — after `splitYAML` returns the parsed unstructured docs and before `applyOne`, run each Deployment through `envoy.Inject(doc, opts)`. Skip if `opts.WithEnvoyFaults` is false OR if the Deployment has the per-workload skip annotation. Non-Deployment kinds pass through unchanged.
- `pkg/sut/manager.go` `DeployOptions` struct — add `WithEnvoyFaults bool` and `EnvoyFaultPorts []int` fields.
- `cmd/simian/sut.go:179` — add `--no-envoy-faults` flag to `simian sut deploy` (default false; CLI sets `WithEnvoyFaults = !no`). Plumb into `DeployOptions`.
- `pkg/sut/onlineboutique/onlineboutique.go` — declare `EnvoyFaultPorts: []int{8080, 7000, 7070, 50051, 3550, 5000, 9555}` (the actual Online Boutique service ports) on the SUT struct via a new optional `SUT` interface method `EnvoyFaultPorts() []int`. Default empty means "use Manager defaults".
- `deploy/helm/simian/values.yaml` and `deploy/helm/simian/templates/deployment.yaml` — add `sutInjection.envoyFaults: true` value, plumb through to controller args. (Controller-side `establish_baseline` MCP tool also needs to honor this when called via `simian sut deploy --use-controller`.)

**Test additions:**
- `pkg/sut/envoy/inject_test.go` — verify generated Deployment has the sidecar container with `simian-envoy-fault` name, the init container with `NET_ADMIN`, the admin port 15000 exposed, the Envoy bootstrap ConfigMap mounted.
- `pkg/sut/manager_test.go` — verify `Deploy` with `WithEnvoyFaults: true` injects sidecars into Deployments but leaves Services/ConfigMaps untouched, and that the per-workload skip annotation suppresses injection.

### 3b — Envoy fault driver (~3 days)

**Files created:**
- `pkg/driver/envoyfault/driver.go` — implements `simian.ChaosDriver`. Apply enumerates target pods (via the standard `targets[].labelSelector`), POSTs to each pod's Envoy admin API on `/runtime_modify` to set `http.fault.delay.fixed_delay`, `.percentage`, `http.fault.abort.http_status`, `.percentage` keys (per the EnvoyHttpDelay or EnvoyHttpAbort kind being applied). EngineUID = JSON-encoded list of `{podIP, namespace, podName}` plus the runtime keys we touched, so Clear knows what to undo. Catalog returns two entries: `EnvoyHttpDelay` and `EnvoyHttpAbort`, both tier `namespace`, each with a SpecTemplate.
- `pkg/driver/envoyfault/driver_test.go` — Apply/Clear with a fake HTTP client; verify the right runtime keys are set.

**Files modified:**
- `pkg/simian/types.go:50` — add `EngineEnvoyFault Engine = "envoy-fault"` constant.
- `pkg/catalog/tiers.go` — register tier base for `envoy-fault:EnvoyHttpDelay → TierNamespace` and `envoy-fault:EnvoyHttpAbort → TierNamespace`.
- `cmd/simian/serve.go:97` — instantiate Envoy driver, register in drivers map. The driver needs an HTTP client and a way to resolve target pods → admin endpoints; pass `clientset` and a small pod-resolver helper.

**Spec contracts:**
```json
// EnvoyHttpDelay
{
  "engine": "envoy-fault",
  "resource_kind": "EnvoyHttpDelay",
  "spec": {"percentage": 100, "fixed_delay_ms": 250, "labelSelectors": {"app": "frontend"}},
  "targets": [{"namespace": "boutique-m3", "name": "frontend"}],
  "duration": "30s"
}
// EnvoyHttpAbort
{
  "engine": "envoy-fault",
  "resource_kind": "EnvoyHttpAbort",
  "spec": {"percentage": 100, "http_status": 503, "labelSelectors": {"app": "frontend"}},
  "targets": [{"namespace": "boutique-m3", "name": "frontend"}],
  "duration": "30s"
}
```

**Test additions:** mock the Envoy admin endpoint (httptest.Server); assert correct runtime keys POSTed; assert Clear unsets them.

### 3c — Planner topology awareness (~1 day)

**Files modified:**
- `pkg/topology/topology.go` — add `EnvoyInjected bool` field to the per-workload struct. Discovery checks for the `simian.chaos/envoy-injected: true` pod-template annotation set in 3a.
- `pkg/planner/generate.go:313` (`summarizeTopology`) — when rendering workloads in the autonomous prompt, append ` envoy=true` to envoy-injected workloads.
- `pkg/driver/envoyfault/driver.go` — Catalog entries' SpecTemplate documents the precondition: "Only target workloads with `envoy=true` in the topology summary."
- `pkg/planner/generate.go` system prompt — add a rule item: "EnvoyHttpDelay and EnvoyHttpAbort require the target workload to have envoy=true in the topology snapshot. If the target lacks Envoy, choose a different fault type."

**Test additions:** topology unit test that verifies `EnvoyInjected` is set correctly given the annotation; planner test that the rendered topology summary includes `envoy=true` for matching workloads.

---

## Phase 4 — Integration, docs, smoke test (~1 day)

**Files modified:**
- `acceptance-m3b.md` — add a Phase 5 ("DPv2 alternative engines") with steps to deploy SUT with `--with-envoy-faults`, run autonomous loop, observe plans choosing NetworkPolicy or EnvoyHttpDelay, verify they actually fire (e.g., `kubectl exec` the loadgenerator and confirm latency increases).
- `README.md` — update the "Supported chaos engines" line to enumerate `chaos-mesh`, `network-policy`, `envoy-fault`. Add a one-paragraph note: "On GKE Dataplane V2, NetworkChaos is silently bypassed by the dataplane; use `network-policy` (partition only) or `envoy-fault` (HTTP-layer) instead."
- `deploy/helm/simian/values.yaml` — surface the new chart values: `sutInjection.envoyFaults`, plus any executor permitted-tier note about new engines.

**No code changes in this phase** — just integration verification and docs.

---

## Critical files to review before/during implementation

- `pkg/simian/types.go:46-52, 223-232` — Engine and CatalogEntry definitions; both extended in this plan.
- `pkg/simian/interfaces.go:38-45` — ChaosDriver interface; new engines implement this unchanged.
- `pkg/mcp/server.go:329` — `GatherCatalog`; **no changes needed**, but understand it's the single aggregation point.
- `cmd/simian/serve.go:97-99` — drivers map registration; new engines added here.
- `pkg/catalog/tiers.go:32-50` — base-tier maps; new engines registered here.
- `pkg/sut/manager.go:109-113` — SUT apply pipeline; injection hook plugs in here.
- `pkg/planner/translate.go:132-218` and `pkg/planner/generate.go:199-250` — both prompts; spec template loops added in Phase 1.

## Verification (end-to-end smoke test on cluster)

After all phases land:

```bash
# Build + deploy SUT with Envoy injection (default on)
make build
bin/simian arena create boutique-m3
bin/simian sut deploy --namespace boutique-m3 --sut online-boutique
# Verify sidecars present:
kubectl get pods -n boutique-m3 -o jsonpath='{.items[*].spec.containers[*].name}' | tr ' ' '\n' | grep simian-envoy-fault | wc -l
# Expect: 12 (one per Online Boutique workload)

# Start autonomous serve and let it run 5+ cycles
source ~/scripts/gemini.sh
bin/simian serve --eligible-namespace boutique-m3 \
  --autonomous --autonomous-namespace boutique-m3 \
  --cycle-interval 90s --max-faults-per-cycle 3 2>&1 | tee /tmp/dpv2-engines.log

# In another shell, after baseline established + 3+ cycles:
grep -E '"event":"plan.generated"' /tmp/dpv2-engines.log \
  | jq -r '.payload.steps[].manifest.engine' | sort | uniq -c
# Expect: chaos-mesh, network-policy, and/or envoy-fault all present

# Verify a network-policy fault actually fires:
kubectl get networkpolicies -n boutique-m3
# When one is active, exec into another pod and confirm traffic to the partitioned workload fails

# Verify an envoy-fault actually fires:
# When EnvoyHttpDelay is active against e.g. frontend, exec into loadgenerator and curl frontend
# Confirm response latency increased by the configured delay
```

**Unit-test verification (per phase):**
```bash
dev/tools/ci  # all checks must remain green after each phase
```

**Acceptance criteria for "done":**
1. `simian sut deploy` injects Envoy sidecars by default; per-workload annotation can opt out.
2. Autonomous loop's audit stream shows `driver.applied` events for `engine: "network-policy"` and `engine: "envoy-fault"` against Online Boutique workloads.
3. The applied chaos resources are observable in the cluster (kubectl get networkpolicies; kubectl exec curl with measurable latency change).
4. No `driver.failed` events from invalid spec.action verbs (PR #16's prompt fix carries through).
5. `pkg/planner` system prompts no longer contain hardcoded Chaos Mesh templates — they come from the driver's catalog entries.

---

## Implementation log (autonomous run, 2026-05-15)

The plan was implemented in 5 sequential PRs against `main`:

| PR | Phase | Branch | Notes |
|---|---|---|---|
| #17 | 1 — SpecTemplate refactor | `feat/spec-template-refactor` | Chaos Mesh templates moved to `pkg/driver/chaosmesh/templates.go`. Both planner prompts now iterate the catalog via `renderCatalogWithTemplates()`. |
| #18 | 2 — NetworkPolicy driver | `feat/network-policy-driver` | New `network-policy` engine, partition-only. 84.6% test coverage. |
| #19 | 3a — Envoy injection | `feat/envoy-injection-pipeline` | New `pkg/sut/envoy` package; `--no-envoy-faults` CLI flag (default off → injection on); per-workload skip annotation. 85.2% coverage. |
| #20 | 3b — Envoy fault driver | `feat/envoy-fault-driver` | New `envoy-fault` engine with `EnvoyHttpDelay` + `EnvoyHttpAbort` kinds; pokes Envoy admin /runtime_modify. 78.4% coverage. |
| #21 | 3c — Topology awareness | `feat/topology-envoy-awareness` | `Workload.EnvoyInjected` field; planner prompt rule 9 + `envoy=true` rendering. |

Phase 4 (docs + decisions) shipped in this PR.

### Decisions made during implementation

These were either left open by the original plan or surfaced as the work progressed. Recorded here so future readers can see why a particular choice exists in the code.

#### Phase 2 (NetworkPolicy driver)

- **Generated names use ULID, not GenerateName.** The K8s fake clientset does not honor `metadata.generateName`; tests would have failed. Switched to a deterministic `simian-np-<ulid>` name set client-side. Same uniqueness guarantee, friendlier to fake clients. (`pkg/driver/networkpolicy/driver.go`)
- **LLM-facing spec is simplified.** Real K8s NetworkPolicy is verbose (`podSelector` / `policyTypes` / empty `ingress: []` arrays). The driver hides this; the LLM emits `{labelSelectors, directions}` and the driver translates. Trades a little driver complexity for a much friendlier prompt template. (`pkg/driver/networkpolicy/driver.go`)

#### Phase 3a (Envoy injection)

- **Sidecar image:** `envoyproxy/envoy:v1.31-latest`. Official upstream; ~85 MB pull penalty per node.
- **Init image:** `nicolaka/netshoot:latest`. Well-known network-debug image with both `iptables-legacy` and `iptables-nft` available. ~95 MB but reliable on any cluster. Considered Istio's `proxy_init` (deprecated), Alpine + `apk add iptables` (network flake risk), and a purpose-built tiny image (yet another dep to maintain). Netshoot wins on "well-understood / works everywhere".
- **Envoy ports:** admin on 15000, inbound listener on 15006. Istio convention so anyone reading the pod manifest knows what to expect.
- **Bootstrap delivery:** per-namespace ConfigMap (`simian-envoy-bootstrap`) prepended to the SUT manifest list. One bootstrap shared across all injected pods in the namespace; `Inject()` returns the prepended slice so the Manager applies the ConfigMap before any Deployment that mounts it. Considered inline-via-`--config-yaml` (length-limit risk) and per-pod ConfigMaps (unnecessary fan-out).
- **Traffic interception is INBOUND only.** Iptables `PREROUTING REDIRECT` on the SUT-declared ports. Faults configured on the destination workload's sidecar; simpler than outbound interception which would split fault config across caller sidecars and require knowing who calls whom.
- **HTTP/2 + websocket upgrades enabled in HCM.** Online Boutique services are mostly gRPC; without `http2_protocol_options: {}` Envoy would fail those streams.
- **Default delay duration baked into bootstrap (30 s).** The driver overrides only the percentage at fault-time, not the duration — keeps the chaos-time admin call to a single key. Per-fault custom duration is supported via the optional `fixed_delay_ms` spec field which sets `fault.http.delay.fixed_duration` runtime key.
- **Per-workload opt-out via pod-template annotation.** Annotation key chosen to mirror the existing `simian.chaos/managed` namespace: `simian.chaos/no-envoy-injection`. SUT-level opt-out is the single CLI flag `--no-envoy-faults` (defaults to false → injection on, matching the chart default).
- **Online Boutique declares 10 inbound ports.** Sourced from the upstream `kubernetes-manifests.yaml` (release v0.10.2). Includes `redis-cart` port 6379 even though redis is raw TCP (not HTTP/gRPC) — Envoy's HCM filter no-ops on non-HTTP traffic, so this is harmless and avoids a future surprise if we add a TCP fault filter later.

#### Phase 3b (Envoy fault driver)

- **Direct pod-IP HTTP, not port-forward.** The driver POSTs to `http://<podIP>:15000/runtime_modify` directly. Works in-cluster (the recommended deployment). Local-serve users on workstation networks need to be on the cluster network or use port-forward; documented as a v1 limitation. Considered using the Kubernetes API server's port-forward but rejected — significantly more code and a heavier dependency surface for a documented v1 cut.
- **EngineUID encodes namespace + labelSelector + kind, not pod identities.** Pods churn during the fault; on Clear, re-resolve via the same selector and reset on whichever pods are still around. Handles rollouts gracefully. Encoded as base64(JSON) for audit safety.
- **Pod resolver filters on three conditions:** Ready + non-empty PodIP + the `simian.chaos/envoy-injected` annotation. The annotation check is defense-in-depth — without it, the driver could accidentally POST runtime overrides to a non-Envoy pod's port 15000 (almost certainly resulting in connection-refused, but the explicit check makes the intent clear).
- **Failure aggregation:** per-pod errors collected and returned as a joined error if any fail; partial success is not rolled back. Clear is naturally convergent (re-running undoes whatever Apply did).
- **5 s HTTP timeout per pod call.** Envoy admin should respond in <100 ms; 5 s leaves headroom for slow networks without hanging the executor.

#### Phase 3c (Topology awareness)

- **Annotation key duplicated as a string literal in `pkg/topology/discoverer.go`.** Importing `pkg/sut/envoy` would create an import cycle (`pkg/sut` already imports `pkg/topology` transitively via the planner). The string is defined once in `pkg/sut/envoy/bootstrap.go:InjectedAnnotation`; if it ever changes, both call sites need updating. A grep test could catch divergence — left as a TODO if the duplication becomes painful.
- **Prompt rule 9 (envoy=true precondition) is enforced in the system prompt only.** No hard validator in the planner that rejects an envoy-fault step against a non-injected workload pre-emptively — the runtime catches it at Apply (no matching pods → driver.failed). This matches the pattern of other catalog-level constraints (excluded workloads, blast tier) which are also LLM-trust + runtime-enforce. Could harden later if real-world LLM compliance turns out poor.

#### Phase 4 (this PR)

- **Helm value `sutInjection.envoyFaults`** wires through to the controller via a new `--sut-inject-envoy-faults` CLI flag (default true) on `simian serve`. The MCP `establish_baseline` tool itself stays argument-poor (just namespace + sut name); the controller's per-deploy WithEnvoyFaults preference is set once at boot via a small `envoyOptingEstablisher` adapter that overlays `WithEnvoyFaults` on every Deploy() call.
- **In-cluster smoke verification deferred to operator.** The existing M3 acceptance run (acceptance-m3b-results.md) will be re-run when an operator next has cluster time. The verification script is in the plan's Verification section above; expected outcome is `driver.applied` events for `engine: "network-policy"` and `engine: "envoy-fault"` against Online Boutique workloads, with measurable cluster-side effects (NetworkPolicy resources visible via `kubectl get`; HTTP latency increases observable via `kubectl exec curl`).
- **README updates** were minimal — added one paragraph under "What works today" pointing at the new engines, and updated the existing DPv2 caveat note to point users at the new alternatives instead of just listing what doesn't work. The full design rationale stays in this plan doc.

### Things explicitly NOT done (out of scope for this autonomous run)

- **No image-pull-secret wiring for the Envoy + netshoot images.** Both pull from public Docker Hub; private registry mirroring is a deployment-side concern, not in scope for the engine itself.
- **No NetworkPolicy support for non-LabelSelector targets.** The driver only supports `{labelSelectors: {...}}` — IPBlock-based partitions are not wired. The autonomous planner has no use for them today.
- **No ToxiProxy alternative.** The plan's Phase 3 lists Envoy as the chosen L7-fault path; ToxiProxy was a considered-and-rejected alternative. If TCP-level effects (slice / slow_close / reset) are needed later, ToxiProxy can be added as a fourth engine alongside this one — the SpecTemplate refactor in Phase 1 makes that additive.
- **No fresh `0.1.0-dev` image push.** The plan calls out that the Helm path needs a published image with the new engines for in-cluster smoke; that's an operator action, not part of the autonomous code change.
