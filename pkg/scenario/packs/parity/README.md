# parity — the agent-side live eval fixtures, reproduced

Eleven scenarios, one per fixture in `internal/faults/fixtures.go` in the
agent repository. The point of the pack is comparability: a recall or
hallucination number produced by Simian against these and one produced by the
agent's own live tier against its fixtures should be arguable about the
subject, not about the rig.

Nothing in this repository imports that one, and nothing in that one imports
this. The independence is the whole argument — a shared library that produced
both sides could be wrong in the same way twice and neither number would
notice. What is shared is a transcription, checked in at
`../../testdata/upstream-fixtures.yaml`, and a drift test that compares the
transcription to the upstream source when you point it at a checkout.

## The equivalence matrix

| Upstream fixture   | Scenario            | Engine kind          | Deviation |
| ------------------ | ------------------- | -------------------- | --------- |
| `fault-imagepull`  | `image-pull`        | `ImageUnresolvable`  | namespace renamed |
| `fault-crashloop`  | `crash-loop`        | `ContainerExitLoop`  | namespace renamed |
| `fault-oomkill`    | `oom-kill`          | `MemoryLimitSqueeze` | namespace renamed; allocates 256Mi rather than 400MB against the same limit |
| `fault-unschedulable` | `unschedulable`  | `Unschedulable`      | namespace renamed; engine-default CPU request rather than 64 cores |
| `fault-failedjob`  | `failed-job`        | `JobFailure`         | — |
| `fault-badselector`| `bad-selector`      | `SelectorDrift`      | Service targets the port it publishes |
| `fault-none`       | `healthy`           | `NoOp`               | — |
| `fault-storefront` | `multi-fault`       | `ImageUnresolvable` + `Unschedulable` + `JobFailure` | three manifests rather than one |
| `fault-sessions`   | `cascade`           | `BackendCrashLoop`   | — |
| `fault-ledger`     | `unbound-volume`    | `UnboundClaim`       | claim and Deployment share a name |
| `fault-invoicing`  | `silent-failure`    | `DependencyStall`    | the failure is described, not produced — see below |

Eleven of eleven. `cascade` was the last hold-out and is what `BackendCrashLoop`
was added for: it needs a crash loop and the Service it takes down to be two
objects in two different wrong states, and no kind could produce that pair.

## The four renamed namespaces

`fault-imagepull`, `fault-crashloop`, `fault-oomkill` and `fault-unschedulable`
name their own fault, and the prompt quotes the namespace. Simian's
`LintPrompt` refuses that — a prompt that leaks the diagnosis measures
paraphrasing instead of diagnosis — so those four become `fault-checkout`,
`fault-payments`, `fault-caching` and `fault-analytics`.

This makes four scenarios *harder* here than upstream, which is a divergence
and is worth naming rather than burying: a subject scoring these gets no hint
the upstream subject got. Upstream is aware of the same problem and applies the
rule from `cascade` onward — that fixture's comment says a namespace called
`fault-crashloop-behind-a-service` "would hand over the answer in the prompt,
which is the thing this tier exists not to do". The pack applies the rule to
the earlier six as well.

## The one deviation that changes what is measured

`silent-failure` is strictly easier than `fault-invoicing`. Upstream *produces*
the failure — the container runs `wget` against a port nothing serves, and the
error is `wget`'s own stderr — precisely so that the diagnosis is not readable
out of the pod spec. `DependencyStall` writes a configured line, and the line
reaches the container through its environment, so a subject that reads the
Deployment and never reads a log can still answer.

Recall stays comparable. The read-path claim — "this is the only scenario no
status field can answer" — does not, and closing it needs a stall kind that
produces the failure rather than describing it.

## What the pack scores, and what the other rig scored

This is the measurement the pack exists to make: run the agent against Simian's
reproduction of its own fixtures, and see whether the numbers its own harness
produces come back.

`core-sre-agent` @ `bin/sre-agent`, Sonnet 5 orchestrator with a Haiku 4.5
specialist roster, GKE (Kubernetes v1.36.3-gke.1537000), whole pack, every
fault landed — `efficacy rate 1.00`:

