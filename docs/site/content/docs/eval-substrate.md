---
title: "Eval substrate plan"
linkTitle: "Eval substrate (plan)"
weight: 45
description: "Plan for turning Simian into the fault plane and ground-truth source for evaluating the go-steer agentic SRE stack."
---

> **Status:** draft, 2026-08-18. Not implemented. Re-sequences [`roadmap.md`]({{< relref "roadmap.md" >}}) M5/M6 and supersedes the M5 `simian evaluate` stub. Companion to [`design.md`]({{< relref "design.md" >}}).

## 1. Purpose

Simian exists to **break clusters intelligently so that the go-steer SRE agents can be tested, evaluated, and demonstrated.**

That sentence has consequences, and most of them cut against how the code is
currently shaped:

| Role | Component |
| --- | --- |
| Agent harness | `core-agent`, `mast` |
| Deterministic detector / triage tool | `k8s-lookout` |
| Agent under test | `core-sre-agent` (and any other subject) |
| Integrated demo | `kube-agent-demo-e2e` |
| **Adversary + ground truth** | **`simian-agent`** |

Simian is the adversary. It is not the harness, it is not the detector, and it
is not the judge of whether the cluster is healthy. It is the only component
that knows, by construction, exactly what is wrong — and that knowledge is the
ground truth everything downstream is scored against.

### 1.1 The distinction that governs the whole design

Simian must verify **efficacy** and must not verify **outcome**.

* **Efficacy** — *did the fault actually land?* Is the pod genuinely in
  `CrashLoopBackOff`, is the netem qdisc genuinely installed, is the partition
  genuinely dropping packets? Simian must know this with certainty.
* **Outcome** — *did the fault matter? what broke downstream? what is the
  blast radius in user-visible terms?* This is precisely what the agent under
  test is being scored on. If Simian computes it, the experiment is
  contaminated.

This is not a theoretical hazard. `NetworkChaos` on GKE Dataplane V2 is
silently bypassed (M1 verification notes, `dpv2-chaos-engines.md`), and
`NetworkPolicy` was long a no-op under kindnet. A fault that silently does nothing
produces an eval result that reads **"the agent missed a network partition"**
when there was no network partition. That is worse than no measurement,
because it is a confident wrong number.

`core-sre-agent` already learned this the hard way. Its live harness bails
with:

```
%d/%d fixtures never manifested — the harness is broken, not the agent
```

Efficacy confirmation is therefore Phase 1, ahead of everything else.

### 1.2 Corollary: Simian stays independent of the harness

The earlier suggestion to rebuild Simian's loop on top of `mast` inverts here.
`mast` is (or hosts) the **system under test**. An adversary that shares a
prompt template, a model client, a tool-calling loop, or a K8s client library
version with the subject can fail in a correlated way and produce an eval that
passes for the wrong reason. Simian keeps its own loop.

This does not mean Simian's loop stays as it is — see §7.

## 2. The customer already exists

`core-sre-agent` contains a hand-rolled miniature of what Simian should be:

* `internal/faults/` — 1,540 LOC, 11 fixtures, each declaring injection YAML,
  settle conditions, and machine-checkable expected findings.
* `cmd/sre-eval-live/` — creates a fresh kind cluster, injects fixtures, runs
  the agent, scores four evaluators, emits a transcript.

It was written because nothing else existed. Its design decisions are correct
and are adopted wholesale below:

* **The prompt names the task, never the fault.** `checkNamespace(ns)` is the
  entire prompt. "A prompt that leaks the diagnosis turns tier 2 back into
  tier 1."
* **Settle conditions gate the agent.** "A CrashLoopBackOff takes two restarts
  and a backoff to exist at all — an agent that looks too early correctly
  reports a healthy cluster and is then scored wrong."
* **Ground truth is the machine-stable triple** `Kind` + `Name` + `Reason`,
  matched leniently: reason token sets (`ImagePullBackOff` and `ErrImagePull`
  are the same fault seconds apart), name-as-prefix (generated pod suffixes),
  and `AlsoAcceptKinds` (a finding about the Deployment stands in for one
  about the Pod). Prose is deliberately not graded.
* **Root-cause is scored separately from recall.** On `fault-sessions`, an
  agent reporting only the CrashLoop root scores the same recall as one
  reporting only the empty-Service symptom, and those are not equally good
  answers.
* **A healthy control fixture exists** (`fault-none`), because an agent that
  reports problems everywhere is not a good agent.
* **Inject failure is recorded separately from agent failure.**

### 2.1 Hard constraint: that code is not modified

`core-sre-agent/internal/faults`, `cmd/sre-eval-live`, and `evals/` are
**frozen for the purposes of this work.** They keep working exactly as they do
today, on their own fixtures, with their own kind cluster lifecycle. The
existing baselines stay re-runnable with the existing binary.

Simian therefore does **not** import them and is **not** imported by them.
Parity is established by transcription plus a checked-in equivalence matrix
(§4), not by a cross-repo Go dependency in either direction. Keeping the
dependency graph empty is the same independence argument as §1.2 — the
adversary must not share code with the subject's repo.

## 3. What Simian can reproduce today: none of it

{{% pageinfo %}}
**Updated 2026-09-06.** The analysis below is the state before the `kube-state`
engine existed, and the "zero of eleven" number is what motivated building it.
**All eleven** are now reproducible and verified against a live GKE cluster.
The last of them was `cascade`, which needed a crash-looping workload behind
its own correctly-selecting Service — a shape no kind produced, and now
`BackendCrashLoop` (#61) does.
{{% /pageinfo %}}

Simian's four engines (`chaos-mesh`, `network-policy`, `envoy-fault`, and the
unimplemented `litmus`) all **perturb a running dataplane**. Every one of the
eleven fixtures is a **declarative-state fault** — an object that is wrong, or
born wrong, in the API server.

