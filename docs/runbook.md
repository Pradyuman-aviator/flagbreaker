# Runbook: Flagbreaker tripped

Flagbreaker links here from the flag's **Health** tab in Flagsmith. The event
above the link says what happened; this page says what to do next.

## 1. Read the event

The Unhealthy event's first block names the workload and the cause:

- **The workload broke after this flag changed.** Pods crashed or stopped
  being ready within `window` (10 minutes by default) of the flag being
  switched on, and they were fine before.
- **The workload broke after this flag's value changed.** The same, for a
  new remote config value. The timeline shows the old and the new value.
- **Requests to the workload started failing after this flag changed** (or
  after its value changed). The guard reads the workload's request counter,
  and more than `maxErrorPercent` of at least `minRequests` requests failed
  with 5xx after the change, where the minute before was fine. The pods may
  all be up and ready.
- **`<provider>` reported "`<alert>`" for this flag.** Another Feature Health
  provider, such as Grafana or Prometheus Alertmanager, marked the flag
  unhealthy.

The **Timeline (UTC)** block lists every step, and the same timeline is on the
FlagGuard:

```sh
kubectl get flagguards -A
kubectl get flagguard -n <namespace> <name> -o jsonpath='{range .status.timeline[*]}{.time}  {.message}{"\n"}{end}'
```

For failing requests, the **Requests** block gives the counts of the minute
up to the report. The guard keeps the latest ones in `status.requests`, and
`kubectl get flagguards` shows the error rate in its **Errors** column. The
`RequestsRead` condition says whether the last read worked.

## 2. If the flag is still on (watch mode)

Watch mode reports and changes nothing. The event says which setting chose
watch mode. To stop the incident, switch the flag off in Flagsmith yourself,
or, if a new value caused it, set a value that works (the timeline shows the
previous one). If a new value alone caused it, switching the flag off doesn't
end the incident: apps read the value either way. If the flag was also
switched on just before, switching it off does.
Flagbreaker notices who switched it off, restarts the pods that crashed, and
reports Healthy once the workload has been healthy for `stableFor`.

If the flag isn't the cause, **dismiss** the Unhealthy event in the Health
tab. Flagbreaker treats that as an acknowledgement: it watches the flag again
and won't blame the same change twice. That can happen with failing requests:
if a service the workload calls goes down within `window` of the change, the
change gets the blame. Check the services it depends on before turning the
flag back on.

## 3. If Flagbreaker switched the flag off (act mode)

It switched off the environment default and every segment override that was
on. It doesn't change identity overrides or experiments; the timeline says so.
If a new value caused the incident, it put the previous value back instead and
left the flag as it was. Then it restarted the crashed pods so they read the
flag again straight away.

Wait for **Recovered**: every pod ready, with no new crashes, for
`stableFor`. If the cause was another provider's alert, that alert must also
resolve, or be dismissed. On a guard that reads requests, requests failing
over `maxErrorPercent` hold it back too: always after an alert, and after
failing pods if the requests were fine in the minute before the change.

If failing requests were the cause, no pod crashed, so none is restarted.
Recovery then waits for requests to succeed: at least `minRequests` of them
over `stableFor`, with no more than `maxErrorPercent` failing. Without
traffic, the guard stays tripped.

## 4. Turn the flag back on

After a bad value was put back, the flag has its previous value again, and
is on or off as it was before; Flagbreaker watches it again. Fix the value, then set it in
Flagsmith; if it breaks the workload again, the breaker trips again.

After a switch-off, fix the cause first. Then do one of these:

- **Restore it exactly as it was**, including the segment overrides
  Flagbreaker switched off:

  ```sh
  kubectl annotate flagguard -n <namespace> <name> flagbreaker.dev/restore=$(date +%s) --overwrite
  ```

  Only overrides that still exist and are still off are turned back on, so a
  change someone made since the incident is kept. The restore counts as a
  flag change: if the workload breaks again, the breaker trips again.

- **Switch it on in Flagsmith.** Flagbreaker watches it again as soon as it's
  on. Segment overrides it switched off stay off: the timeline says how many,
  `status.snapshot.enabledSegmentOverrides` lists their segment IDs, and the
  restore annotation still turns them back on.

## 5. Keep it from acting

| Scope | How |
|---|---|
| Everything | Switch `flagbreaker_enabled` off in Flagsmith |
| Every flag, report only | Set `flagbreaker_mode` to `watch` |
| One flag | Add it to `flagbreaker_policy`: `{"<flag>": {"mode": "watch"}}`, or `"off"` to ignore it |
| One workload | `spec.mode: watch` on its FlagGuard, or the `flagbreaker.dev/mode: watch` annotation |
| How many failed requests trip it | `maxErrorPercent` in `flagbreaker_policy`, such as `{"<flag>": {"maxErrorPercent": 10}}`, or in the FlagGuard's `spec.requests`, which a guard made from annotations keeps. Keep it above the workload's usual error rate: where more fail anyway, failing requests trip nothing and don't hold the recovery from failing pods, but an incident an alert caused doesn't recover while they fail |
| Failing requests, for one workload | Remove `spec.requests` from its FlagGuard (or the `flagbreaker.dev/requests-port` annotation); the pods are still watched |

The most cautious setting wins. Flagbreaker never gets around change requests:
if an environment requires them, it drops to watch mode and sets the
`BlockedByChangeRequests` condition.
