# lookout — the observer's example scenarios, reproduced

Nine scenarios: eight mapped from the ten under `examples/scenarios/` in the
observer repository, plus a control that has no counterpart there. The subject
here is a watcher rather than a triager, so
the set leans toward faults that are invisible at pod altitude: a deploy that
never landed while the old revision kept serving, a Service whose selector
matches nothing, a disruption budget with no headroom, a certificate with two
days left.

As with the parity pack, there is no Go dependency in either direction.

## The equivalence matrix

| Observer scenario | Scenario           | Engine kind          | Deviation |
| ----------------- | ------------------ | -------------------- | --------- |
| `bad-rollout`     | `bad-rollout`      | `RolloutStuck`       | — |
| `cert-expiry`     | `cert-expiry`      | `CertExpiry`         | also creates the Deployment that mounts the Secret |
| `crashloop`       | `crash-loop`       | `ContainerExitLoop`  | — |
| `endpoints-empty` | `endpoints-empty`  | `SelectorDrift`      | — |
| `image-pull`      | `image-pull`       | `ImageUnresolvable`  | — |
| `oom`             | `oom`              | `MemoryLimitSqueeze` | fixed allocation rather than a ramp |
| `pdb-gridlock`    | `pdb-gridlock`     | `PDBGridlock`        | budget written with no headroom rather than scaled into gridlock |
| `pending`         | `pending`          | `Unschedulable`      | engine-default CPU request rather than 64 cores |
| `failed-mount`    | — | — | **no kind** |
| `node-failure`    | — | — | **no kind** |
| —                 | `healthy`          | `NoOp`               | added: the set has no control |

Eight of ten, plus one addition.

The addition is `healthy`, and it is not padding. A pack without a control
cannot detect a subject that reports every failure mode everywhere — recall
would be perfect and precision unmeasured — and that risk is higher for a
watcher than for a triager, whose whole job is to decide that most of what it
sees is fine.

## The two gaps

**`failed-mount`** — a pod whose volume references a ConfigMap that was never
created, parked in `ContainerCreating` with `FailedMount` events. There is no
kube-state kind for it. It is the cheapest of the two to close: a bundle with a
Deployment mounting a name nothing else creates, gated on the pod's
`FailedMount` event or on a container status of `ContainerCreating`. The reason
it is worth having rather than approximating is structural — it is the only
scenario in either pack whose fault is a *dangling reference*, and a report that
names the missing object is doing something no status field handed it.

**`node-failure`** — stop a worker node and watch what the observer does *not*
do, which is open a session per evicted pod. This one is not a missing bundle,
it is a missing tier. Every kube-state kind is namespace-scoped by construction:
the engine synthesizes objects inside a namespace it is allowed to write to, and
a fault that takes a node down affects every workload on it, including ones
nobody consented to break. Closing it means a node-tier kind with the safety
fences to match, and the fence is the hard part, not the fault. The
`NodeUnready` hole is tracked as its own piece of work for that reason.

## Reason vocabulary

Expectations are written in Kubernetes' vocabulary — the tokens a report about
the cluster would use — plus the observer's own signal kind where it is
distinctive enough to be safe. `rollout.stall`, `objectstate.endpoints_empty`
and `capacity.pending-aged` are long and compound; `MatchesReason` is a
bidirectional substring match, so a short token would annex anything containing
it. Bare `BackOff` is deliberately absent from `crash-loop` for exactly that
reason: it would also accept `ImagePullBackOff`, and a report about the wrong
fault would score full recall.

Where one fault has two correct spellings, both are listed. `crash-loop` accepts
`ExcessiveRestarts` alongside `CrashLoopBackOff`, because a subject that names
the restart count has found the fault. It has named it less sharply, and that is
a real difference — but it is a *severity* difference, and the severity measure
is where it gets charged. Recall asks whether the subject found the fault, not
how well it phrased it.

Timing is deliberately not part of that choice. The efficacy gates hold until
the pod is sitting in `CrashLoopBackOff` and has been for as long as anyone
cares to look, so which spelling a subject reaches for is its own judgement
rather than an accident of when the scan landed. Getting that wrong cost a run:
before the gates were fixed, three identical runs scored severity 0.67, 1.00,
1.00.
