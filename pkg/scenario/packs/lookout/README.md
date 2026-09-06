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
| `pending`         | `pending`          | `Unschedulable`      | engine-default CPU request rather than 64 cores; gate holds 5m30s |
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

## What the pack scores today

The reference measurement, and the reason it is worth writing down: the subject
is a detector, so these numbers are a property of the pack rather than of a
model's mood. Two consecutive runs that disagree are a bug in Simian.

k8s-lookout v0.9.0, GKE (Kubernetes v1.36.3-gke.1537000), whole pack, every
fault landed — `efficacy rate 1.00`:

| Scenario | recall | severity | hallucinated_fault |
| --- | --- | --- | --- |
| `bad-rollout` | 1.00 | 1.00 | 1.00 |
| `cert-expiry` | 1.00 | 1.00 | 1.00 |
| `crash-loop` | 1.00 | 1.00 | 1.00 |
| `endpoints-empty` | **0.00** | 0.33 | 1.00 |
| `healthy` (control) | — | 1.00 | 1.00 |
| `image-pull` | 1.00 | 1.00 | 1.00 |
| `oom` | 1.00 | 0.67 | 1.00 |
| `pdb-gridlock` | **0.00** | 0.33 | 1.00 |
| `pending` | 1.00 | 1.00 | 1.00 |
| **pack mean** | **0.75** | **0.81** | **1.00** |

One run, not a best-of: the subject is a detector, so a second run that
disagreed would be a bug rather than variance.

Every scenario Simian is responsible for scores 1.00. The two zeroes are the
upstream coverage gaps below, and they are recorded here rather than filed
against this pack.

Three of these once read `recall 0.00`, all filed as Simian's under #110. One of
them was.

**`pending` was ours.** The observer's pod-level check has a five-minute dwell
before it will call a Pending pod a fault, and Simian's efficacy gate passed
about two seconds after apply — the pod really was Pending and really was
`Unschedulable`, which is true and is not the same as steady. So the subject was
asked while it was still, by its own definition, looking at a slow scheduler.
Same shape as the crash-loop bug this pack already caught, in a different kind.
The gate now holds: `Unschedulable` takes a `dwell`, defaulting to 90 seconds,
and this scenario raises it to `5m30s` with a matching `duration: 12m`. That
number is calibrated to the detector, which an *engine default* must never be —
but a pack whose whole purpose is to reproduce another project's examples has to
be visible to that project's detector, or it measures the grace period instead
of the diagnosis. The scenario says so in its own comments.

**`pdb-gridlock` and `endpoints-empty` were not.** Both were re-tested by hand on
GKE against a namespace built to order — a two-replica Deployment under a
`minAvailable: 2` budget sitting at `disruptionsAllowed: 0`, and a Service whose
selector matched nothing. Both faults were unambiguously present, and the verb
the subject adapter invokes reported neither. `lookout health` excludes the
`pdb` class by design — gridlock is disruption *readiness*, not one of the health
categories it scans — while `lookout triage delta` names it instantly. And no
one-shot verb reports an empty-endpoints Service at all;
`objectstate.endpoints_empty` exists only in the watch source. Those are upstream
coverage gaps in the verb, not fault-timing bugs here, and are filed as such with
the scenarios attached.

## The measure that was wrong, and it was the precision one

`bad-rollout` also scored `hallucinated_fault` **0.50** for a while, which is the
harder bug of the two because it charged the subject for being right.

The wedge container exits non-zero, so the new revision's pod really is in
`CrashLoopBackOff`, and the detector reported it alongside the
`ProgressDeadlineExceeded` the scenario asks for. The scorer read the injected
failure modes out of the expectations alone, saw no crash-loop family there, and
called a true statement an invention. Promoting the pod to an expectation would
have traded that for a recall bug: the subject that reports only the Deployment
gives the *better* answer, and would have scored 0.50 for it.

Scenarios now carry `also_true` — reason tokens the fault mechanically produces,
which suppress the hallucination charge and count for nothing in recall. Only
`bad-rollout` uses it. `oom` declines the identical exemption on purpose, and
says why in its own comments.

## Two clocks, on the control

The control has its own version of the timing bug, and it took a slow machine to
see. On a
two-core GitHub runner `healthy` scored severity **0.33**, against 1.00 twice on
this kind cluster and 1.00 on GKE: the detector reported
`Deployment/… RolloutIncomplete`, because the pod's `Ready` condition and the
Deployment's `status.readyReplicas` are written by different controllers and the
subject was asked in the gap. Every kind whose workload is supposed to be
healthy now waits for both — see `simian-workload-rolled-out` in
[efficacy-probes](../../../../docs/site/content/docs/efficacy-probes.md).
Nothing in the table above moved when that landed, because no machine it has
ever been measured on is slow enough to show the window — which is the point.
The number that proves the fix is the one from the two-core runner.

`crash-loop`, `image-pull` and `healthy` are the three the `e2e-kind` workflow
runs on every push to `main` — a fault that lands in seconds, one that takes
four minutes to become steady, and the case where reporting nothing is the
right answer. The whole pack runs weekly. Keep the list in
`.github/workflows/e2e-kind.yml` in step with the IDs here; a stale one fails
the run rather than silently testing less, because `--only` refuses an ID the
pack does not contain.
