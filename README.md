# Flagbreaker

**A circuit breaker for Flagsmith flags on Kubernetes.** When a workload breaks
right after a flag is switched on, or given a new remote config value (its
pods crash or stop being ready, or, with request checking on, its requests
start failing while every pod stays up), Flagbreaker:
- reports it to [Flagsmith Feature Health](https://docs.flagsmith.com/advanced-use/feature-health);
- in act mode, switches the flag off, or for a bad value, puts the previous
  value back;
- restarts the crashed pods;
- confirms on the cluster that the workload recovered.

[![ci](https://github.com/Pradyuman-aviator/flagbreaker/actions/workflows/ci.yml/badge.svg)](https://github.com/Pradyuman-aviator/flagbreaker/actions/workflows/ci.yml)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
![Flagsmith 2.295.2](https://img.shields.io/badge/Flagsmith-2.295.2%2C%20self--hosted-6633ff)

<!-- Demo video: add the link here. -->

**No time to run it?** Each of these takes about a minute:
- the demo video;
- the [CI runs](https://github.com/Pradyuman-aviator/flagbreaker/actions/workflows/ci.yml). On every push to main and every pull request, an end-to-end job brings up kind, self-hosted Flagsmith and Flagbreaker from scratch, breaks the demo app and checks the recovery;
- the [real incident timeline](#try-it) below.

A flag flip, or a new remote config value, changes production without a
deploy:
- **Rollout tools can't see it.** Argo Rollouts and Flagger watch new
  ReplicaSets, and a flag flip creates none.
- **Nothing in open-source Flagsmith acts on it.** Flagsmith records
  Feature Health, but nothing in its open-source code acts on an Unhealthy
  event.

Flagbreaker is both halves:
- **A provider:** it sends Kubernetes workload health to Feature Health.
- **An actuator:** it turns Unhealthy, from any provider, into a switch-off
  that is checked on the cluster.

## Try it

You need Docker (with about 3 GB of memory free), bash, curl and jq. On
Windows, use Git Bash. The scripts download their own pinned kind, kubectl and
helm into `./bin`, checked against SHA-256 values in the repo, and never touch
your `~/.kube/config`.

```sh
git clone https://github.com/Pradyuman-aviator/flagbreaker && cd flagbreaker
bash hack/demo/up.sh        # kind + self-hosted Flagsmith + Flagbreaker + a demo shop; ~8 min the first time
bash hack/demo/watch.sh     # in a second terminal: a live view of flags, pods, guards and Feature Health
bash hack/demo/break.sh     # switch new_pricing on, the way a person would
bash hack/demo/break.sh checkout_config   # or give checkout_config a value the shop can't parse
bash hack/demo/break.sh new_payments   # payments fail; every pod stays up
bash hack/demo/down.sh      # delete it all
```

Open three things side by side and watch one incident:
- the Flagsmith dashboard, at http://localhost:8080;
- the shop, at http://localhost:8090. It stops answering while the pods
  crash, shows "Payment failed" while `new_payments` is on, and comes back
  on its own;
- `watch.sh`, in a terminal.

| Script | What it does |
|---|---|
| `hack/demo/up.sh` | Brings up everything. Safe to re-run. |
| `hack/demo/watch.sh` | Live terminal view: what Flagsmith serves, the shop, the pods, each guard's phase and last step, and Feature Health. |
| `hack/demo/break.sh [flag] [on\|off]` | Switches a flag the way a person would, with the admin's token, so Flagsmith records who did it. `break.sh new_payments` makes the shop's payments fail while its pods stay up. `break.sh checkout_config` sets a value the shop can't parse instead; `break.sh checkout_config good` puts the good one back. |
| `hack/demo/alert.sh fire\|resolve` | Sends a Grafana / Alertmanager alert for `express_checkout` to Flagsmith. |
| `hack/demo/restore.sh` | Asks Flagbreaker to put the flag back as it was. |
| `hack/demo/e2e-all.sh` | Every scenario, checked end to end: act mode with a restore, watch mode set from Flagsmith, a Grafana alert, a bad remote config value put back, and failing requests while every pod stays up. <!-- Add the duration measured on the first green CI run with e2e-errors.sh. --> |
| `hack/demo/down.sh` | Deletes the cluster and the local state. |

`up.sh` builds the whole demo through Flagsmith's API, with no clicks in the
dashboard:
- self-hosted Flagsmith 2.295.2, from its official Helm chart;
- an organisation, a project and a Production environment;
- the flags (one of them, `checkout_config`, is remote config), a segment, two
  Feature Health providers and a signed webhook;
- Flagbreaker, from the Helm chart in this repo;
- `checkout`, a small Go app that reads its flags with the Flagsmith SDK and
  counts its requests by status code on `/metrics`;
- `shopper`, the same app checking out 5 times a second, so there are
  requests that can fail.

At the end it prints the dashboard URL and an admin login. If ports 8000, 8080
or 8090 are taken, run `API_PORT=18000 UI_PORT=18080 APP_PORT=18090 bash hack/demo/up.sh`.

What happens after `break.sh`, from a real run on this demo. This is the
"Timeline (UTC)" block of the flag's Health tab in Flagsmith; the FlagGuard's
`status.timeline` holds the same steps.

To see them yourself, run
`KUBECONFIG=.demo/kubeconfig bin/kubectl get flagguard -n shop checkout-new-pricing -o yaml`.

```
13:05:17  new_pricing was switched on in Production by admin@flagbreaker.local
13:05:19  Deployment shop/checkout: 1 of 2 pods started failing 2s after the flag was switched on by admin@flagbreaker.local at 13:05:17 UTC
13:05:19  Switched new_pricing off in Production (its environment default and 1 segment override). Identity overrides and experiments are not changed.
13:05:19  Restarted 1 crashed pod, so the workload reads the flag again now instead of after the kubelet's back-off
13:05:42  Verified recovery: all 2 pods ready with no new crashes for 22s. Detected 2s after the change, switched off 0s after that, recovered 24s after the switch-off.
```

For the length of the incident, the flag also carries the **Unhealthy** tag
in Flagsmith's flag list. The demo sets `flagbreaker_mode` to `act`, so it
switches the flag off; out of the box, Flagbreaker only watches.

## How Flagsmith is used

Flagsmith is where every decision starts and ends. Take Flagsmith away and
Flagbreaker has nothing to watch, nothing to switch and nowhere to report.

| Flagsmith feature | What Flagbreaker does with it |
|---|---|
| Flags, through `/api/__future__/environments/{key}/features/{id}/` | Reads the flag. Switches off the environment default and every segment override that is on, in one versioned change; if Flagsmith refuses that, for example because of an experiment's override, one change per override. A restore turns them back on. |
| Remote config (flag values) | Notices when the environment default gets a new value, such as a JSON config. If the workload breaks right after, puts the previous value back and leaves the flag on: the SDK's `GetFeatureValue` returns a value whether or not the flag is on, so switching it off wouldn't help. |
| Feature Health, Webhook provider | Reports Unhealthy and Healthy with the incident timeline, the failing pods, the failed-request counts and runbook links, which show in the flag's Health tab and as the Unhealthy tag. |
| Feature Health events (the Grafana / Prometheus Alertmanager provider, or any other) | Trips on another provider's alert for the flag. Waits for the alert to clear before calling the workload recovered. Treats a person's **Dismiss** as an acknowledgement. |
| Environment webhooks (`FLAG_UPDATED`, HMAC-SHA256 signed) | Learns exactly when a flag changed and who changed it. |
| Remote config, read with the official Go SDK | Reads the breaker's own settings: `flagbreaker_enabled` (the brake), `flagbreaker_mode` (watch or act) and `flagbreaker_policy` (a mode per flag, and for guards that read requests a `maxErrorPercent`, as JSON). |
| Change requests | Flagbreaker never gets around them. Change requests come with Flagsmith's workflows module, which the open-source build doesn't include, so the demo can't show them. When an environment requires approvals, the `__future__` endpoint answers 409, and Flagbreaker drops to watch mode with a `BlockedByChangeRequests` condition. |
| Self-hosting | The demo runs Flagsmith from its official Helm chart and sets it up through the API. It also turns on the dashboard's own Feature Health screens, which stock self-hosted installs hide, by pointing the dashboard at a local Flagsmith-on-Flagsmith project. |

## Guard a workload

The one-line way is a pair of annotations. Flagbreaker creates a FlagGuard per
flag, owned by the workload:

```sh
kubectl annotate deployment checkout -n shop \
  flagbreaker.dev/flags=new_pricing,express_checkout \
  flagbreaker.dev/environment-key=<client-side environment key>
```

This works on Deployments, StatefulSets and DaemonSets. For every setting,
write the FlagGuard yourself:

```yaml
apiVersion: flagbreaker.dev/v1alpha1
kind: FlagGuard
metadata:
  name: checkout-new-pricing
  namespace: shop
spec:
  flag: new_pricing
  environmentKey: "<client-side environment key>"
  workload: {kind: Deployment, name: checkout}
  window: 10m        # how long after a switch-on a breakage is blamed on it
  failingPods: 1     # how many pods must fail
  stableFor: 30s     # how long the pods must stay healthy to count as recovered (the demo uses 20s)
  links:
    - {title: Runbook, url: "https://example.com/runbook"}
```

A namespace may act only on environments a cluster admin has allowed for it.
Flagbreaker uses an organisation API key, so this keeps one team from
switching off another team's flags:

```sh
kubectl annotate namespace shop flagbreaker.dev/allowed-environments=<key>
```

**Watch or act.** Flagbreaker asks three places:
- the guard's `spec.mode`;
- `flagbreaker_policy` for that flag;
- `flagbreaker_mode`.

The most cautious answer wins. It switches a flag off only when something says
`act` and nothing says `watch`. Out of the box, and whenever Flagsmith can't
be read, it watches: it reports and changes nothing. `flagbreaker_enabled`
off pauses it everywhere.

**Turning the flag back on is up to a person.** Fix the cause, then either:
- switch the flag on in Flagsmith;
- or restore it exactly as it was, including the segment overrides
  Flagbreaker switched off:

  ```sh
  kubectl annotate flagguard -n shop checkout-new-pricing flagbreaker.dev/restore=$(date +%s) --overwrite
  ```

  In the demo, `bash hack/demo/restore.sh` runs this for you.

Either way the flag is watched again, and a second breakage trips the breaker
again. The [runbook](docs/runbook.md) covers each step.

## Catch failing requests, not just crashes

Kubernetes only knows whether a pod is alive. A bad flag often keeps every
pod alive and makes requests fail instead: the checkout page says "Payment
failed" while `/healthz` still says ok. Most web apps already count their
requests by status code. Give a FlagGuard `spec.requests`, and Flagbreaker
reads that counter straight from each of the workload's pods, as Prometheus
would scrape it, with no Prometheus server:

```yaml
spec:
  flag: new_payments
  environmentKey: "<client-side environment key>"
  workload: {kind: Deployment, name: checkout}
  requests:
    port: 8080                    # each pod's metrics port
    path: /metrics                # the default
    metric: http_requests_total   # the default: a counter, or a histogram's or summary's _count
    statusLabel: code             # the default: the label with the HTTP status
    maxErrorPercent: 5            # the default: more failed requests than this is failing
    minRequests: 20               # the default: fewer requests are too few to judge
```

For a guard made from annotations, one more annotation does it, with
`flagbreaker.dev/requests-path`, `requests-metric` and
`requests-status-label` for the rest:

```sh
kubectl annotate deployment checkout -n shop flagbreaker.dev/requests-port=8080
```

- **What counts as failed.** Every series of the metric counts toward the
  total; a series whose status label starts with 5 counts as failed. 4xx
  counts as handled, not failed.
- **The rule.** After a switch-on or a new value, once at least
  `minRequests` requests arrived and more than `maxErrorPercent` of them
  failed, the change is blamed, even with every pod up and ready. That holds
  only if the minute before the change was fine: at least `minRequests`
  requests, with no more than `maxErrorPercent` failing.
- **The rest is as for pods.** The same `window`, rollout and
  already-failing rules apply. Act mode switches the flag off, or puts the
  previous value back; it restarts nothing, since nothing crashed. Feature
  Health gets the counts.
- **Recovery is checked on the requests too.** Requests failing in the last
  minute mean the workload isn't healthy yet. After an incident failing
  requests caused, at least `minRequests` requests must go through over
  `stableFor`, with no more than the limit failing; without traffic, the
  guard stays tripped.
- **Tune the limit from Flagsmith.** `{"new_payments": {"mode": "act",
  "maxErrorPercent": 2}}` in `flagbreaker_policy` replaces the guard's limit
  for that flag, decimals allowed. It never turns request reading on by
  itself.
- **NetworkPolicies.** Flagbreaker reads each pod's metrics port from its own
  pod. Where NetworkPolicies deny ingress by default, allow it:

  ```yaml
  apiVersion: networking.k8s.io/v1
  kind: NetworkPolicy
  metadata:
    name: allow-flagbreaker-metrics
    namespace: shop
  spec:
    podSelector:
      matchLabels: {app: checkout}
    policyTypes: [Ingress]
    ingress:
      - from:
          - namespaceSelector:
              matchLabels: {kubernetes.io/metadata.name: flagbreaker-system}
            podSelector:
              matchLabels: {app.kubernetes.io/name: flagbreaker}
        ports:
          - {port: 8080, protocol: TCP}
  ```

In the demo, `bash hack/demo/break.sh new_payments` switches on a payment
provider that fails every payment. The incident timeline says how many
requests failed, against how many in the minute before, and that every pod
stayed ready; the recovery entry says how many went through.

<!-- Paste here the timeline that hack/demo/e2e-errors.sh prints ("passed. Timeline:") in the first green CI run, as a code block like the new_pricing one above. Copy it; don't type numbers. -->

## Install in your cluster

Each release publishes the chart and a multi-arch image (amd64 and arm64) to
GHCR, through [release.yml](.github/workflows/release.yml). To install the
latest one:

```sh
helm install flagbreaker oci://ghcr.io/pradyuman-aviator/charts/flagbreaker --version 0.2.0 \
  -n flagbreaker-system --create-namespace \
  --set flagsmith.url=http://flagsmith-api.flagsmith:8000 \
  --set flagsmith.apiKey=<organisation API key> \
  --set flagsmith.healthWebhookPath=/api/v1/feature-health/<path> \
  --set flagsmith.settingsEnvironmentKey=<environment holding the flagbreaker_* flags>
kubectl annotate namespace <namespace> flagbreaker.dev/allowed-environments=<client-side environment key>
```

To upgrade, apply the new CRD first, because `helm upgrade` never updates
CRDs. Without it, the API server drops the status fields the new version
writes, such as the flag's value.

```sh
kubectl apply --server-side --force-conflicts -f https://raw.githubusercontent.com/Pradyuman-aviator/flagbreaker/v0.2.0/charts/flagbreaker/crds/flagbreaker.dev_flagguards.yaml
helm upgrade flagbreaker oci://ghcr.io/pradyuman-aviator/charts/flagbreaker --version 0.2.0 \
  -n flagbreaker-system --reuse-values
```

To run your own build instead, push the image to a registry your cluster
pulls from (on kind, use `kind load docker-image`), then install the chart
from this repo with the same `flagsmith.*` settings:

```sh
docker build -t <registry>/flagbreaker:dev . && docker push <registry>/flagbreaker:dev
kubectl apply --server-side -f charts/flagbreaker/crds/
helm install flagbreaker ./charts/flagbreaker -n flagbreaker-system --create-namespace \
  --set image.repository=<registry>/flagbreaker --set image.tag=dev  # plus the flagsmith.* settings above
```

- **`healthWebhookPath`.** Create a Webhook provider under Project Settings
  → Feature Health, or call `POST /api/v1/projects/{id}/feature-health/providers/`
  with `{"name": "Webhook"}`. Keep only the path of the `webhook_url` it
  returns; the path is the credential.
- **Webhooks (optional, for exact change times and authors).**
  - Point an environment webhook at `http://flagbreaker-webhook.flagbreaker-system.svc:8082/webhooks/flagsmith`.
  - Give it a secret, and pass the same secret as `flagsmith.webhookSecret`.
  - Add that host to Flagsmith's `SSRF_ALLOWED_HOSTS`.
- **More settings.** See [values.yaml](charts/flagbreaker/values.yaml).

## What's real and what's staged

- **Real:**
  - Flagsmith 2.295.2, with its API, webhooks, Feature Health and dashboard;
  - the Kubernetes cluster and the pods crashing;
  - the switch-off, the pod restarts and the recovery check;
  - the shop's request counter, Flagbreaker reading it from each pod, and the
    requests that fail.
- **Staged:**
  - The demo app's bugs: `checkout` exits while `new_pricing` is on, and when
    `checkout_config` holds a value it can't parse, and its payments fail
    while `new_payments` is on.
  - The shoppers: a loop that checks out 5 times a second.
  - The Grafana alert, which `hack/demo/alert.sh` posts to Flagsmith's
    Grafana provider in the format Alertmanager uses. There's no Prometheus
    in the demo.

## Why not Argo Rollouts, Flagger, or an Alertmanager rule?

- **Argo Rollouts and Flagger** roll back deploys. A flag flip is not a deploy,
  so they never see it.
  - If the workload rolled out any time from 30 s before a flag change
    onward, Flagbreaker doesn't blame the flag; it only reports.
- **An Alertmanager rule** can already mark a flag Unhealthy through
  Flagsmith's Grafana provider, but nothing in open-source Flagsmith then
  switches the flag off. Flagbreaker acts on those alerts too. For failing
  requests, it needs neither a Prometheus server nor a rule per flag: it
  reads the app's own counter, and it knows which flag changed.
- **Flagsmith's Release Pipelines** can abort on an unhealthy flag, but they
  are a closed beta. Flagbreaker is open source and guards any flag at any
  time, and it checks recovery on the workload itself, because an API call
  that succeeds proves nothing about the pods.

## Under the hood

```mermaid
stateDiagram-v2
  [*] --> Armed
  Armed --> Tripped: pods fail or requests fail after a switch-on or a new value, or another provider's alert fires
  Tripped --> Recovered: pods healthy and requests not failing for stableFor, and no alert firing
  Tripped --> Armed: a person re-enables the flag or dismisses the incident
  Recovered --> Armed: the flag is on again, it gets a new value, or at once after an incident a new value alone caused
```

- **The decisions** live in one pure function,
  [`breaker.Decide`](internal/breaker/decide.go).
- **The controller** is built on controller-runtime, with leader election,
  Kubernetes Events and Prometheus metrics, such as
  `flagbreaker_detection_seconds` and `flagbreaker_recovery_seconds`.
- **The design notes,** in [docs/design.md](docs/design.md), cover the full
  rules, every Flagsmith endpoint, the safety measures and the metrics.
- **Tests.** 168 Go tests cover the rules, the client, webhook verification,
  reading request counters, and whole incidents run through `Reconcile`
  against a fake Flagsmith, including one per bug that two independent code
  reviews found. On every
  push to main and every pull request, CI runs them, plus an end-to-end job
  that brings up the demo on a fresh kind cluster and runs every scenario.
- **Where it has run.**
  - It has run end to end on Windows 11 (Git Bash, Docker Desktop) and on
    Ubuntu in CI.
  - On macOS, the scripts are checked under its bash 3.2 and every image is
    multi-arch, but it hasn't been run end to end there yet.

## Limitations

- **Only the environment default is tracked: switching it, and changing its
  value.** A rollout through a segment override alone, such as a `% Split`
  segment, isn't tracked, and other providers' alerts trip a guard only while
  the environment default is on. Values of segment overrides and multivariate
  weights aren't tracked either.
- **Identity overrides and experiments are never switched off.** The timeline
  says so.
- **Who made a change can be missing.** Flagsmith sends `FLAG_UPDATED` with
  the state at send time, a few seconds after the change. If Flagbreaker
  switches the flag off within those seconds, the timeline may give the
  person's change without a name.
- **Feature Health is a beta, and `/api/__future__` is marked experimental.**
  The demo pins Flagsmith 2.295.2.
- **Unlicensed self-hosted Flagsmith allows one project and one seat per
  organisation.** So the demo uses an organisation API key, not a
  least-privilege user.
- **Request checking needs a counter Flagbreaker can reach.** The workload
  must serve a Prometheus text-format counter on a port of each pod that the
  controller can reach: no TLS, no auth, no protobuf, no named ports.
- **The request history lives in memory.** A change seen before a
  controller restart isn't blamed for failing requests; the pods rule still
  applies to it.
- **It takes traffic.** `minRequests` requests are needed in the minute
  before a change and in the time after it, or failing requests aren't
  blamed.
- **Only 5xx counts.** A wrong page served with 200, or a change that stops
  requests from arriving at all, isn't seen.
- **An outage elsewhere can be blamed.** If a downstream service starts
  failing within the window of a change, the change gets the blame. Watch
  mode, the minute-before check and Dismiss limit the damage.
- **Segment-only rollouts are still untracked,** as above, for requests too.

## Tried it?

Feedback is welcome, especially from macOS and Apple Silicon, where the demo
hasn't run end to end yet. If something doesn't work, open an
[issue](https://github.com/Pradyuman-aviator/flagbreaker/issues) with your OS,
your Docker version and the last lines `up.sh` printed.

## License

[Apache-2.0](LICENSE)
