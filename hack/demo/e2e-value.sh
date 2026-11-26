#!/usr/bin/env bash
# End-to-end check of a bad remote config value, in act mode:
#   1. The checkout Deployment's flagbreaker.dev/flags annotation made the
#      FlagGuard checkout-checkout-config.
#   2. A person sets checkout_config to a value the app can't parse; the pods
#      crash.
#   3. Flagbreaker trips and puts the previous value back. The flag stays on.
#      It restarts the crashed pods and reports Unhealthy to Feature Health.
#   4. It verifies recovery on the cluster, reports Healthy and watches the
#      flag again.
# Run after hack/demo/up.sh.

source "$(dirname "${BASH_SOURCE[0]}")/../lib.sh"
source "$ROOT_DIR/hack/demo/common.sh"
G=checkout-checkout-config
ID=${FLAGSMITH_CONFIG_ID:-}
[[ -n "$ID" ]] || die "this demo predates checkout_config; run hack/demo/up.sh again"

value_now() { fs "$(flag_path "$ID")" | jq -r '.environment_default.value.value // ""'; }

reset_demo
log "1. the annotation on deployment/checkout made FlagGuard $G"
wait_for $G 60 '.metadata.labels["flagbreaker.dev/generated"] == "true" and (.status.phase // "") != ""'
require_settled_guard $G
wait_for_settled_rollout
for _ in $(seq 1 30); do
  [[ "$(guard $G | jq -r '.status.flagValue.value // ""')" == "$GOOD_CONFIG" ]] && break
  sleep 1
done
[[ "$(guard $G | jq -r '.status.flagValue.value // ""')" == "$GOOD_CONFIG" ]] ||
  die "Flagbreaker should have seen checkout_config's good value: $(guard $G | jq -c .status.flagValue)"

log "2. setting checkout_config to $BAD_CONFIG, as $FLAGSMITH_ADMIN_EMAIL"
set_value "$ID" "$BAD_CONFIG"
start=$(date +%s)
wait_for_phase $G Tripped 90
log "   tripped after $(($(date +%s) - start))s"

log "3. checking that the previous value is back and the flag is still on"
[[ "$(value_now)" == "$GOOD_CONFIG" ]] || die "checkout_config should have its previous value again, has: $(value_now)"
[[ "$(jq -r .default <<<"$(flag "$ID")")" == true ]] || die "putting the value back must not switch the flag off"
guard $G | jq -e '[.status.timeline[].event] | index("ValueReverted") != null' >/dev/null ||
  die "the timeline should record the value put back: $(guard $G | jq -c '[.status.timeline[].event]')"
wait_for_health "$ID" UNHEALTHY 30

log "4. waiting for verified recovery"
wait_for $G 240 '[.status.timeline[].event] | index("Recovered") != null'
log "   recovered $(($(date +%s) - start))s after the change"
wait_for_health "$ID" HEALTHY 30
# Nothing is left to wait for: the flag is on with a value that works.
wait_for_phase $G Armed 30

log "passed. Timeline:"
print_timeline $G
