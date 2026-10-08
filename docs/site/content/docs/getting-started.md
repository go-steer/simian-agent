---
title: "Getting started"
linkTitle: "Getting started"
weight: 10
description: "From nothing to watching Simian break a real application, check the damage and record exactly what it did — on GKE (about 30 minutes) or on kind on your laptop (about 15)."
---

Simian is a chaos engineering controller for Kubernetes. You point it at a
namespace — an *arena* — and it injects faults into what runs there: kills a
pod, slows a network path, starves a container of CPU, breaks a rollout. It
checks that each fault actually took effect, takes it back out on time, checks
that the workload recovered, and records all of it in an audit trail. It can
plan faults itself with an LLM, or apply the ones you ask for.

This page takes you from an empty project to all of that, once on **GKE** and
once on **kind** on your laptop. The two paths differ only in steps 2 and 3;
everything from step 4 on is the same.

> **Written as a runbook.** Every step has a **Check** with the output to
> expect, and an **If not** for the common failure. Follow it in order and do
> not move on past a failing check. An agent setting Simian up for someone
> should also read [`AGENTS.md`](https://github.com/go-steer/simian-agent/blob/main/AGENTS.md)
> first.

## What you need

| | GKE | kind |
|---|---|---|
| Tools | `git`, Go 1.26+, `make`, `kubectl`, `helm` 3, `curl`, `gcloud` (authenticated) | `git`, Go 1.26+, `make`, `kubectl`, `helm` 3, `curl`, `docker`, `kind` |
| Access | a GCP project where you can create GKE clusters and grant IAM roles | Docker with at least 4 CPUs and 8 GB of memory |
| LLM | Vertex AI in that project (step 3 grants the controller access) | a Gemini API key, exported as `GEMINI_API_KEY` |
| Cost | a 3-node `e2-standard-4` cluster while it runs, plus Vertex calls | Gemini API calls only |

Steps 1–7 need no LLM at all; steps 8 and 9 do.

## 1. Get Simian

```bash
git clone --depth 1 --branch v0.3.0 https://github.com/go-steer/simian-agent
cd simian-agent
make build
bin/simian --help
```

Everything below runs from this directory. The chart, the CLI and the
published image all come from the same release.

**Check:** the help lists `arena`, `sut`, `chaos`, `watch` and `audit` among
its commands.
**If not:** `make build` needs Go 1.26 or newer (`go version`).

## 2. A cluster with Chaos Mesh

Simian drives [Chaos Mesh](https://chaos-mesh.org/) for most of its faults, so
the cluster needs it installed.

### On GKE

```bash
export GOOGLE_CLOUD_PROJECT=<your-project>   # where the cluster and Vertex live
export ZONE=us-central1-a
export CLUSTER=simian-quickstart
export KUBECONFIG=$PWD/.kube/quickstart.yaml # this cluster's credentials only

gcloud services enable container.googleapis.com aiplatform.googleapis.com \
    --project "$GOOGLE_CLOUD_PROJECT"
gcloud container clusters create "$CLUSTER" \
    --project "$GOOGLE_CLOUD_PROJECT" --zone "$ZONE" \
    --machine-type e2-standard-4 --num-nodes 3 \
    --enable-dataplane-v2 \
    --workload-pool="$GOOGLE_CLOUD_PROJECT.svc.id.goog"
gcloud container clusters get-credentials "$CLUSTER" \
    --project "$GOOGLE_CLOUD_PROJECT" --zone "$ZONE"

helm upgrade --install chaos-mesh https://charts.chaos-mesh.org/chaos-mesh-2.8.4.tgz \
    -n chaos-mesh --create-namespace \
    --set chaosDaemon.runtime=containerd \
    --set chaosDaemon.socketPath=/run/containerd/containerd.sock \
    --set dnsServer.create=true \
    --wait
```

Cluster creation takes 5–10 minutes. `--enable-dataplane-v2` makes the cluster
enforce NetworkPolicies, which Simian's `network-policy` engine needs;
`--workload-pool` lets the controller reach Vertex AI with its own identity in
step 3. Keeping `KUBECONFIG` in the checkout means nothing here can touch
another cluster you have credentials for. Autopilot clusters cannot run Chaos
Mesh — see [GKE bring-up]({{< relref "gke-bring-up.md" >}}).

### On kind

```bash
make cluster                          # kind + Calico + Chaos Mesh, about 3 minutes
export KUBECONFIG=$PWD/.kube/e2e.yaml # written by make cluster
```

This creates a three-node kind cluster named `simian-e2e-dev`, and writes its
credentials only to `.kube/e2e.yaml`; `make cluster-down` in step 12 deletes
that cluster and nothing else. Other kind clusters on the machine are left
alone.

### Check (both)

```bash
kubectl -n chaos-mesh get pods
```

`chaos-controller-manager-…` (three of them), `chaos-dns-server-…`,
`chaos-dashboard-…` and one `chaos-daemon-…` per worker node (three on GKE,
two on kind, whose control-plane node runs none), all `Running`.
**If not:** wait a minute and look again; on GKE the daemons start after the
nodes report Ready.

## 3. Install the controller

### On GKE: Vertex AI through Workload Identity

```bash
dev/tools/grant-vertex-access   # binds roles/aiplatform.user to the controller's identity

helm upgrade --install simian deploy/helm/simian -n simian-system --create-namespace \
    -f examples/values-baked-defaults.yaml \
    --set llm.vertex.project="$GOOGLE_CLOUD_PROJECT" \
    --set metrics.podMonitoring.enabled=true \
    --wait --timeout 4m
```

The grant is project-wide and outlives the cluster; step 12 revokes it.
`metrics.podMonitoring.enabled` has GKE's Managed Service for Prometheus
collect Simian's metrics (step 10).

### On kind: a Gemini API key

```bash
kubectl create namespace simian-system
kubectl -n simian-system create secret generic simian-llm \
    --from-literal=geminiApiKey="$GEMINI_API_KEY"

helm upgrade --install simian deploy/helm/simian -n simian-system \
    -f examples/values-baked-defaults.yaml \
    --set llm.vertex.enabled=false --set llm.apiKey.enabled=true \
    --wait --timeout 4m
```

### Check (both)

```bash
kubectl -n simian-system get pods,pvc
```

`simian-controller-…` is `1/1 Running` and `persistentvolumeclaim/simian-audit`
is `Bound`. The overlay `examples/values-baked-defaults.yaml` is the
recommended starting point: one fault at a time, at most five minutes each,
namespace-scoped faults only.
**If not:** `kubectl -n simian-system logs deploy/simian-controller`. A
`helm` error naming `llm.vertex.project` means the `--set` above was missed.

## 4. Connect the CLI

The CLI talks to the controller over its MCP endpoint. Forward it locally and
tell the CLI where it is:

```bash
kubectl -n simian-system port-forward svc/simian-controller 18081:8081 >/tmp/simian-pf.log 2>&1 &
echo $! >/tmp/simian-pf.pid
export SIMIAN_MCP_URL=http://localhost:18081/sse
sleep 2; curl -s -m 3 -o /dev/null -w "%{http_code}\n" "$SIMIAN_MCP_URL" || true
```

**Check:** `200`. The endpoint streams, so curl stops after three seconds and
exits non-zero (28) even when it worked; `|| true` keeps that from looking
like a failure — the status code is the check.
**If not:** `cat /tmp/simian-pf.log`. If the port is taken, pick another
local port in both lines. A `404` means something other than Simian answered
on that port.

The port-forward breaks whenever the controller pod is replaced — a `helm
upgrade`, a node upgrade — but does not exit until the next connection
through it fails, and it keeps the local port until then. To restart it, stop
the old one first:

```bash
kill "$(cat /tmp/simian-pf.pid)" 2>/dev/null
kubectl -n simian-system port-forward svc/simian-controller 18081:8081 >/tmp/simian-pf.log 2>&1 &
echo $! >/tmp/simian-pf.pid
```

## 5. An arena with an application in it

Deploy Google's [Online Boutique](https://github.com/GoogleCloudPlatform/microservices-demo)
into a new arena called `boutique`. `--use-controller` has the controller take
a *baseline* of the healthy application; it needs one before it will run
faults there on its own. The annotation keeps faults off the load generator,
which drives the traffic the application needs to look alive.

```bash
bin/simian sut deploy --namespace boutique --create-arena --use-controller \
    --annotation simian.chaos/exclude-workloads=loadgenerator
```

This takes 1–5 minutes: it waits for every workload to be Ready and then for a
stability window.

**Check:**

```bash
bin/simian arena describe boutique   # eligible: true, excluded: loadgenerator
bin/simian baseline show --namespace boutique | head -c 300; echo
kubectl -n boutique get pods         # every pod Running and Ready
```

**If not:** `sut deploy` prints which workload never became Ready. On kind,
image pulls are the usual cause of a slow first deploy; run it again.

## 6. Your first fault

Kill one pod of `productcatalogservice`, and have Simian take the fault back
out after a minute:

```bash
bin/simian chaos --namespace boutique --workload productcatalogservice \
    --kind PodChaos --spec '{"action":"pod-kill","mode":"one"}' --duration 60s
```

No LLM is involved: `--kind` and `--spec` describe the fault exactly. Simian
still runs it through its safety checks — the namespace must be an arena, the
workload must not be excluded, the fault must fit the duration and severity
caps — then applies it, and confirms with Chaos Mesh that it was injected.

**Check:** the command prints the fault's UID — `{"fault_uid":"f-01M…"}` —
and within a few seconds

```bash
bin/simian chaos --list-active --namespace boutique   # the fault, with its deadline
kubectl -n boutique get pods -l app=productcatalogservice   # a pod a few seconds old
```

For a live view, run `bin/simian watch --namespace boutique` in a second
terminal (it redraws; `Ctrl-C` to leave). It shows the active faults, what
autonomous mode decided (empty until step 9), and the recent faults and how
they ended.

## 7. Read what happened

Once the minute is up, ask the controller for its record. The controller
checks deadlines on a 30-second sweep, so a fault can end up to half a minute
after its deadline; until then its row shows `open`.

```bash
kubectl -n simian-system exec deploy/simian-controller -- \
    simian audit export /var/lib/simian/audit.jsonl
```

One row per fault:

```
FAULT                       STARTED               SOURCE    KIND      TARGETS                         OUTCOME  REASON           ENDED                 INJECTED  EFFICACY  RECOVERED  SPEC
f-01M4…                     2026-10-06T14:02:11Z  directed  PodChaos  boutique/productcatalogservice  expired  deadline-reached  2026-10-06T14:03:11Z  yes       -         yes        {"action":"pod-kill",…
```

| Column | Meaning |
|---|---|
| `OUTCOME` / `REASON` | how the fault ended: `expired deadline-reached` is the normal end; `refused` means the safety checks turned it down, with the reason; `cleared` means it was taken out early |
| `INJECTED` | Chaos Mesh's own word that the fault took |
| `EFFICACY` | Simian's probes saw the effect — a connection timing out under a partition, say. `-` when the kind has no probe: a killed pod is self-evident |
| `RECOVERED` | after the fault ended, the targeted pods were Ready again within five minutes. **`no` is the first thing to look at** — the fault left something broken |

`--format json` gives the same rows with the full spec, and the pods that did
not recover and why. The controller's log has every step as an event
(`kubectl -n simian-system logs deploy/simian-controller | grep '"msg":"audit"'`).

The record lives on the controller's volume and survives restarts; this is
what you compare an agent's diagnosis against.

## 8. Ask in plain English

The rest of the page needs the LLM.

```bash
bin/simian chaos --namespace boutique \
    --intent "stress the CPU of cartservice for two minutes"
```

The controller turns the sentence into a concrete fault (here a
`StressChaos`), validates it like any other, and applies it.

**Check:** a fault UID, and after two minutes a `StressChaos` row in the audit
export with `expired deadline-reached`.
**If not:** a `permission denied` from Vertex on GKE usually means the grant
in step 3 has not propagated yet — wait two minutes and retry. On kind, the
controller's log says what Gemini answered
(`kubectl -n simian-system logs deploy/simian-controller | grep -i gemini`);
an error about the API key means the key in the `simian-llm` secret is wrong
or is not a Gemini API key. Recreate the secret as in step 3 and restart the
controller (`kubectl -n simian-system rollout restart deploy/simian-controller`).

## 9. Let it plan on its own

Autonomous mode is the point of Simian: every cycle it checks the arena is
healthy, reads its topology, asks the LLM for a hypothesis and a plan, and
runs the plan within the safety caps. Turn it on with a short cycle so you can
watch a few:

```bash
helm upgrade simian deploy/helm/simian -n simian-system --reuse-values \
    --set autonomous.enabled=true --set 'autonomous.namespaces={boutique}' \
    --set autonomous.cycleInterval=3m --wait
kill "$(cat /tmp/simian-pf.pid)" 2>/dev/null
kubectl -n simian-system port-forward svc/simian-controller 18081:8081 >/tmp/simian-pf.log 2>&1 &
echo $! >/tmp/simian-pf.pid
```

(The upgrade replaces the controller pod, hence the new port-forward. If the
step 8 fault is still running, the new controller adopts it and its first
cycle waits for it to end: one fault at a time.)

**Check:** within a few minutes, watch what it decides:

```bash
bin/simian watch --namespace boutique   # redraws every few seconds; Ctrl-C to leave
```

The `AUTONOMOUS CYCLES` section shows the latest hypothesis and plan in full,
then one line per cycle with what it applied or why it was skipped:

```
AUTONOMOUS CYCLES (4)
  latest plan (18:28:10):
    If the adservice is unavailable, the frontend will handle the
    error gracefully and continue to serve user requests for other
    product features without significant latency or errors.
    1. PodChaos → adservice for 1m30s
       why that long: The pod-kill action is instantaneous. The 90-second duratio…
  18:28:10  completed PodChaos→adservice (1 applied)
  18:26:10  completed NetworkChaos→paymentservice (1 applied)
  18:24:10  skipped   budget-full
  18:22:10  completed NetworkChaos→currencyservice (1 applied)
```

The same as a table, which suits an agent better than a redrawing view:

```bash
kubectl -n simian-system exec deploy/simian-controller -- \
    simian audit export --cycles /var/lib/simian/audit.jsonl
```

A skipped cycle says why: `health-gate` (something in the arena is not Ready,
often the previous fault's pod still restarting), `budget-full` (one fault at
a time), `no-valid-plan` (the LLM's plans failed validation). A cycle shown
as `open` is still planning or applying.

Turn it off again before you leave it unattended, unless that is the point:

```bash
helm upgrade simian deploy/helm/simian -n simian-system --reuse-values \
    --set autonomous.enabled=false --wait
```

## 10. Metrics

The controller serves Prometheus metrics on port `9090`:

```bash
kubectl -n simian-system port-forward svc/simian-controller 19090:9090 >/tmp/simian-metrics-pf.log 2>&1 &
echo $! >/tmp/simian-metrics-pf.pid
sleep 2; curl -s http://localhost:19090/metrics | grep '^simian_'
```

**Check:** `simian_build_info{version="0.3.0"} 1`, and counters for the arena
such as `simian_faults_applied_total{arena="boutique",…,kind="PodChaos",…}` and
`simian_cycles_total{arena="boutique",outcome="completed",…}`. They exist from
the start, at `0` until something happens. They count since the controller
started, and turning autonomous mode off at the end of step 9 restarted it, so
expect zeros here; the audit export in step 7 is the lasting record.

### On GKE: in Cloud Monitoring

The PodMonitoring from step 3 sends the same metrics to Managed Service for
Prometheus. After a few minutes they are queryable in the console (Metrics
Explorer, PromQL: `simian_faults_applied_total`), or from the command line:

```bash
curl -s -H "Authorization: Bearer $(gcloud auth print-access-token)" \
  "https://monitoring.googleapis.com/v1/projects/$GOOGLE_CLOUD_PROJECT/location/global/prometheus/api/v1/query?query=sum%20by%20(kind)(simian_faults_applied_total)"
```

**Check:** a JSON result with one series per fault kind in the catalog, at
`0` for the kinds not applied yet.

**If not:** `kubectl -n simian-system describe podmonitoring simian-controller`
shows whether the endpoint is being scraped; collection can take five minutes
to appear.

For a dashboard of it all, import the one Simian ships:

```bash
gcloud monitoring dashboards create --project "$GOOGLE_CLOUD_PROJECT" \
    --config-from-file=deploy/dashboards/cloud-monitoring.json
```

It appears as **Simian** in the console under Monitoring → Dashboards. (Step 12
deletes it.)

[Seeing what Simian is doing]({{< relref "observability.md" >}}) lists every
metric, with queries and alerts worth having — above all
`simian_fault_recovery_checks_total{passed="false"}`, a fault that left
something broken.

## 11. Point your agent at it

Simian makes the faults; diagnosing them is the job of whatever agent your
team is building. With autonomous mode on, ask your agent to assess the
`boutique` namespace, then compare its answer with the audit export: the
export says exactly what was injected, where and when.

Give the agent read access **without** the `chaos-mesh.org` API group, or it
can read the answer straight off Chaos Mesh's objects in the arena — see
[Agents that triage the cluster]({{< relref "deploy.md#agents-that-triage-the-cluster" >}}).
To score agents systematically, against scenario packs with ground truth, see
the [eval substrate]({{< relref "eval-substrate.md" >}}).

## 12. Clean up

### On GKE

Autonomous mode is off since step 9, but its last fault may still be running.
Wait until `bin/simian chaos --list-active --namespace boutique` shows none,
so the uninstall does not leave a Chaos Mesh experiment behind; then:

```bash
kill "$(cat /tmp/simian-pf.pid)" "$(cat /tmp/simian-metrics-pf.pid)" 2>/dev/null   # the port-forwards
helm uninstall simian -n simian-system
kubectl -n simian-system delete pvc simian-audit   # the audit volume is kept on uninstall
dev/tools/grant-vertex-access --revoke
gcloud monitoring dashboards list --project "$GOOGLE_CLOUD_PROJECT" --filter='displayName="Simian"' --format='value(name)' \
  | xargs -r -n1 gcloud monitoring dashboards delete --quiet   # if you imported it in step 10
gcloud container clusters delete "$CLUSTER" --project "$GOOGLE_CLOUD_PROJECT" --zone "$ZONE" --quiet
rm -f .kube/quickstart.yaml
```

### On kind

```bash
kill "$(cat /tmp/simian-pf.pid)" "$(cat /tmp/simian-metrics-pf.pid)" 2>/dev/null   # the port-forwards
make cluster-down
```

**Check:** `gcloud container clusters list --project "$GOOGLE_CLOUD_PROJECT"`
no longer lists the cluster (GKE), or `kind get clusters` no longer lists it
(kind).

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `helm` fails: `llm.vertex.project is required` | step 3 without `--set llm.vertex.project` | add it, or use the API-key path |
| CLI: `connection refused` | the port-forward died (controller pod replaced) | rerun the port-forward line from step 4 |
| CLI: `unexpected status code: 404` | something else listens on that local port | forward to another port and update `SIMIAN_MCP_URL` |
| Vertex `PERMISSION_DENIED` | grant not propagated, or Workload Identity missing | wait two minutes; check `--workload-pool` was set at cluster creation |
| every cycle skipped `no baseline cached` | the arena was deployed without `--use-controller` | `bin/simian baseline establish --namespace boutique` |
| a fault `refused target-incompatible` | Simian can tell the fault cannot run safely there — e.g. HTTPChaos on a gRPC port, IOChaos on a read-only filesystem | expected; the reason names the container. Autonomous mode learns from it |
| a fault `cleared probe-failed` | its effect was never observed, so it was rolled back | for `NetworkPolicy`, the cluster must enforce NetworkPolicies (step 2's `--enable-dataplane-v2`) |
| `RECOVERED no` | the fault left a pod broken | the JSON export names the pods; deleting them restores them |
| CLI: `tool … returned error` | the controller refused or failed the request | the line above it says why; a `budget-exceeded` means a fault is still running (wait for it), an LLM error is usually transient (retry) |

## Next

- [Deploying with Helm]({{< relref "deploy.md" >}}) — your own workloads as arenas, and production settings.
- [Using the chaos engines]({{< relref "chaos-engines.md" >}}) — every fault kind, directed and autonomous.
- [CLI reference]({{< relref "cli-reference.md" >}}) and [Helm values]({{< relref "helm-values.md" >}}).
- [Known limitations]({{< relref "known-limitations.md" >}}) — read before relying on a fault kind.
