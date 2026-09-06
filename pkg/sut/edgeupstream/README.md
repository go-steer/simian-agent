# edge-upstream

Two tiers and a Service between them. `simian sut deploy --sut edge-upstream`,
or a scenario's `substrate: edge-upstream`.

```
      probe / kubelet                edge readiness             upstream
            │                        proxies through                │
            ▼                                │                      ▼
        ┌────────┐   http://upstream:80/cgi-bin/work   ┌──────────────────┐
        │  edge  │ ──────────────────────────────────► │     upstream     │
        │ nginx  │                                     │  busybox httpd   │
        └────────┘                                     └──────────────────┘
         /healthz  local, cheap → liveness              /healthz  static → readiness
         /         proxied, 1s read timeout             /cgi-bin/work  CPU-bound
```

It exists because a dataplane fault needs something to be aimed at, and the
`kube-state` engine cannot provide it: every workload it creates carries a
suffix derived from the fault's UID, so a second fault in the same scenario
cannot predict the first one's name or labels. See `Scenario.Substrate`.

## What each piece is load-bearing for

| Choice | Why |
|---|---|
| The upstream's readiness probe is `exec` on **loopback** | A kubelet `httpGet` arrives over the pod's interface, so a netem delay on that interface delays the probe and the upstream goes NotReady. The scenario then reads straight off `kubectl get pods` and the dataplane question is never asked. |
| …and against the **static** path, not the CGI one | A probe that pays the per-request CPU cost flips the upstream NotReady under saturation, which collapses the saturation fixture into the same "a pod is unhealthy" answer. |
| The edge's **liveness** is local, its **readiness** is proxied | A slow upstream must make the edge NotReady, not restart it. CrashLoopBackOff is a different incident with a different diagnosis, and one no fixture here injects. |
| The upstream is **CPU-bound per request** | See below. Without it, saturation produces no symptom at all. |
| The upstream has a **CPU limit** | StressChaos joins the target's cgroup. With no limit the stressors compete for a whole node and the callee barely notices. |
| The CGI script emits its **headers last** | busybox httpd streams CGI output as it is produced. Headers first means the caller gets a 200 in milliseconds and a truncated body, kubelet sees a 200, and the edge stays Ready through a fault that landed. |
| An init container waits for the upstream to **answer** | nginx resolves a literal `proxy_pass` host once, at config load, and exits if it is not there. A restart count nobody injected is a finding a subject will report and be charged for inventing. (`wget`, not `nslookup`: busybox's `nslookup` walks every search domain and exits non-zero if any NXDOMAINs, which on GKE they all do but the first. The loop never terminates.) |

## Measured

On `std-simian-test` (GKE, Dataplane V2), latency via the API server's pod
proxy, which carries roughly 350ms of its own overhead:

| | `edge` ready | `upstream` ready | upstream CPU | upstream latency |
|---|---|---|---|---|
| baseline | 2/2 | 2/2 | ~1m | 0.46s |
| NetworkChaos `delay: 3s` on `app=upstream` | **0/2** | 2/2 | ~1m, flat | 3.3s |
| StressChaos `cpu: {workers: 4, load: 100}` | **0/2** | 2/2 | **pegged at the 200m limit** | 2.8s |

The two fault rows are the point. Every field the API server records is
identical between them; the only thing that tells them apart is CPU. A subject
that answers "the edge is broken, restart it" is wrong in both, and a subject
that answers "scale the upstream up" is right in one and wrong in the other.

### The first version did not work

The upstream originally served a static `return 200` from nginx. Under
StressChaos the cgroup sat at 100% of its quota — verifiably, in `kubectl top`
— and the latency did not move: 0.36s stressed against 0.36s idle. Answering
that request needs microseconds of CPU, and CFS hands out a slice every period
regardless of how much contention there is for it.

So the saturation fixture had nothing to saturate. A shell loop is a blunt
instrument, but it is the honest one: it gives the callee the property a real
saturation incident has, which is that serving a request costs CPU.

## What is deliberately absent

**No load generator.** The efficacy probe drives the traffic it measures, so
the measurement and the load are on one clock. A background loadgen puts a
second, unobserved traffic source in the namespace, and a gate that fails
because the loadgen was between requests is a flake nobody can reproduce.

**No metrics stack.** Whether a subject can see CPU is a property of the
subject's tools, not of the fixture. A fixture that shipped its own
observability would be grading itself.
