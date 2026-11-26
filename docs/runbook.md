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
- **`<provider>` reported "`<alert>`" for this flag.** Another Feature Health
  provider, such as Grafana or Prometheus Alertmanager, marked the flag
  unhealthy.

The **Timeline (UTC)** block lists every step, and the same timeline is on the
FlagGuard:

```sh
kubectl get flagguards -A
kubectl get flagguard -n <namespace> <name> -o jsonpath='{range .status.timeline[*]}{.time}  {.message}{"\n"}{end}'
```

## 2. If the flag is still on (watch mode)

Watch mode reports and changes nothing. The event says which setting chose
watch mode. To stop the incident, switch the flag off in Flagsmith yourself,
or, if a new value caused it, set a value that works (the timeline shows the
previous one). Switching the flag off doesn't end a value incident: apps read
the value either way.
Flagbreaker notices who switched it off, restarts the pods that crashed, and
reports Healthy once the workload has been healthy for `stableFor`.

If the flag isn't the cause, **dismiss** the Unhealthy event in the Health
tab. Flagbreaker treats that as an acknowledgement: it watches the flag again
and won't blame the same change twice.

## 3. If Flagbreaker switched the flag off (act mode)

It switched off the environment default and every segment override that was
on. It doesn't change identity overrides or experiments; the timeline says so.
If a new value caused the incident, it put the previous value back instead and
left the flag as it was. Then it restarted the crashed pods so they read the
flag again straight away.

Wait for **Recovered**: every pod ready, with no new crashes, for
`stableFor`. If the cause was another provider's alert, that alert must also
resolve, or be dismissed.

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

The most cautious setting wins. Flagbreaker never gets around change requests:
if an environment requires them, it drops to watch mode and sets the
`BlockedByChangeRequests` condition.
