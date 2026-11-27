#!/usr/bin/env bash
# End-to-end check of failing requests, in act mode:
#   1. The FlagGuard checkout-new-payments reads http_requests_total from each
#      checkout pod. The shopper Deployment checks out 5 times a second, and
#      every checkout goes through.
#   2. A person switches new_payments on. Payments fail with HTTP 500, but
#      every pod stays up and ready.
#   3. Flagbreaker trips on the failing requests, switches the flag off and
#      reports Unhealthy to Feature Health with the counts. No pod crashed,
#      so it restarts none.
#   4. It verifies recovery on the requests, reports Healthy, and the shop
#      answers again. The pods are the same ones throughout.
# Run after hack/demo/up.sh.

source "$(dirname "${BASH_SOURCE[0]}")/../lib.sh"
source "$ROOT_DIR/hack/demo/common.sh"
G=checkout-new-payments
ID=${FLAGSMITH_NEW_PAYMENTS_ID:-}
[[ -n "$ID" ]] || die "this demo predates new_payments; run hack/demo/up.sh again"

# pods_now -> the checkout pods' names and restart counts, sorted
pods_now() {
  "$KUBECTL" get pods -n shop -l app=checkout -o json |
    jq -c '[.items[] | select(.metadata.deletionTimestamp == null)
      | {name: .metadata.name, restarts: ([.status.containerStatuses[]?.restartCount] | add // 0)}] | sort_by(.name)'
}

# webhook_text -> the text blocks of Flagbreaker's latest Feature Health event for the flag
webhook_text() {
  fs "/api/v1/projects/$FLAGSMITH_PROJECT_ID/feature-health/events/" |
    jq -r --argjson f "$ID" '(.results? // .) | map(select(.feature == $f and .provider_name == "Webhook"))
      | sort_by(.created_at) | last | [.reason.text_blocks[]? | "\(.title // "")\n\(.text)"] | join("\n")'
}

reset_demo
require_settled_guard $G
wait_for_settled_rollout
# e2e-value can leave a crash on a checkout pod that Flagbreaker didn't
# restart; in the minute before the switch-on, it would mean the workload
# was already failing.
wait_for_crash_free_minute

# The rule needs 20 checkouts in the minute before the switch-on, and that
# minute ends 5s before the change, which polling may date up to 5s early.
# 100 in the last minute, 20s of shopping, leave enough for that.
log "1. waiting for a clean baseline: at least 100 checkouts in the last minute, none failed"
wait_for $G 150 '(.status.requests.total // 0) >= 100 and .status.requests.failed == 0 and any(.status.conditions[]?; .type == "RequestsRead" and .status == "True")'
log "   $(guard $G | jq -r '.status.requests | "\(.total) checkouts in the last minute, \(.failed) failed, read from \(.podsRead) pods"')"
before="$(pods_now)"

log "2. switching new_payments on, as $FLAGSMITH_ADMIN_EMAIL; the pods stay up and ready"
switch "$ID" on
start=$(date +%s)
wait_for_phase $G Tripped 90
log "   tripped after $(($(date +%s) - start))s"

log "3. checking the trip, the switch-off and the Unhealthy report"
guard $G | jq -e '[.status.timeline[] | select(.event == "Tripped") | .message] | any(test("requests failed"))' >/dev/null ||
  die "the trip should be about failing requests: $(guard $G | jq -c '[.status.timeline[].message]')"
[[ "$(guard $G | jq -r '.status.requestsIncident')" == true ]] || die "status.requestsIncident should be true: $(guard $G | jq -c .status)"
guard $G | jq -e '[.status.timeline[].event] | index("PodsRestarted") == null' >/dev/null ||
  die "no pod crashed, so none should be restarted: $(guard $G | jq -c '[.status.timeline[].event]')"
[[ "$(jq -r .default <<<"$(flag "$ID")")" == false ]] || die "new_payments should be off"
[[ "$(pods_now)" == "$before" ]] || die "the checkout pods should be the same ones, with the same restarts: $(pods_now), was $before"
wait_for_health "$ID" UNHEALTHY 30
text="$(webhook_text)"
grep -q '[Rr]equests' <<<"$text" || die "the Unhealthy event should be about requests: $text"

log "4. waiting for verified recovery on the requests"
wait_for_phase $G Recovered 120
log "   recovered $(($(date +%s) - start))s after the switch-on"
guard $G | jq -e '[.status.timeline[] | select(.event == "Recovered") | .message] | any(test("requests failed in that time"))' >/dev/null ||
  die "the recovery should count the requests: $(guard $G | jq -c '[.status.timeline[].message]')"
wait_for_health "$ID" HEALTHY 30
code="$(curl -s -o /dev/null -w '%{http_code}' -m 5 "$APP_URL" || true)"
[[ "$code" == 200 ]] || die "the shop should answer again, answers $code"
[[ "$(pods_now)" == "$before" ]] || die "the checkout pods should be the same ones, with the same restarts: $(pods_now), was $before"

log "passed. Timeline:"
print_timeline $G
