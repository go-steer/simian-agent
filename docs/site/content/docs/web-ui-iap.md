---
title: "The web UI behind Identity-Aware Proxy"
linkTitle: "Web UI behind IAP"
weight: 26
description: "Put Simian's web UI on a real hostname behind Identity-Aware Proxy on GKE, so the people you choose can watch it and inject and clear faults from a browser."
---

By default the web UI is read-only and reached by port-forward (see
[Seeing what Simian is doing]({{< relref "observability.md#in-a-browser-the-web-ui" >}})).
This page puts it on a hostname behind
[Identity-Aware Proxy](https://cloud.google.com/iap/docs/concepts-overview) on
GKE. IAP decides who may see the page; `ui.iap.writers` decides who may also
inject and clear faults, each recorded in the audit trail in their name.

Only the UI's port goes behind the load balancer. The MCP endpoint stays
unauthenticated inside the cluster.

It follows the [Getting started]({{< relref "getting-started.md" >}}) GKE
path through step 5: a cluster with Chaos Mesh, Simian v0.3 or later
installed, and an arena. Each step has a **Check** and an **If not**.

## 1. Choose a hostname, a certificate and a load balancer

| | Gateway API (validated end to end) | Ingress |
|---|---|---|
| Certificate | one you already have in Certificate Manager, through a certificate map — a wildcard, say | a Google-managed certificate the chart requests for the hostname |
| Cluster | the Gateway API on (`--gateway-api=standard`) | nothing extra |
| Chart values | `ui.iap.gateway.*` | `ui.iap.ingress.*` |

Use one, not both. The rest of the page shows the Gateway path and notes
where the Ingress differs.

```bash
export GOOGLE_CLOUD_PROJECT=<your-project>
export UI_HOST=simian.example.com            # the name users will open
export CERT_MAP=<your-certificate-map>       # Gateway only
export PROJECT_NUMBER=$(gcloud projects describe "$GOOGLE_CLOUD_PROJECT" --format='value(projectNumber)')
```

**Check:** `echo $PROJECT_NUMBER` prints only digits. For the Gateway,
`gcloud certificate-manager maps entries list --map "$CERT_MAP"` has an
`ACTIVE` entry whose hostname covers `$UI_HOST` (`*.example.com`, say).
**If not:** a project ID where the number should be is refused when the
controller starts; the map needs an entry for the name or a wildcard over it.

## 2. Reserve an address and point the name at it

```bash
gcloud compute addresses create simian-ui --global --project "$GOOGLE_CLOUD_PROJECT"
gcloud compute addresses describe simian-ui --global --project "$GOOGLE_CLOUD_PROJECT" --format='value(address)'
```

Create a DNS `A` record for `$UI_HOST` with that address, wherever your
domain's DNS is.

**Check:** `getent hosts $UI_HOST` prints the address.
**If not:** wait for the record to propagate. With the Ingress path the
certificate is not issued until it resolves.

For the Gateway path, turn the Gateway API on if it is not:

```bash
gcloud container clusters update "$CLUSTER" --gateway-api=standard \
    --zone "$ZONE" --project "$GOOGLE_CLOUD_PROJECT"
```

**Check:** `kubectl get gatewayclass` lists `gke-l7-global-external-managed`.
**If not:** it appears a few minutes after the update; look again.

## 3. Turn on IAP and the load balancer

```bash
helm upgrade simian deploy/helm/simian -n simian-system --reuse-values \
    --set ui.auth=iap --set ui.iap.projectNumber="$PROJECT_NUMBER" \
    --set 'ui.iap.writers={you@example.com}' \
    --set ui.iap.gateway.enabled=true --set ui.iap.gateway.host="$UI_HOST" \
    --set ui.iap.gateway.certificateMap="$CERT_MAP" \
    --set ui.iap.gateway.staticIPName=simian-ui \
    --wait --timeout 4m
```

For the Ingress path, replace the four `ui.iap.gateway.*` values with
`ui.iap.ingress.enabled=true`, `ui.iap.ingress.host="$UI_HOST"` and
`ui.iap.ingress.staticIPName=simian-ui`.

`ui.iap.writers` takes emails and `domain:example.com`. Leave it empty for a
page everyone can view and nobody can write from. Add
`--set 'ui.iap.admins={you@example.com}'` for the people who may also turn
autonomous mode on, configure, pause and resume it, and clear all faults
from the page's Configuration panel.

**Check:**

```bash
kubectl -n simian-system get gateway simian-ui        # PROGRAMMED True, ADDRESS the reserved IP
kubectl -n simian-system describe gcpbackendpolicy simian-ui | grep -A3 Conditions   # Attached True
```

**If not:** `kubectl -n simian-system describe gateway simian-ui` names what
is missing — most often the certificate map's name, or a Gateway API that is
not on yet.

## 4. Let people in

IAP admits no one until they hold **IAP-secured Web App User**
(`roles/iap.httpsResourceAccessor`). Grant it on the UI's backend service
only:

```bash
BACKEND=$(gcloud compute backend-services list --global --project "$GOOGLE_CLOUD_PROJECT" \
    --filter="name~simian-system-simian-controller" --format='value(name)')
gcloud iap web add-iam-policy-binding --resource-type=backend-services --service="$BACKEND" \
    --member=user:you@example.com --role=roles/iap.httpsResourceAccessor \
    --project "$GOOGLE_CLOUD_PROJECT"
```

Groups and `domain:` members work too. A project-wide grant of the role also
works and outlives the backend service.

**IAP's default sign-in admits only accounts in your project's organization.**
The Google-managed OAuth client it uses without further setup authorizes no
one outside it, whatever the grant says: they sign in and see *You don't have
access*. To admit other accounts, see [Accounts outside the organization](#accounts-outside-the-organization).

**Check:** from a machine with no session,

```bash
curl -s -o /dev/null -w '%{http_code} %{redirect_url}\n' https://$UI_HOST/ui/
```

prints `302` and a `https://accounts.google.com/...` URL: IAP is in front.
A new load balancer can take five to ten minutes before it answers at all.
**If not:** `gcloud compute backend-services get-health "$BACKEND" --global`
should say `HEALTHY`; the health check is `/healthz` on the UI's port.

## 5. Open it

Open `https://$UI_HOST/ui/` and sign in. The top right shows your email; a
writer sees **Inject a fault** and a **Clear** button on each running fault,
anyone else a note that they may view.

**Check:** inject a `PodChaos` into a workload of your arena. It appears
under Active faults, and in the audit export with your email:

```bash
kubectl -n simian-system exec deploy/simian-controller -- \
    simian audit export --format json /var/lib/simian/audit.jsonl | grep requested_by
```

**If not:** a page that says the request was refused names the reason:

| It says | Cause |
|---|---|
| `audience … is not a backend service of project …` | `ui.iap.projectNumber` is not the project's number |
| `… may view but not submit or clear faults` | the account is not in `ui.iap.writers` |
| *You don't have access* (IAP's page) | no IAP grant, or an account outside the organization |
| `max concurrent faults reached` | a fault is already running; one at a time is the default (`executor.maxConcurrentFaults`) |

## What protects it

- **IAP** stands in front of every path. The controller still checks each
  request's IAP assertion itself — its ES256 signature against IAP's keys,
  its issuer, and that its audience is a backend service of your project — so
  nothing that reaches the pod around the load balancer gets in. Only
  `/healthz` answers without one. `ui.iap.audience` pins the exact backend
  service instead of any in the project.
- **Writes** need the `X-Simian-UI` header, which a page on another site
  cannot send with your IAP cookie — unless the controller lists that page
  in `ui.allowedOrigins` ([several Simians](#several-simians-in-one-ui)).
- **Every fault from the page goes through the executor**: arenas only, no
  excluded workloads, within the duration and concurrency limits.

## Several Simians in one UI

One page can watch and drive several Simians — one controller per cluster,
say. The browser calls each controller directly, signed in to each one's IAP
as you, so each still checks who you are and records what you do in its own
audit trail; nothing sits in between. Pick one from the switcher next to the
arena, or click it in the strip along the bottom, which shows every Simian's
state, running faults, autonomous mode and your role there.

**1. Choose the page.** Either one of the controllers' own pages, with the
others listed in its chart values:

```bash
helm upgrade simian deploy/helm/simian -n simian-system --reuse-values \
    --set 'ui.controllers[0].name=simian-2' \
    --set 'ui.controllers[0].url=https://simian-2.example.com'
```

or the standalone UI — the same image, running `simian web` instead of
`simian serve`, behind IAP of its own like any page:

```bash
simian web --addr :8080 \
    --controllers simian-1=https://simian.example.com \
    --controllers simian-2=https://simian-2.example.com
```

Either way users can add more by URL in the page (**⧉**, or the name in the
header), kept in their browser. A URL is the controller UI's origin, e.g.
`https://simian-2.example.com`.

**2. Let the page call each controller.** Every controller the page should
reach lists the page's origin in `ui.allowedOrigins`:

```bash
helm upgrade simian deploy/helm/simian -n simian-system --reuse-values \
    --set 'ui.allowedOrigins={https://simian.example.com}'
```

Only listed origins get CORS headers, with credentials; any other page gets
answers it cannot read.

**3. Let IAP pass the CORS preflight.** Reads go straight through, but
injecting, clearing and halting send `X-Simian-UI`, so the browser asks first
with an `OPTIONS` request that carries no cookie — and IAP turns that away
unless told not to. On each controller's backend service (`$BACKEND` from
step 4):

```bash
cat > iap-cors.yaml <<'YAML'
access_settings:
  cors_settings:
    allow_http_options: true
YAML
gcloud iap settings set iap-cors.yaml --resource-type=backend-services \
    --service="$BACKEND" --project "$GOOGLE_CLOUD_PROJECT"
```

The controller answers the preflight itself, for listed origins only; the
real request still needs IAP's assertion. Whether `GCPBackendPolicy` (the
Gateway path) or `BackendConfig` (Ingress) can set this instead of
`gcloud iap settings` is still to be confirmed on GKE.

**4. Sign in once per controller.** IAP signs you in only on a visit to the
controller's own page, not on a call from another page. A Simian you have
not signed in to shows **not allowed / sign in**, with a **Sign in to …**
button that opens its page in a new tab; come back and the page tries again.
The same state means the controller does not list this page in
`ui.allowedOrigins` — the browser does not say which, so the page names
both.

**Check:** the strip shows each Simian **live** with your role there;
inject a fault on one, and its audit export (step 5) has your email in
`requested_by` — on that controller, not the page's.

**If not:** writes fail with *did not answer this page* while reads work:
the preflight is not getting through IAP (step 3). Every call fails: sign
in to that Simian, then check its `ui.allowedOrigins` lists the page's
origin exactly (scheme, host and any port).

Controllers on hostnames under one domain (`simian.example.com`,
`simian-2.example.com`) are one site to the browser; across different
sites, a browser that blocks third-party cookies may not send IAP's cookie
with the call.

## Accounts outside the organization

Give IAP an OAuth client of your own, with an **External** audience, instead
of the Google-managed one:

1. In the console, **Google Auth Platform** → **Audience**: *External*.
2. **Clients** → **Create client** → *Web application*, with the authorized
   redirect URI `https://iap.googleapis.com/v1/oauth/clientIds/CLIENT_ID:handleRedirect`
   (with the client's ID in place of `CLIENT_ID`, once it has one).
3. Put its secret in a Secret, from your own terminal — not in a chat or a
   shared log:

   ```bash
   kubectl -n simian-system create secret generic simian-iap-oauth --from-literal=key=CLIENT_SECRET
   ```

4. Name the client to the chart:

   ```bash
   helm upgrade simian deploy/helm/simian -n simian-system --reuse-values \
       --set ui.iap.oauthClient.clientID=CLIENT_ID \
       --set ui.iap.oauthClient.secretName=simian-iap-oauth
   ```

Then grant the outside accounts as in step 4.

## Turning it off

```bash
helm upgrade simian deploy/helm/simian -n simian-system --reuse-values \
    --set ui.iap.gateway.enabled=false --set ui.auth=none --set 'ui.iap.writers={}'
gcloud compute addresses delete simian-ui --global --project "$GOOGLE_CLOUD_PROJECT"
```

The IAP grants go with the backend service. Remove the DNS record yourself.
