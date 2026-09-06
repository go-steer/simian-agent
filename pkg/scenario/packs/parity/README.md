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

## Running it

Every scenario carries a `namespace`-tier fault with a default efficacy gate,
so a subject is never graded against a namespace where the fault did not land.
The namespaces are created by the run, not by this pack; nothing here touches
anything already in the cluster.