| # | Fixture | Namespace | Ground truth (Kind / Name / representative Reason) | Severity | Reproducible with a Simian engine today? |
| --- | --- | --- | --- | --- | --- |
| 1 | `imagePull` | `fault-imagepull` | Pod / `checkout-api` / `ImagePullBackOff` | Critical | **Yes** — `kube-state` `ImageUnresolvable` |
| 2 | `crashLoop` | `fault-crashloop` | Pod / `payments-worker` / `CrashLoopBackOff` | Critical | **Yes** — `kube-state` `ContainerExitLoop`. (`PodChaos pod-failure` does not: it yields a pause-image pod, a different reason) |
| 3 | `oomKill` | `fault-oomkill` | Pod / `cache-warmer` / `OOMKilled` | Critical | **Yes** — `kube-state` `MemoryLimitSqueeze`, which brings its own limit. (`StressChaos` does not: it needs a pre-set limit to OOM against) |
| 4 | `unschedulable` | `fault-unschedulable` | Pod / `analytics-etl` / `Unschedulable`, `FailedScheduling` | Critical | **Yes** — `kube-state` `Unschedulable` |
| 5 | `failedJob` | `fault-failedjob` | Job / `nightly-report` / `BackoffLimitExceeded` | Warning | **Yes** — `kube-state` `JobFailure` |
| 6 | `serviceSelectorMismatch` | `fault-badselector` | Service / `frontend` / `NoEndpoints` | Warning | **Yes** — `kube-state` `SelectorDrift` |
| 7 | `healthy` | `fault-none` | *(none — control)* | OK | **Yes** — `kube-state` `NoOp`, which synthesizes a *healthy* workload rather than applying nothing, so the control is not scoreable by counting objects |
| 8 | `multipleFaults` | `fault-storefront` | 3 findings: `orders-api` imagepull, `recommendation-etl` unschedulable, `inventory-sync` job failure | Critical | **Yes** — three faults in one scenario, all three kinds now exist |
| 9 | `cascade` | `fault-sessions` | Pod / `session-store` / `CrashLoopBackOff` **(root)** → Service / `session-store` / `NoEndpoints` | Critical | **Yes** — `kube-state` `BackendCrashLoop`, the crash loop *behind its own Service*, so the missing endpoints are a consequence and not the fault |
| 10 | `unboundVolume` | `fault-ledger` | PersistentVolumeClaim / `ledger-data` / `VolumeBindingFailed` | Critical | **Yes** — `kube-state` `UnboundClaim` |
| 11 | `silentFailure` | `fault-invoicing` | Pod / `invoice-reconciler` / `DependencyFailure` — Deployment Available, Service has endpoints, every broad check reports clean | Critical | **Yes** — `kube-state` `DependencyStall`, gated through the `logs` probe type |

**Zero of eleven** when this was written, and the single most useful fact in
the document. It was not an indictment of the engines — Chaos Mesh is excellent
at what it does — it was a statement that Simian had been building one half of
the fault space and the eval rig needed the other half first. Deliverable A
below is the answer, and eleven of eleven fixtures are reproducible today.

The last one to close is the one worth naming. `cascade` is the only fixture
whose ground truth distinguishes a **root** from a symptom, so it is the only
one that can ask whether a subject stopped at what a user noticed. Reproducing
it took a thirteenth kind rather than a composition of two existing ones,
because the point is precisely that the two findings are *not* independent.

Conversely, `internal/faults` structurally cannot produce anything Simian's
existing engines are good at. Applying YAML cannot create latency, packet
loss, DNS blackholes, clock skew, IO stalls, or L7 aborts. Those are the
failures where **the symptom is not where the fault is**, which is the class
that separates a real diagnostic agent from a `kubectl get pods` wrapper — and
there is currently no way to put one in front of the agents at all.

So the two halves are complementary, and Simian should own both.

## 4. Deliverable A — the `kube-state` engine