| Scenario | recall | root_cause | severity | hallucinated_fault | time_to_detect |
| --- | --- | --- | --- | --- | --- |
| `image-pull` | 1.00 | — | 1.00 | 1.00 | 42.4s |
| `crash-loop` | 1.00 | — | 1.00 | 1.00 | 32.4s |
| `oom-kill` | 1.00 | — | 1.00 | 1.00 | 45.3s |
| `unschedulable` | 1.00 | — | 1.00 | 1.00 | 36.2s |
| `failed-job` | 1.00 | — | 1.00 | 1.00 | 39.1s |
| `bad-selector` | 1.00 | — | 0.67 | 1.00 | 27.1s |
| `healthy` (control) | — | — | 0.67 | 1.00 | 38.5s |
| `multi-fault` | 1.00 | — | 1.00 | 1.00 | 96.8s |
| `cascade` | 1.00 | 1.00 | 1.00 | 1.00 | 48.0s |
| `unbound-volume` | 1.00 | — | 1.00 | 1.00 | 37.3s |
| `silent-failure` | **0.00** | — | 0.67 | 1.00 | 73.1s |
| **pack mean** | **0.90** | **1.00** | **0.91** | **1.00** | **46.9s** |

2.5M input tokens (1.7M cached), 38k output, 78 tool calls. Delegation on 3 of
11 fixtures, four specialists — `job-inspector` twice, `reliability-auditor`,
`config-auditor`, `pod-inspector`.

Unlike the lookout pack's row, **this one is a sample and not a property.** The
subject is an LLM: two runs that disagree are variance, not a bug, which is
exactly why the detector's row next to it is worth keeping.

### Against the agent's own harness

The comparison, against the tier-2 baseline in the agent repo's `AGENTS.md`
(2026-08-15, `sre-eval-live`, Sonnet 5, 11/11 scored, on kind):

| Measure | `sre-eval-live` | this pack | Δ |
| --- | --- | --- | --- |
| fault_recall | 1.000 | 0.90 | −0.10 |
| hallucinated_fault | 1.000 | 1.00 | 0.00 |
| fault_severity | 0.909 | 0.91 | +0.00 |
| root_cause | 1.000 | 1.00 | 0.00 |

Three of the four land inside ±0.01 — and each is the mean of eleven
independently written scenarios, injected by a different engine, on a different
cluster, scored by a different implementation of the same four measures. That
is the claim the pack was built to test, and it holds.

Two details are worth more than the aggregate:

**`bad-selector` severity is 0.67, and upstream's is too.** Their note reads
"`fault-badselector` severity is 0.67 for the seventh consecutive run, still
the only cell that has never moved." Eighth, on another rig. A cell that stable
across two harnesses is a property of the agent's severity judgment on that
fault, not of anyone's fixture.

**The whole recall gap is `silent-failure`, and it is the fixture both projects
already distrust.** Upstream scored `fault-invoicing` 1.00 and wrote that "the
score is not worth much" — the agent coined `UpstreamDependencyMissing`, which
their reason list accepts by substring, on a fixture that leaked its diagnosis
into `containers[0].command`. Simian's version leaks it too, through the
container's environment (see the deviation above), so it is the *easier* of the
two. The agent scored 0.00 anyway: eighteen tool calls, three of them
`k8s_resource_spec`, and not one log tool among them. It filed four hygiene
advisories about the right Deployment — `MissingLivenessProbe`, `MissingPDB`,
`ColocatedReplicas`, `MissingResourceRequests` — and never named the stall.

None of those four is charged as an invention, correctly: they are true
statements about the workload, and no token among them asserts a cause. So this
is recall failing on its own, which is the shape a real miss has.

The reading is not "one rig is right". It is that a fixture whose 1.00 came
from a coined token matched by substring will also produce a 0.00, and the two
runs together say more about the fixture than either says alone.

## Running it

Every scenario carries a `namespace`-tier fault with a default efficacy gate,
so a subject is never graded against a namespace where the fault did not land.
The namespaces are created by the run, not by this pack; nothing here touches
anything already in the cluster.
