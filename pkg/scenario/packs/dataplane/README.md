# dataplane — the faults the API server does not record

Every other pack can be solved by reading object status well enough. A pod that
will not pull writes `ImagePullBackOff` into its own container status; a rollout
that will not finish writes `ProgressDeadlineExceeded` into its Deployment's
conditions. The answer is in the cluster, spelled out, in a field. A subject
that walks the object graph carefully and never sends a single request scores
well on the parity and lookout packs, and that is not an accident of how they
were written — it is what "inject a fault" tends to produce.

This pack is the other kind. Nothing about "the p99 doubled" is stored in the
API server. The faults here degrade traffic, and the only honest evidence that
anything is wrong is a request that behaves differently than it did a moment
ago.

## The matched pair

`latency-not-saturation` and `stress-real` are one fixture, split in two, and
neither is worth much without the other.

Both present the identical symptom: a caller with no available replicas in
front of a callee whose pods are Ready and whose Deployment agrees. Measured on
GKE, every field the API server records is the same across the two. The only
thing that separates them is utilisation — flat in one, pinned at the cgroup
limit in the other.

|                            | `latency-not-saturation` | `stress-real`      |
| -------------------------- | ------------------------ | ------------------ |
| fault                      | NetworkChaos `delay: 3s` | StressChaos `cpu`  |
| caller ready               | 0/2                      | 0/2                |
| callee ready               | 2/2                      | 2/2                |
| callee latency             | ~3s                      | ~2.5s              |
| callee CPU                 | idle                     | at its limit       |
| right answer               | the path is slow         | the callee is out of CPU |
| `scale the callee up`      | wrong                    | right              |

An agent that answers "it is slow, scale it up" scores 1.00 on one and 0.50 on
the other, and is charged with inventing a fault on the one it got wrong. No
fixture set containing only one of these can tell that agent apart from one
that looked at utilisation before answering — which is the entire reason both
ship. `TestTheMatchedPairSeparatesSubjectsThatGuessFromSubjectsThatMeasured` in
`pkg/eval` is that claim, asserted rather than described.

The discrimination rests on three things, each of which could be undone by a
plausible-looking edit:

- `network-degradation` and `cpu-saturation` are separate failure families, so
  a claim in one is charged as an invention in the other's scenario.
- `HighLatency` and `Latency` are in neither, because a workload can be slow
  for four unrelated reasons and the bare word names none of them. A subject
  that reports only slowness scores the symptom in both halves and is charged
  in neither, which is exactly right: it said something true and diagnosed
  nothing.
- Neither half's `also_true` exempts the other's diagnosis. The asymmetry
  between those two lists *is* the measurement.

Everything below the diagnosis is deliberately shared. The prompt, the
substrate, the efficacy gate and the symptom expectation are the same in both
files, so a subject that reads object status and stops scores identically in
each — otherwise part of the difference between the two scores would be a
difference in difficulty rather than in cause.

## The other two

`abort-503-not-a-bug` and `partition-one-way` produce the same object status as
the pair — caller 0/2 Ready, callee 2/2 — from two more causes, so all four
critical scenarios in this pack are indistinguishable to a subject that reads
the API server and stops. What separates them is graded differently in each,
which is deliberate:

| | the fault | what the grade turns on |
| --- | --- | --- |
| `latency-not-saturation` | netem delay on the callee | naming the network rather than CPU |
| `stress-real` | CPU stressors in the callee's cgroup | naming CPU rather than the network |
| `abort-503-not-a-bug` | synthesized 503 on the callee's work path | naming the **callee** as the root |
| `partition-one-way` | the caller's egress to the callee dropped | naming the link as severed rather than slow |

`abort-503-not-a-bug` is the one graded on an object rather than on a word. A
status code is an observation — a 503 is what a synthesized abort, a shed load
and an empty backend pool all look like — so every reason token in it is
generic and nothing in it can be charged as an invented cause. The two subjects
worth telling apart both write "503": one says the caller is returning them and
one says the callee is. Both are true. Only the second is the answer, and the
first is what a subject concludes from the fact that the caller is the only
object the cluster reports as unhealthy.