*Shipped for `synthesize` mode and all thirteen kinds (#56, #57, #58, #61). `mutate`
mode is #59 and the node-level kinds are the second half of #58; the driver
rejects `mode: mutate` with an explanatory error rather than ignoring the
field.*

A fifth driver, `EngineKubeState Engine = "kube-state"`, that produces
declarative-state faults. It slots in behind the existing `ChaosDriver`
interface and the existing executor chokepoint — no new privileged path, same
validation, same lease, same reaper.

It operates in two modes:

**Mode `synthesize`** — apply a self-contained bundle that is born broken, into
a namespace Simian created. Byte-for-byte the shape `internal/faults` uses.
This is the parity mode: deterministic, no dependency on what is already
running, and directly comparable to existing baselines.

**Mode `mutate`** — patch an existing healthy workload in the arena so that it
becomes broken, recording the original for revert. This is the strategic mode:
it is what lets Simian break *Online Boutique*, or a customer's real
namespace, rather than a synthetic `busybox` stand-in — and it is the mode
topology-driven generation (§7) needs.

Both modes go through `Apply`/`Clear` and the lease registry, so `mutate`
reverts on TTL expiry the same way a Chaos Mesh CR is deleted.

### 4.1 Fault kinds

| Kind | Mechanism | Produces | Parity with |
| --- | --- | --- | --- |
| `ImageUnresolvable` | patch container image to an unresolvable-but-real-host reference | `ImagePullBackOff` / `ErrImagePull` | 1, 8 |
| `ContainerExitLoop` | patch command/args to exit non-zero | `CrashLoopBackOff` | 2 |
| `MemoryLimitSqueeze` | patch `resources.limits.memory` below working set | `OOMKilled` | 3 |
| `Unschedulable` | patch `resources.requests.cpu` beyond cluster capacity, or an unsatisfiable `nodeSelector` | `Pending` + `FailedScheduling` | 4, 8 |
| `JobFailure` | create/patch a Job that exhausts `backoffLimit` | `BackoffLimitExceeded` | 5, 8 |
| `SelectorDrift` | patch `Service.spec.selector` off the workload's labels | empty EndpointSlice | 6 |
| `BackendCrashLoop` | synthesize a crash-looping workload and a Service whose selector *matches* it | EndpointSlice listing the pods with `ready: false`; a root and a symptom in two objects | 9 |
| `UnboundClaim` | PVC referencing a nonexistent StorageClass, plus a consumer pod | `VolumeBindingFailed` / unbound | 10 |
| `DependencyStall` | synthesize a workload that serves real HTTP and logs a failing upstream call while staying Ready and Available | log-only signal, all field checks clean | 11 |
| `PDBGridlock` | a PodDisruptionBudget whose `minAvailable` equals the replica count | `disruptionsAllowed: 0`; every eviction returns 429 | — |
| `RolloutStuck` | bring up a working revision, wait for it, then patch in one that cannot start | `ProgressDeadlineExceeded` with the previous revision still fully available | — |
| `CertExpiry` | synthesize a `kubernetes.io/tls` Secret whose certificate expires within hours, and mount it | a healthy workload serving a certificate about to expire | — |
| `NoOp` | applies nothing; still leases and audits | healthy control | 7 |

`NoOp` is not a curiosity. It is how the eval measures false positives, and it
must flow through the identical code path so that nothing about the run
distinguishes it from a real fault.

`DependencyStall` is the hardest and the most valuable, and it shipped in
`synthesize` mode with the `logs` probe type that gates it. Getting it right in
`mutate` mode against a real SUT is the difference between a rig that grades
`kubectl` transcription and one that grades diagnosis.

### 4.2 Reuse

`pkg/sut/manager.go:330` (`applyOne`) already does dynamic-client server-side
apply, and `pkg/sut/envoy/inject.go` already does deployment mutation with
revert. The driver is assembly, not invention.

## 5. Deliverable B — efficacy confirmation

> **Shipped.** `Mode: "Settle"`, the `k8s` probe type, the `Apply` gate and the
> `fault.efficacy` audit event are implemented — see
> [Efficacy probes]({{< relref "efficacy-probes.md" >}}) for the user-facing
> reference. The `http` and `logs` types have since shipped too; `cmd` is
> deliberately not implemented and `prometheus` remains future work.

`FaultManifest.Probes []ProbeSpec` becomes the settle/efficacy mechanism.

```go
type ProbeSpec struct {
    Name string         // "pods report an image pull failure"
    Type string         // k8s | cmd | http | prometheus
    Mode string         // Settle | SOT | EOT | Edge | Continuous | OnChaos
    Spec map[string]any // jsonpath, expect-contains / expect-empty, timeout
}
```

Adding `Mode: "Settle"` gives us `internal/faults`' `Condition` with a
superset of its expressiveness — the k8s type covers every existing fixture's
`kubectl get -o jsonpath` poll, and the `cmd`/`http` types cover dataplane
faults where the proof is `tc -s qdisc` output or an observed latency
percentile rather than an object field.

Behaviour:

* `Apply` returns only after every `Settle` probe passes, or fails with a new
  typed error when one times out.
* A new audit event `fault.efficacy` records pass/fail per probe with the
  observed value. **This is the recording that keeps the dataset honest** — an
  eval result whose fault has no passing efficacy record is not a data point,
  it is a harness bug, and it must be reported as such rather than averaged in.
* Every dataplane fault kind gets a probe. `NetworkChaos` on DPv2 then fails
  loudly at inject time instead of silently producing a false negative months
  later.

## 6. Deliverable C — scenarios, ground truth, and the runner

### 6.1 The `Scenario` type

A fixture is not one fault — `multipleFaults` is three and `cascade` is one
fault with two graded findings. So ground truth attaches to a scenario, not a
manifest:

```go
type Scenario struct {
    ID       string             // stamped into every audit event; the join key
    Name     string             // "cascade", "latency-not-saturation"
    Prompt   string             // names the task, never the fault
    Faults   []FaultManifest    // each carrying Settle probes
    Expect   []ExpectedFinding
    Severity string             // scenario-level expected severity
    Source   string             // "pack:parity" | "generated:topology"
}

type ExpectedFinding struct {
    Kind            string
    Name            string   // matched as a prefix
    Reasons         []string // any one counts; empty means ungraded
    AlsoAcceptKinds []string
    MinSeverity     string
    Root            bool     // root cause vs downstream symptom
}
```

`ExpectedFinding` is deliberately field-for-field `faults.Want`. Same matching
semantics, same tolerances, so numbers from the two rigs are comparable
without a translation argument.

**`simian.AuditEvent.ScenarioID` was defined and stamped by the audit sink long
before anything populated it.** `Scenario.ID` populates it. That one field
joins Simian's audit log, k8s-lookout's findings, and the agent's transcript
into a single correlatable record, which is the mechanical prerequisite for
"see what we did and did not detect, did and did not fix."

It is carried in the **context**, not passed as an argument. Simian emits audit
events from around thirty call sites across the executor, the autonomous loop
and the lease reaper; threading a parameter through all of them is churn that
the next new call site silently forgets, and a missing join key is invisible
until someone tries to score a run and finds a hole in it. `audit.WithScenarioID`
puts the ID on the context and the sink stamps every event that does not
already carry one. An event that sets the ID explicitly wins, which is how the
lease reaper — outliving the context that applied the fault — still attributes
a late expiry to the right scenario.

### 6.2 The packs

`pkg/scenario/packs/parity/` — the eleven scenarios from §3, transcribed. Plus
a checked-in equivalence matrix and a test asserting that the transcription
still matches the upstream declarations, so drift is caught by CI rather than
by a confusing baseline delta.

`pkg/scenario/packs/lookout/` — k8s-lookout's `examples/scenarios/` is a
**third** fixture corpus, ten scenarios each with `inject`/`verify`/`revert`.
Six overlap the parity set (`crashloop`, `oom`, `image-pull`,
`pending`≈`unschedulable`, `endpoints-empty`≈`badselector`,
`failed-mount`≈`unboundVolume`) and four do not:

| Scenario | Adds a `kube-state` fault kind |
| --- | --- |
| `node-failure` | `NodeUnready` — cordon/stop, the only multi-node fixture |
| `pdb-gridlock` | `PDBGridlock` — a PDB that makes eviction impossible |
| `cert-expiry` | `CertExpiry` — a Secret holding an expired certificate |
| `bad-rollout` | `RolloutStuck` — a Deployment wedged mid-rollout |

Those four are worth having independently of lookout: `pdb-gridlock` and
`bad-rollout` in particular are failure modes an operator meets constantly and
that neither of the other two corpora contains.

Then `pkg/scenario/packs/dataplane/` — the scenarios `internal/faults` cannot
express. First five, chosen because each has a symptom that appears somewhere
other than the fault:

| Scenario | Fault | Why it is hard |
| --- | --- | --- |
| `latency-not-saturation` | Envoy L7 latency on one upstream | Every resource metric is green; the symptom is p99 on a *caller* |
| `abort-503-not-a-bug` | Envoy synthetic aborts | Looks like an application bug in the wrong service |
| `partition-one-way` | NetworkChaos/NetworkPolicy asymmetric | Both ends look healthy in isolation |
| `dns-blackhole-partial` | DNSChaos on one name | Intermittent, and the failing pod is not the misconfigured one |
| `stress-real` | StressChaos CPU | The matched-pair control for `latency-not-saturation` — same symptom, different cause |

`stress-real` and `latency-not-saturation` as a matched pair is the point: an
agent that says "it's slow, scale it up" scores well on one and badly on the
other, and no fixture set that only contains one of them can tell.

### 6.3 `cmd/simian-eval`

A **second binary**, alongside `cmd/simian`. Rationale: cluster-lifecycle
management, subject adapters, and scoring have no business linking into the
operator binary that runs in-cluster with chaos RBAC, and mirroring
`cmd/sre-eval-live`'s shape keeps the two rigs recognisable to the same reader.

```
simian-eval \
  --pack packs/parity,packs/dataplane \
  --subject exec:./bin/sre-agent \
  --cluster kind \
  --out runs/2026-08-18/
```

Flow, per scenario:

1. Provision the arena (and SUT, for `mutate`-mode scenarios).
2. Inject via the **normal executor path** — same validation, same audit, same
   leases. The eval must not have a privileged back door, or it stops
   measuring the product.
3. Gate on efficacy probes. Failure here is `InjectError`, reported separately
   and never scored as an agent miss.
4. Hand the subject the prompt.
5. Collect the report; score.
6. Watch for external remediation (§6.5); revert; verify reverted.

**What "fresh cluster" means in practice.** Fresh *arena* per scenario, not a
fresh cluster: a kind cluster takes minutes to stand up and a pack has dozens
of scenarios. `--cluster kind` stands one throwaway cluster up for the whole
run and deletes it afterwards, including on Ctrl-C; `--cluster current` (the
default) uses the kubeconfig's cluster and leaves it standing. The isolation
scenarios actually need is namespace isolation, and that is enforced rather
than assumed: a scenario holds every namespace it touches for its whole
lifetime, so two scenarios never share one, and a control — which names no
namespace — takes the whole cluster to itself. A control running beside a live
fault would see real breakage, report it correctly, and be scored as having
hallucinated it.

**It destroys only what it created.** A scenario naming a namespace that
already exists gets that namespace annotated as an arena and left standing at
the end, with a log line saying so. A rig that deletes namespaces it merely
found is one bad scenario file away from deleting something that mattered.

**Both artifacts, written as it goes.** `audit.log` is opened before the
cluster is touched, and the scorecard printed at the end is produced by reading
the two files back through the same `pkg/eval` code `simian evaluate` uses
(§6.6) — not from the runs still in memory. If the artifacts could not be
scored tomorrow, the run finds out now, while the cluster is still there to
look at. Every scenario the harness attempts emits an `eval.scenario_started`
line, so a scenario that failed before any fault event existed is still in the
log the join runs against: the offline read reports a harness failure rather
than a corrupt pair of files.

**Refusals that happen before a cluster is touched.** An `--only` ID that is
not in the pack (a typo that silently grades nothing is how a suite comes back
green having measured nothing); a fault shorter than `--subject-timeout`,
because the lease expires mid-investigation, the reaper clears it, and the
harness records that disappearance as the subject having remediated a fault it
never touched — `--allow-short-faults` accepts the measurement out loud.

### 6.4 The subject seam

Simian must not import `mast`, `core-agent`, or `core-sre-agent`.

```go
type Subject interface {
    Name() string
    Investigate(ctx context.Context, prompt string) (Report, error)
}
```

Adapters:

* **`exec:`** ✅ — run a binary, read a JSON report on stdout. Covers
  `core-sre-agent`, `mast` workload bundles, `claude -p`, `gemini-cli`, and a
  shell script. Built first; it covers everything that matters.
* **`noop:`** ✅ — the null subject: reports nothing, ever. The zero-score floor
  a scorecard is read against, and the cheapest way to find out whether a pack
  actually manifests before an agent is pointed at it.
* **`http:`** — REST + SSE. Covers `mast-web` and, notably, ChaosBlade's Blade
  AI, which turns a competitor into a benchmarkable subject.
* **`mcp:`** — for subjects that expose themselves as tools.

`Report` mirrors the machine-stable triple and nothing else. The `exec`
adapter translates `core-sre-agent`'s `schema.HealthReport` into it; that
translation is ~30 lines and lives on Simian's side of the fence.

The `exec:` adapter hands the prompt over three ways at once — on stdin, in
`$SIMIAN_PROMPT`, and substituted for a `{prompt}` placeholder in the argv if
one is there — so a subject can be a Go binary, a shell one-liner or an agent
CLI without a wrapper script in between. It reads the **last** JSON object on
stdout, which is what lets a subject narrate: agents print reasoning as they
go, and requiring clean stdout would mean grading whichever tool happened to
be quiet. A subject that exits non-zero, prints nothing parseable, or runs past
`--subject-timeout` is a `SubjectError` — scored as a hard zero, never skipped,
because a subject must not be able to improve its mean by crashing.

### 6.5 Scoring

Deliberately the same four measures `core-sre-agent/evals` uses, so the
numbers are comparable, plus three the adversary is uniquely positioned to
provide:

| Measure | Source |
| --- | --- |
| Recall | expected findings matched |
| Root cause | did the report name the root, not just the symptom |
| Severity | distance, not exact match; the direction is in the comment |
| Hallucinated fault | claiming a concrete failure mode that was not injected |
| **Time to detect** | injection timestamp is Simian's, not inferred |
| **Time to remediate** | the reaper finding a fault already cleared is not an error — it is the agent having fixed it, timestamped |
| **Efficacy rate** | fraction of scenarios that actually manifested; the harness's own report card |

The fourth measure was drafted as general precision — "findings outside ground
truth" — and shipped as something narrower, because general precision is the
wrong metric here. A scenario's manifests are minimal: no liveness probes, no
PDBs, no resource limits on the deliberately-broken workloads. A subject that
notes those is *correct*, and precision would mark it down for thoroughness,
which pushes us to prompt subjects to say less. So exactly one class of finding
is charged: claiming one of the concrete failure modes the fault kinds know how
to inject, in a scenario that did not inject it. Calling a `Pending` pod a
`CrashLoopBackOff` is a misdiagnosis; noting that it also has no resource limits
is not. That is what makes a healthy control cost something, and it matches what
`core-sre-agent` actually scores.

The vocabulary of concrete failure modes is ported from the agent rig rather
than re-derived, exclusions included. Most of those exclusions were paid for
with a live run that scored an honest report as an invention: `Unschedulable`
and `FailedScheduling` say scheduling failed and not why, `NotReady` is written
about pods, containers and nodes alike, and `DeadlineExceeded` is a substring of
what the Deployment controller writes on a stalled rollout. Matching is on the
exact normalized token in consequence — a bare family member must not annex
every longer token containing it.

Time-to-remediate falls out for free and is worth saying plainly: the lease
reaper, built to stop Simian leaking faults, becomes a measuring instrument
the moment the subject is allowed to write.

Scoring is pure. Nothing in `pkg/eval` touches a cluster, a clock or a network:
a `Run` carries the expectations, the report, the timestamps and whether the
fault landed, so the same inputs always produce the same scores. That is what
lets `simian evaluate` (§6.6) reproduce a live run's numbers offline, hours
later, from artifacts alone.

### 6.6 `simian evaluate`

The M5 stub already describes itself as *"Drive an external evaluation harness
against scenario records"* — the intent was right, it just predates knowing
what the harness and the records were. It becomes the **offline** scorer: read
an audit log plus a subject report, emit the scorecard. No cluster lifecycle,
no subject execution. `simian-eval` orchestrates and calls the same
`pkg/eval` code. This is what makes the scorecard usable in
`kube-agent-demo-e2e`, where the clusters are long-lived and nobody is going
to run a kind harness.

```
simian evaluate --pack packs/parity --audit run.log --report agent.json
```

**Two artifacts, split along the line of who observed what.** The audit log is
Simian's record of breaking things: which faults applied, whether their
efficacy gates passed, and when. None of it can be taken from the subject —
the whole point of the gate is that the harness does not take the subject's
word for the cluster's state. The report is the subject's side: what it found,
when the report came back, and on a write-enabled run, when the fault was
observed gone. They join on the `ScenarioID` that `pkg/audit` stamps onto every
event, which is the reason the reconstruction is possible at all.

**Absence of evidence is not manifestation.** A scenario counts as manifested
only when *every* one of its faults has a passing efficacy record and no
failing one. An applied fault with no efficacy record at all is not a fault
that landed — it is a fault nobody checked, and every score built on it would
be a confident number about a cluster whose state is unknown. Those scenarios
render as `NOT SCORED — <why>` rather than as a row of zeros, and are excluded
from every mean. The strictness extends to partial cascades: half an incident
is not the incident the expectations describe, so grading against it would
score the subject on ground truth that was never true.

A healthy control leaves exactly the same trace as a scenario nobody injected,
and only the pack can tell them apart — which is why the join takes the pack.
A control that reached the subject at all has done its job, and its measures
must be scored: measuring invention is the only reason controls are in the
pack.

**The harness reports before the subject does.** The scorecard puts the
efficacy rate above any measure, and below `--min-efficacy` (0.8 by default)
it prints the numbers, says they are *unmeasured rather than poor*, and exits
non-zero. Reported first and refused second, because the rows that failed to
inject are the ones that explain the refusal. A rig with a known-flaky gate can
lower the bar, but has to say so on the command line rather than getting the
numbers by default.

### 6.7 End-to-end with k8s-lookout — the rig's own control

k8s-lookout should be **subject number one**, before any agent. Not as a
courtesy to a sibling repo — because a deterministic subject is the only way
to calibrate the instrument.

An LLM agent's score moves for three reasons: the fault, the agent, and
sampling noise. With lookout there is no third term. Run the same scenario
twice and get two different scores and the harness is broken; that is a test
you cannot run with any agent subject. `core-sre-agent` already reaches for
this — its `-bounded` flag "swaps the producer: the same fixtures and the same
four evaluators, scoring `internal/bounded` instead of the agent." Same idea,
one repo over.

It is also nearly free. `lookout health --format=json` already emits the §4.2
finding stream, and `emit.Finding` already carries the fields ground truth is
keyed on:

| `emit.Finding` | `ExpectedFinding` |
| --- | --- |
| `KindOfObject` | `Kind` |
| `Name` | `Name` (prefix-matched) |
| `Reason` (already canonicalised by `engine.CanonicalReason`) | `Reasons` |
| `Severity` | `MinSeverity` |
| `Kind` (check kind, e.g. `pod.crashloop`) | *stronger key than the object kind* |
| `Fingerprint` (incident-class hash) | *exact match, no token-set fuzz needed* |

The `exec:` adapter for lookout is a subprocess call and a field rename.
Against an agent we must match reason tokens leniently because an agent writes
prose; against lookout we can assert on `Fingerprint` and get an exact,
unarguable answer.

**What the e2e measures, in both directions:**

* *Simian is wrong* — lookout does not see a fault Simian believes it
  injected. Either the fault never landed (an efficacy bug: §5 should have
  caught it, and did not) or the ground truth is mislabelled. Both are Simian
  bugs and both must be found before any agent number is trustworthy.
* *Lookout is wrong* — Simian injected a fault, efficacy probes confirm it
  landed, and lookout reports the namespace clean. That is a genuine detector
  coverage gap, filed against lookout, discovered by a rig built for something
  else. `silentFailure` and the dataplane pack are where this will happen:
  lookout's `graphfeed` watches pods, nodes and ReplicaSets, and `netprobe`
  probes from the operator's vantage rather than from inside the mesh.

**And it sets the floor for every agent score.** Lookout is the baseline row
of the scorecard. An agent that has lookout as a tool and scores *below*
lookout on a scenario is not failing to diagnose — it is failing to use its
tools, which is a completely different bug with a completely different fix.
Without the lookout row you cannot tell those apart. This is the single most
actionable number the rig can produce, and it costs one subprocess adapter.

The e2e runs in CI as `simian-eval --pack lookout --subject exec:lookout`,
on kind, on a schedule — deliberately mirroring lookout's own
`.github/workflows/e2e-kind.yml` (push-to-main smoke, weekly full) so the two
repos' cluster jobs stay recognisably the same shape.

## 7. Deliverable D — the intelligence

Everything above is a better fixture corpus. This is the part that is
genuinely Simian's and that no static corpus can do.

`pkg/topology` already builds an informer-backed dependency graph. Today it is
serialised into a prompt string at `pkg/planner/generate.go:258` and the model
is asked to be creative. That is prompt filler, not reasoning over a graph.

Make it load-bearing:

* **Structural target selection.** Compute the interesting properties from the
  graph rather than hoping the model infers them from a blob: chokepoints
  (high fan-in, no alternate path), single-replica services on a critical
  path, workloads with no PDB, Services whose selector matches exactly one
  ReplicaSet, cross-namespace edges. Then choose faults whose symptom lands
  somewhere other than the target.
* **Generate whole `Scenario`s, not faults.** The generator's output must
  include the settle probe and the expected findings, or it cannot be scored —
  and this is tractable precisely because the generator chose the fault. It
  knows the ground truth by construction. It must also produce the
  task-shaped prompt without leaking the diagnosis, which is `checkNamespace`'s
  discipline stated as a generation constraint.
* **Adversarial curriculum.** Track which fault classes the stack misses and
  generate more of those. This is the payoff and the thing a fixed corpus
  structurally cannot do: a corpus tells you your score, a curriculum finds
  your blind spots.

This is also where the loop finally needs to be tool-using rather than
single-shot. `CompletionRequest.Tools` and `CompletionResponse.ToolCalls`
already exist in the interface and are already marshalled by the Gemini
provider (`pkg/llm/gemini/gemini.go:151-158`, `:186-195`); no caller sets
them. Graph queries are the tools worth exposing first — "what depends on X",
"what is unreplicated", "what has no PDB" — because they are read-only,
cheap, and exactly the questions the generator needs answered.

Note the sequencing argument: making the loop tool-using *before* there are
tools worth calling produces a worse planner than the single-shot one, because
its only tools would return replica counts and `{"configured":false}`. The
graph tools are the first ones with real answers.

## 8. Phasing

| Phase | Deliverable | Acceptance |
| --- | --- | --- |
| **0** | Fence fixes | The four known holes closed; see §9 |
| **1** | Efficacy (`ProbeSpec`, settle gate, `fault.efficacy` audit event) | A `NetworkChaos` fault on a DPv2 cluster fails at inject time with a named probe, instead of succeeding |
| **2** | `kube-state` driver, both modes, nine fault kinds | Each of the nine produces its target Reason on kind, verified by its own probe |
| **3** | `Scenario` type, `ScenarioID` plumbed, parity + lookout packs | Twenty-one scenarios reach the same observable state as their upstream twins; equivalence test green |
| **4** | `pkg/eval` + `cmd/simian-eval` + `exec:` subject | **k8s-lookout scored in CI, twice, with identical results** (§6.7) — then `core-sre-agent`, comparable to its existing baseline |
| **5** | Topology-driven generation | A generated scenario, never hand-written, that the stack misses — with valid ground truth |
| **6** | Curriculum; multi-cluster; the dashboard | Driven by `kube-agent-demo-e2e`'s two-cluster fleet |

Phases 0–4 are the rig. Phase 5 is the product. Phase 6 is the demo.

### 8.1 Prerequisite: Simian had no local cluster story

*Resolved by #53. The diagnosis is kept because the decision it forced is
recorded below.*

At plan time: `grep -rl 'kind create cluster'` across this repo returned
**nothing**. There was no kind config, no cluster script, no e2e workflow — CI
was `test`, `lint`, `tidy`, `govulncheck` and nothing else. Every verification
claim in the roadmap had been made by hand against a live GKE cluster.

An eval rig cannot be built on a cluster someone has to remember to create.
This is the true first task, and it is copy-work rather than design work:
`k8s-lookout/examples/kind/{cluster.yaml,up,down}` and
`core-sre-agent/internal/kindcluster` are both known-good and both solve
exactly this. `kindcluster` is the closer fit — it is Go, it is already shaped
as a library for a test harness, and it does fresh-per-run with a
`context.WithoutCancel` teardown.

Note the constraint discovered earlier: how much of NetworkPolicy the CNI
enforces varies, and GKE Dataplane V2 bypasses Chaos Mesh `NetworkChaos`. **No
single environment runs all of Simian's engines.** The kind config must
therefore either pin a CNI that enforces NetworkPolicy (Calico) or the
dataplane pack must declare which environments it is valid in — and §5's
efficacy probes are what make that failure loud instead of silent.

#### Decision: kind + Calico is the reference environment *(#53, shipped)*

Calico, pinned. Older kindnet *accepted* `NetworkPolicy` objects without
enforcing them, and Simian's `network-policy` engine works by creating exactly
those objects: a partition fault would apply cleanly, report success, and block
nothing.

For a chaos tool that is a bug. For an eval rig it is disqualifying — the
subject under test gets scored on an incident that never happened, and there is
no error anywhere to notice, just a fault that did not land. That is the
precise failure mode §5 exists to prevent, so the substrate must not be the
thing introducing it.

Recent kindnet closes that particular gap — measured on kind v0.31.0 /
Kubernetes v1.35.0, a workload is reachable before a deny-all-ingress policy
and refused after. The rig still pins Calico, for reasons unrelated to the old
caveat: Calico is what the eval targets look like, and a pinned CNI does not
change behaviour underneath the rig when the node image moves.

| engine | kind + kindnet | kind + Calico | GKE Dataplane V2 |
| --- | --- | --- | --- |
| `chaos-mesh` | yes | yes | `NetworkChaos`: no |
| `network-policy` | recent kindnet only | yes | yes |
| `envoy-fault` | yes | yes | yes |

kind + Calico is the reference because it runs all three implemented engines on
a version Simian pins. GKE Dataplane V2 stays a second target where
`chaos-mesh` `NetworkChaos` scenarios are invalid; declaring that per-scenario
lands with the dataplane pack (#67).

The decision is enforced rather than documented: `TestCNIEnforcesNetworkPolicy`
in `test/e2e` proves connectivity, applies a deny-all, and fails the build if
traffic still flows. A comment in a YAML file would not have survived the first
CNI bump.

What shipped with it: `internal/kindcluster` (create/delete/kubeconfig,
Calico, Chaos Mesh, verification), `make cluster` / `make cluster-down` /
`make e2e`, and an `e2e-kind` workflow on push to main. Credentials land in
`.kube/e2e.yaml` inside the work tree and never in `~/.kube/config`, so the
file naming the throwaway cluster physically cannot name a real one.

### 8.2 Vertical slice first

The roadmap's own M1 note says the right thing — "sequenced as a vertical
slice first, then breadth and depth" — and it applies with more force here,
because the expensive mistake available is building thirty fixtures against a
scoring model that turns out to be wrong.

So: **four fault kinds → one pack → lookout as subject → a scored run in CI**,
end to end, before any breadth. If that chain works, everything after it is
filling in a table. If it does not, we find out having written four fixtures
instead of thirty.

The slice is #53 → #54 → #56 → #60 → #62 → #63 → #64, with #48 alongside it
because a live namespace escape should not sit. That chain is the only thing in
the ledger that must be strictly serial.

### 8.3 Issue ledger

Tracked by #74–#79, one tracking issue per phase. Sizes are relative:
**S** ≈ a focused sitting, **M** ≈ a day's work, **L** ≈ several. Everything
within a group is independent of its siblings.

| Issue | Work | Size | Depends on |
| --- | --- | --- | --- |
| **Phase 0 — fences** *(all parallel, all independently mergeable)* |
| #48 | Validate `spec.selector.namespaces` against arena eligibility | M | — |
| #49 | Wire `executor.permittedTiers` from chart → flag → executor | S | — |
| #50 | ✅ `networking.k8s.io` create/delete in the per-arena Role, all three sites, held together by a parity test | S | — |
| #51 | ✅ `activeFaultCount` counts NetworkPolicies and names them; netpol faults carry a cluster-side expiry a restarted controller reaps | M | — |
| #52 | Housekeeping: `errors.As`, `topology` `stopCh` leak, `tierOrdinal` unknown-tier default, TOCTOU on concurrency/cooldown, drop `coverage.out` and `resume`, fix `examples/network-latency-manifest.json` | S | — |
| **Phase 1 — ground under our feet** |
| #53 | ✅ `internal/kindcluster` + `make cluster` + an `e2e-kind` job that asserts the rig (§8.1) | M | — |
| #54 | `ProbeSpec` `Settle` mode, `k8s` probe type, executor gate, `fault.efficacy` audit event | M | — |
| #55 | Probes on the existing engines' catalog entries — DPv2 `NetworkChaos` now fails loudly | M | #54 |
| **Phase 2 — the `kube-state` engine** |
| #56 | ✅ Driver skeleton + `synthesize` mode + 4 kinds: `ImageUnresolvable`, `ContainerExitLoop`, `MemoryLimitSqueeze`, `Unschedulable`, each with a default efficacy gate; verified on GKE | L | #54 |
| #57 ✅ | Remaining parity kinds: `JobFailure`, `SelectorDrift`, `UnboundClaim`, `NoOp`, and `DependencyStall` with the `logs` probe type it needed — each gated and verified on GKE | L | #56 |
| #58 ✅ | Lookout-only kinds: `PDBGridlock`, `RolloutStuck`, `CertExpiry` — namespace tier, gated and verified on GKE. `NodeUnready` is **not shippable in this engine**: a phantom Node is deleted by the cloud-node-controller within 10s on GKE, and every other mechanism mutates a real node, which is #59's problem ([known limitations]({{< relref "known-limitations.md" >}}#nodeunready-cannot-be-synthesized-on-a-cloud-managed-cluster)) | M | #56 |
| #59 | `mutate` mode + revert-on-lease-expiry | L | #56 |
| **Phase 3 — scenarios** |
| #60 ✅ | `Scenario` type, `ScenarioID` plumbed through executor + audit, pack loader | M | — |
| #61 | `BackendCrashLoop` — the `cascade` shape, the last parity gap, gated and verified on GKE — then the lookout pack (10) + parity pack (11) + the equivalence-matrix test | M | #57, #58, #60 |
| **Phase 4 — the rig** |
| #62 ✅ | `pkg/eval`: `Report`, `Subject`, and the seven measures | M | #60 |
| #63 ✅ | `cmd/simian-eval` + `exec:`/`noop:` adapters + arena lifecycle, namespace fencing, and the artifacts scored back through #66 | M | #53, #62 |
| #64 | **Lookout subject + the scored e2e in CI** — §6.7 | M | #61, #63 |
| #65 | `core-sre-agent` subject; reproduce its existing baseline through this rig | M | #64 |
| #66 ✅ | `simian evaluate`: audit + report artifacts joined on `ScenarioID`, `NOT SCORED` rows, `--min-efficacy` refusal | S | #62 |
| **Phase 5 — the product** |
| #67 | Dataplane pack (5 scenarios), starting with the `stress-real` / `latency-not-saturation` matched pair | L | #55, #61 |
| #68 | Graph query tools on `CompletionRequest.Tools`; make the loop tool-using | L | — |
| #69 | Topology-driven `Scenario` generation with ground truth attached | L | #59, #67, #68 |
| #70 | Adversarial curriculum — generate against measured blind spots | L | #69 |
| **Phase 6 — the demo** |
| #71 | Multi-cluster, driven by `kube-agent-demo-e2e`'s two-cluster fleet | L | #63 |
| #72 | Scorecard view in the web UI (#45) — archive mode over a run artifact | L | #64, #45 |
| #73 | Decide Litmus: implement or remove the surface (§11.4) | S | — |

**Critical path:** #54 → #56 → #60 → #62 → #63 → #64. Everything in Phase 0 is off
the path and can land in any order by anyone. #53 is off the path but blocks
#63, so it wants doing early.

**First three merges, in order:** #48 (the namespace escape is a live
correctness bug and does not want to sit), #53 (nothing can be tested until
there is a cluster), #54 (efficacy is the foundation everything else stands on).

