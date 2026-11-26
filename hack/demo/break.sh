#!/usr/bin/env bash
# Switches a flag on (new_pricing by default) in Production, the way a person
# would: with the admin user's token, so Flagsmith records who made the
# change. The demo app crashes while new_pricing is on, and Flagbreaker
# should take it from there. For checkout_config, it sets a value instead.
#
#   hack/demo/break.sh                       # new_pricing on
#   hack/demo/break.sh express_checkout      # another flag on
#   hack/demo/break.sh new_pricing off       # off again, by hand
#   hack/demo/break.sh checkout_config       # a value the app can't parse
#   hack/demo/break.sh checkout_config good  # the good value again, by hand

source "$(dirname "${BASH_SOURCE[0]}")/../lib.sh"
source "$ROOT_DIR/hack/demo/common.sh"

name=${1:-new_pricing}
to=${2:-on}
case "$name" in
  new_pricing) id=$FLAGSMITH_NEW_PRICING_ID ;;
  express_checkout) id=$FLAGSMITH_EXPRESS_CHECKOUT_ID ;;
  checkout_config)
    id=${FLAGSMITH_CONFIG_ID:-}
    [[ -n "$id" ]] || die "this demo predates checkout_config; run hack/demo/up.sh again"
    value=$BAD_CONFIG
    [[ "${2:-bad}" == good ]] && value=$GOOD_CONFIG
    log "setting checkout_config to $value in Production as $FLAGSMITH_ADMIN_EMAIL"
    set_value "$id" "$value"
    fs "$(flag_path "$id")" | jq -c '{enabled: .environment_default.enabled, value: .environment_default.value.value}'
    log "watch it: KUBECONFIG=.demo/kubeconfig bin/kubectl get flagguards -n shop -w"
    exit 0
    ;;
  *) die "unknown demo flag: $name" ;;
esac

log "switching $name $to in Production as $FLAGSMITH_ADMIN_EMAIL"
switch "$id" "$to"
flag "$id"
log "watch it: KUBECONFIG=.demo/kubeconfig bin/kubectl get flagguards -n shop -w"
