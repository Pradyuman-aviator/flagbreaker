#!/usr/bin/env bash
# End-to-end check of watch mode, set for one flag from Flagsmith:
#   1. flagbreaker_policy says new_pricing is watch-only.
#   2. A person switches new_pricing on; the pods crash. Flagbreaker trips and
#      reports Unhealthy, but leaves the flag on.
#   3. A person switches the flag off, as the Health tab asks. Flagbreaker
#      notices who did it, restarts the crashed pods, verifies recovery and
#      reports Healthy.
# Run after hack/demo/up.sh.

source "$(dirname "${BASH_SOURCE[0]}")/../lib.sh"
source "$ROOT_DIR/hack/demo/common.sh"
G=checkout-new-pricing
ID=$FLAGSMITH_NEW_PRICING_ID

case "$(phase $G)" in
  Armed | Recovered) ;;
  *) die "FlagGuard $G should be Armed or Recovered before the test, is $(phase $G)" ;;
esac
wait_for_settled_rollout
trap 'set_value "$FLAGSMITH_POLICY_ID" "{}"' EXIT

log "1. setting flagbreaker_policy in Flagsmith: new_pricing is watch-only"
set_value "$FLAGSMITH_POLICY_ID" '{"new_pricing": {"mode": "watch"}}'
sleep 6 # the settings cache lives 5s

log "2. switching new_pricing on, as $FLAGSMITH_ADMIN_EMAIL"
switch "$ID" on
wait_for_phase $G Tripped 90
[[ "$(guard $G | jq -r .status.mode)" == watch ]] || die "the trip should be in watch mode: $(guard $G | jq -c .status)"
wait_for_health "$ID" UNHEALTHY 30
sleep 10
[[ "$(jq -r .default <<<"$(flag "$ID")")" == true ]] || die "watch mode must leave the flag on"
[[ "$(phase $G)" == Tripped ]] || die "the guard should stay tripped while the flag is on and the pods crash"
log "   tripped in watch mode; the flag is still on: $(guard $G | jq -r .status.modeReason)"

log "3. switching new_pricing off, as a person would after reading the Health tab"
switch "$ID" off
wait_for $G 30 '.status.flagDisabledAt != null'
wait_for_phase $G Recovered 240
wait_for_health "$ID" HEALTHY 30
guard $G | jq -e '[.status.timeline[].event] | index("FlagSwitchedOffByPerson") != null' >/dev/null ||
  die "the timeline should record the person's switch-off"

log "passed. Timeline:"
print_timeline $G
