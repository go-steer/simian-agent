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
| An init container waits for the upstream to **answer** | nginx resolves a literal upstream host once, at config load, and exits if it is not there. A restart count nobody injected is a finding a subject will report and be charged for inventing. (`wget`, not `nslookup`: busybox's `nslookup` walks every search domain and exits non-zero if any NXDOMAINs, which on GKE they all do but the first. The loop never terminates.) |
| The edge proxies through a **variable**, and reloads on a resolv.conf change | See below. Without it the caller resolves the callee once and no DNS fault can reach it. |

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

Three more faults land on the same substrate and produce the same two columns —
`edge` 0/2, `upstream` 2/2 — from three further causes. What tells *those* apart
is what the edge answers on each of its two paths, and what happens when you
skip the name:

| | edge `/` | edge `/healthz` | callee, from elsewhere | resolve, then dial |
|---|---|---|---|---|
| HTTPChaos `replace: {code: 503}` on the callee's work path | 503 | 200 | **503** | both succeed |
| NetworkChaos `partition, direction: to` | 504 in 1.00s | 200 | 200 | resolve ✓, dial ✗ |
| DNSChaos `error` on one name | 502 | 200 | 200 | resolve ✗, **dial ✓** |

The last column is the whole argument for shipping the bottom two as separate
scenarios. Measured from inside the edge under DNSChaos: `nslookup
upstream.<ns>.svc.cluster.local` returns SERVFAIL, `nslookup
kubernetes.default.svc.cluster.local` answers normally, and
`wget http://<upstream ClusterIP>/cgi-bin/work` returns `upstream ok`. Nothing
is dropping packets; the caller never learns where to send any. Rolling the
fault back returns the edge to 2/2 with a restart count of 0.

### The first version did not work

The upstream originally served a static `return 200` from nginx. Under
StressChaos the cgroup sat at 100% of its quota — verifiably, in `kubectl top`
— and the latency did not move: 0.36s stressed against 0.36s idle. Answering
that request needs microseconds of CPU, and CFS hands out a slice every period
regardless of how much contention there is for it.

So the saturation fixture had nothing to saturate. A shell loop is a blunt
instrument, but it is the honest one: it gives the callee the property a real
saturation incident has, which is that serving a request costs CPU.

### Neither did the first attempt at a DNS fault

Chaos Mesh injects DNS chaos by rewriting the target pod's `/etc/resolv.conf`
to point at its own DNS server, which answers the configured patterns with an
error and forwards everything else. It does this to a pod that is already
running.

nginx reads `/etc/resolv.conf` once. With a literal `proxy_pass` host it does
not read it even then — it resolves the name at config load and holds the
address for the life of the process. Measured on GKE: the CR reported
`AllInjected=true`, `nslookup upstream` inside the caller failed, and the caller
served 200 the entire time. A fault that applies, reports success, and changes
nothing is the worst possible outcome for a rig whose job is to grade other
people's diagnoses.

Three edits fix it, and all three are needed:

| | |
|---|---|
| `resolver <addr>` in the config | nginx will not read resolv.conf for a nameserver, and a variable upstream without a `resolver` is a config error. The address is substituted in at startup from resolv.conf's first `nameserver` line. |
| `set $callee <fqdn>; proxy_pass http://$callee...` | A variable upstream is re-resolved per request, honouring the record's TTL. The name has to be fully qualified: nginx's resolver does not apply resolv.conf's `search` list, so a bare `upstream` is queried as an absolute name and NXDOMAINs. |
| A loop that watches resolv.conf and runs `nginx -s reload` | The injection changes the nameserver *after* nginx started. Without the reload, nginx keeps querying the pre-chaos resolver and the fault still does nothing. |

This is also what a real nginx deployment does, for the same reason, which is
the test the change had to pass before going in — a substrate detail that exists
only to make a fault land is fixture-fitting. It costs the other scenarios
nothing: when resolv.conf does not change, the loop is a string comparison every
two seconds.

`TestTheCallerResolvesTheCalleePerRequest` asserts all three, because any one of
them alone reads like a stylistic choice someone could tidy away.

## What is deliberately absent

**No load generator.** The efficacy probe drives the traffic it measures, so
the measurement and the load are on one clock. A background loadgen puts a
second, unobserved traffic source in the namespace, and a gate that fails
because the loadgen was between requests is a flake nobody can reproduce.

**No metrics stack.** Whether a subject can see CPU is a property of the
subject's tools, not of the fixture. A fixture that shipped its own
observability would be grading itself.