`partition-one-way` is the one that cannot be answered from a single vantage
point. Each end reports itself healthy and each end is telling the truth; the
failure exists only in the relationship, and a subject has to compare two
measurements to see it. It also charges the pair's network answer:
`network-partition` and `network-degradation` are separate families, so calling
a link that carries nothing "congested" or "slow" is charged here, and calling
a link that carries every packet three seconds late "severed" is charged in
`latency-not-saturation`. Neither exempts the other, in either direction.

`PacketLoss` is the closest call in the pack. It belongs in
`partition-one-way`'s `also_true` on its merits — a partition is
hundred-percent loss, and a subject that measured the link and called it lossy
measured correctly. It is left out because `also_true` exempts by family rather
than by token, so listing it would license "the link is congested" as well, and
keeping severed distinguishable from slow is worth more than crediting one
hedge.

## The control

`dataplane-healthy` is the namespace where "the callee is slow" is wrong. Given
a pack in which every other scenario is a flavour of slowness, that answer is
worth guessing, and the control is what makes guessing cost something.

It carries probes of its own, which is unusual for a control and is the point:
a control's claim is that the namespace is healthy *at the moment the subject
is asked*. The `NoOp` gate only speaks for the `NoOp`'s own workload, so
without them a substrate that came up degraded would be graded as a healthy
cluster and every honest report about it charged as a hallucination.

## The substrate

All five scenarios name `substrate: edge-upstream` — a caller, a callee, and a
Service between them. See `pkg/sut/edgeupstream/README.md`, particularly the
part about why the callee runs a CGI work loop instead of returning a static
200: with a trivial responder, CPU saturation produces no latency at all and
the matched pair collapses into one scenario measured twice.

A scenario needs a substrate at all because the `kube-state` engine cannot
supply one. It appends a suffix derived from the fault's UID to everything it
creates, so a second fault in the same scenario can predict neither the first
one's name nor its labels — and a fault cannot inject latency into a caller
that does not exist.

## Environment validity

**NetworkChaos delay** applies netem through the tc qdisc. On some GKE
Dataplane V2 (eBPF/Cilium) versions the CR applies cleanly and the effect never lands,
because the datapath bypasses the qdisc. Verified landing on current GKE. That
failure mode is not silent here in any case: the Settle gate measures the
latency it claims, and a fault that did not land is rolled back rather than
graded.

**StressChaos** runs stress-ng inside the target's cgroup, so it is only as
strong as the cgroup's ceiling. The substrate sets a 200m CPU limit for exactly
this reason. On a cluster where that limit is stripped by a mutating webhook or
a LimitRange, the stressors compete for a whole node, the callee stays fast,
and the Settle gate fails — loudly, which is the intended outcome.

**NetworkChaos partition** uses iptables rules in the selected pods' network
namespaces rather than tc, so the eBPF caveat above does not apply to it.
`direction: to` names the packets that are dropped and not the connections that
survive: TCP needs a return path, so the callee cannot complete a handshake
back to the caller either. The scenario is written around the caller's outbound
calls rather than around a claim that the reverse direction still carries
requests.

**HTTPChaos** puts a transparent proxy in the target pod's network namespace,
which needs a container runtime chaos-daemon can join and a plaintext port to
intercept; it does not apply to HTTPS. Verified landing on GKE with containerd.

**Neither half of the pair is gated by a default probe,** and neither is
HTTPChaos or a partition that names a `target`. StressChaos and HTTPChaos have none to
have — there is no field on any object that says a cgroup is full or that a
status code was synthesized — a NetworkChaos naming a `target` is refused one
because the controller stands outside both labelled sets and would see nothing
change, and the delay gate is written out on purpose and suppresses the default
by reusing its probe names.

So every gate in this pack is hand-written, and every one of them has an SOT
half as well as a Settle half. The substrate existed before the fault, so "the
callee is slow" is not evidence of anything without "the callee was fast", and
`TestEveryDataplaneFaultIsProvedByARequest` requires both halves to be an *http*
probe: an object read cannot tell a fault that landed from a substrate that came
up wrong.

## Scores from here are not comparable with the other packs

Deliberately. A subject can score 1.00 on parity and 0.00 on every scenario
here, and that difference is the measurement — it says the subject reads object
status and does not measure traffic. Averaging the two together would hide the
one thing this pack was built to show.
