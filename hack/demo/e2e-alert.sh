#!/usr/bin/env bash
# End-to-end check of following another Feature Health provider:
#   1. The checkout Deployment's flagbreaker.dev/flags annotation made the
#      FlagGuard checkout-express-checkout.
#   2. A person switches express_checkout on. The pods are fine, so nothing
#      happens.
#   3. A Grafana / Alertmanager alert labelled with the flag fires. Flagsmith
#      marks the flag Unhealthy; Flagbreaker sees that and switches it off.
#   4. It stays tripped until the alert resolves, then reports Healthy.
# Run after hack/demo/up.sh.

source "$(dirname "${BASH_SOURCE[0]}")/../lib.sh"
source "$ROOT_DIR/hack/demo/common.sh"
G=checkout-express-checkout
ID=$FLAGSMITH_EXPRESS_CHECKOUT_ID
ALERT="$ROOT_DIR/hack/demo/alert.sh"

log "1. the annotation on deployment/checkout made FlagGuard $G"
wait_for $G 60 '.metadata.labels["flagbreaker.dev/generated"] == "true" and (.status.phase // "") != ""'
case "$(phase $G)" in
  Armed | Recovered) ;;
  *) die "FlagGuard $G should be Armed or Recovered before the test, is $(phase $G)" ;;
esac
bash "$ALERT" resolve >/dev/null # clear any alert left from an earlier run

log "2. switching express_checkout on, as $FLAGSMITH_ADMIN_EMAIL; the pods stay healthy"
switch "$ID" on
wait_for $G 30 '.status.flagEnabled == true'
sleep 10
[[ "$(phase $G)" != Tripped ]] || die "healthy pods and no alert must not trip the guard"

log "3. firing a Grafana alert for express_checkout"
bash "$ALERT" fire
wait_for_health "$ID" UNHEALTHY 30 Grafana
start=$(date +%s)
wait_for_phase $G Tripped 60
log "   tripped $(($(date +%s) - start))s after Flagsmith stored the alert"
[[ "$(guard $G | jq -r .status.alert.provider)" == Grafana ]] || die "the trip should name the Grafana alert"
[[ "$(jq -r .default <<<"$(flag "$ID")")" == false ]] || die "express_checkout should be off"
wait_for_health "$ID" UNHEALTHY 30

log "4. the pods are healthy, but recovery waits for the alert to resolve"
sleep 25
[[ "$(phase $G)" == Tripped ]] || die "the guard must stay tripped while the alert fires"
bash "$ALERT" resolve
wait_for_phase $G Recovered 90
wait_for_health "$ID" HEALTHY 30

log "passed. Timeline:"
print_timeline $G
