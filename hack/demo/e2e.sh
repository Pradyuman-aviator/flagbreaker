#!/usr/bin/env bash
# End-to-end check in act mode, on the demo cluster:
#   1. A person switches new_pricing on; the checkout pods crash.
#   2. Flagbreaker trips, switches the flag off for everyone, including the
#      beta_testers segment override, restarts the crashed pods, and reports
#      Unhealthy to Flagsmith Feature Health.
#   3. It verifies recovery on the cluster and reports Healthy.
#   4. The restore annotation turns the flag back on exactly as it was. The
#      bug is still there, so the breaker trips again and recovers again.
# Exits non-zero on any failure. Run after hack/demo/up.sh.

source "$(dirname "${BASH_SOURCE[0]}")/../lib.sh"
source "$ROOT_DIR/hack/demo/common.sh"
G=checkout-new-pricing
ID=$FLAGSMITH_NEW_PRICING_ID
SEG=$FLAGSMITH_BETA_SEGMENT_ID

case "$(phase $G)" in
  Armed | Recovered) ;;
  *) die "FlagGuard $G should be Armed or Recovered before the test, is $(phase $G)" ;;
esac
wait_for_settled_rollout
# Start from the state bootstrap.sh made: off for everyone, on for beta testers.
as_person PATCH "$(flag_path "$ID")" "{\"segment_overrides\": [{\"segment\": {\"id\": $SEG}, \"enabled\": true}]}" >/dev/null
set_value "$FLAGSMITH_POLICY_ID" '{}'

log "1. switching new_pricing on in Production, as $FLAGSMITH_ADMIN_EMAIL"
switch "$ID" on
start=$(date +%s)
wait_for_phase $G Tripped 90
log "   tripped after $(($(date +%s) - start))s"

log "2. checking the complete switch-off and the Unhealthy report"
state="$(flag "$ID")"
[[ "$(jq -r .default <<<"$state")" == false ]] || die "the environment default should be off: $state"
[[ "$(jq -r --arg s "$SEG" '.segments[$s]' <<<"$state")" == false ]] || die "the beta_testers override should be off: $state"
wait_for_health "$ID" UNHEALTHY 30
[[ "$(guard $G | jq -r '.status.snapshot.enabledSegmentOverrides | index('"$SEG"') != null')" == true ]] ||
  die "the snapshot should remember the beta_testers override"

log "3. waiting for verified recovery"
wait_for_phase $G Recovered 240
log "   recovered $(($(date +%s) - start))s after the change"
wait_for_health "$ID" HEALTHY 30
print_timeline $G

log "4. restoring with the flagbreaker.dev/restore annotation"
"$KUBECTL" annotate flagguard $G -n shop flagbreaker.dev/restore="$(date +%s)" --overwrite >/dev/null
wait_for $G 30 '.status.phase == "Armed" and (.status.snapshot == null)'
state="$(flag "$ID")"
[[ "$(jq -r .default <<<"$state")" == true ]] || die "restore should turn the default back on: $state"
[[ "$(jq -r --arg s "$SEG" '.segments[$s]' <<<"$state")" == true ]] || die "restore should turn the beta_testers override back on: $state"
log "   restored; the bug is still there, so the breaker must trip again"
wait_for_phase $G Tripped 90
[[ "$(guard $G | jq -r '.status.trigger.changedBy')" == "flagbreaker (restore)" ]] || die "the second trip should blame the restore"
[[ "$(jq -r .default <<<"$(flag "$ID")")" == false ]] || die "the flag should be off again"
wait_for_phase $G Recovered 240

log "passed. Timeline of the second incident:"
print_timeline $G
