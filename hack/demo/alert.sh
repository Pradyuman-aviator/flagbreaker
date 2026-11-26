#!/usr/bin/env bash
# Sends a Prometheus Alertmanager / Grafana alert to Flagsmith's Grafana
# Feature Health provider, labelled with a flag, the way an alert rule with
# flagsmith_feature and flagsmith_environment labels would.
#
#   hack/demo/alert.sh fire    [flag]   # the alert starts firing
#   hack/demo/alert.sh resolve [flag]   # the alert resolves
#
# The alert is staged for the demo (there is no Prometheus here); Flagsmith's
# handling of it, and Flagbreaker's, are real.

source "$(dirname "${BASH_SOURCE[0]}")/../lib.sh"
source "$ROOT_DIR/hack/demo/common.sh"

action=${1:-fire}
feature=${2:-express_checkout}
now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
case "$action" in
  fire) status=firing ends="0001-01-01T00:00:00Z" ;;
  resolve) status=resolved ends="$now" ;;
  *) die "usage: $0 fire|resolve [flag]" ;;
esac

payload="$(jq -n --arg s "$status" --arg f "$feature" --arg now "$now" --arg ends "$ends" '{
  receiver: "flagsmith", status: $s,
  alerts: [{
    status: $s,
    labels: {alertname: "CheckoutErrorRateHigh", severity: "page", flagsmith_feature: $f, flagsmith_environment: "Production"},
    annotations: {
      summary: "checkout 5xx rate is 7.2% (threshold 2%)",
      description: ("The error rate rose after " + $f + " was switched on."),
      runbook_url: "https://github.com/Pradyuman-aviator/flagbreaker/blob/main/docs/runbook.md"
    },
    startsAt: $now, endsAt: $ends,
    generatorURL: "http://prometheus.example/graph?g0.expr=checkout_5xx_ratio",
    fingerprint: ("checkout-error-rate-" + $f)
  }]
}')"

log "alert $status for $feature in Production"
curl -fsS -X POST -H "Content-Type: application/json" --data "$payload" "$API_URL$FLAGSMITH_GRAFANA_WEBHOOK_PATH" >/dev/null