### 8.4 What "done" looks like

A single command, in CI, producing a table:

```
scenario                 lookout   sre-agent   mast+lookout
fault-crashloop            1.00        1.00          1.00
fault-invoicing            0.00        0.50          1.00
latency-not-saturation     0.00        0.00          0.50
stress-real                1.00        1.00          1.00
pdb-gridlock               1.00        0.00          1.00      ← agent below its own tool
```

That table is the deliverable. Everything in this document exists to make it
trustworthy: §5 so a row is not silently measuring nothing, §6.1 so the
columns can be joined, §6.7 so the first column is a floor rather than another
opinion.

It renders in the browser as a view of the **existing** web UI (#45), not a
second application — see [`web-ui-design.md`]({{< relref "web-ui-design.md" >}}),
"One site, two data sources". That decision constrains #63: **the
`simian-eval --out` run artifact must be self-describing JSON that renders with
no backend at all.** Which is a gift rather than a cost — the same property
makes a run attachable to a CI job, diffable between two commits, and openable
by someone who has never installed Simian.

Two rendering rules matter enough to state here as well as there: a scenario
whose efficacy probe failed renders as **not measured**, never as a zero; and
the deterministic-detector column is a **floor**, so a subject scoring below it
is flagged as failing to use its tools rather than failing to diagnose.

## 9. Phase 0 in detail

These were found as safety bugs. Under this plan they are also
**dataset-integrity bugs** — an eval is only valid if the fault was exactly
what the label says it was, and a fault that escapes its namespace has
mislabelled every other scenario running beside it.

1. **Namespace escape.** `pkg/driver/chaosmesh/driver.go:95-105` copies
   `spec` verbatim and injects only `spec.duration`; eligibility validation
   (`pkg/executor/executor.go:241-265`) checks only `m.Targets[].Namespace`,
   never `spec.selector.namespaces`. A manifest can therefore target any
   namespace in the cluster.
2. **`executor.permittedTiers` is inert.** `deploy/helm/simian/values.yaml:46-48`
   is read by nothing — no flag, never passed by `deployment.yaml`. Node-tier
   chaos cannot be disabled by configuration.
3. **Per-arena Role is missing `networking.k8s.io`.** `deploy/manifests/00-rbac.yaml:66-82`
   and the chart's `serviceaccount.yaml:55-74` omit NetworkPolicy
   create/delete, so the DPv2-recommended engine is Forbidden in-cluster. It
   works from `simian serve` locally because that path uses the operator's own
   kubeconfig, which is why this was not caught.
4. **`arena.activeFaultCount` omits NetworkPolicies** (`pkg/arena/arena.go:367`),
   so the pre-destroy safety check under-reports exactly the fault class that
   leaks permanently on crash — the `networkpolicy` driver has no TTL of its
   own (`pkg/driver/networkpolicy/driver.go:75-176`).

Also in scope, lower severity: TOCTOU on the concurrency and cooldown checks
(`executor.go:276-293`), the advisory-only severity cap (`pkg/loop/loop.go:253`
compares the LLM's self-declared tier), `tierOrdinal` defaulting unknown tiers
to *least* severe (`loop.go:313-324`), the `pkg/topology` informer goroutine
leak (`discoverer.go:75-90`), and `err.(*simian.ExecutorError)` where
`errors.As` is wanted (`executor.go:309`).

## 10. Non-goals

* **Outcome verification.** §1.1. Simian never reports what broke downstream.
* **Detection logic.** That is `k8s-lookout`. Simian never grows checks.
* **An agent harness.** That is `mast` / `core-agent`. Simian's loop stays its
  own and stays small.
* **Competing with ChaosBlade / Blade AI.** Different category — a
  conversational chaos operator for human drill-running. Under this plan it is
  a *subject* the `http:` adapter can benchmark, which is more useful than
  competing with it.
* **Replacing `internal/faults`.** §2.1. It keeps working, unmodified, on its
  own fixtures.

## 11. Open decisions

1. **Engine name.** `kube-state` is proposed over `workload` because the
   driver also mutates Services, PVCs, and Jobs. Not load-bearing; easy to
   change before Phase 2.
2. **Where the dataplane packs' SUT comes from.** Online Boutique is heavy for
   a per-scenario fresh cluster. A smaller purpose-built topology with a known
   dependency graph may serve Phase 5 better — and a graph we designed is a
   graph we can write assertions about.
3. **Whether `mutate`-mode reverts are trustworthy enough to run scenarios
   sequentially on one cluster,** or whether fresh-per-scenario (as
   `sre-eval-live` does) stays mandatory. Fresh is correct and slow; this is a
   throughput decision to make after Phase 2, with data.
4. **Litmus.** `pkg/driver/litmus/` is an empty directory whose constant, tier
   rule, and RBAC all ship, so an apply fails with `no driver registered`.
   Either implement it or remove the surface — under this plan there is no
   urgency for a second dataplane engine, so removal is the cheaper honest
   answer.
