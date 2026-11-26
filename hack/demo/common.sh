# Helpers for the demo and e2e scripts. Source this after hack/lib.sh.
# shellcheck shell=bash

require curl jq
[[ -f "$DEMO_DIR/flagsmith.env" ]] || die "no demo state in .demo/; run hack/demo/up.sh first"
# shellcheck disable=SC1091
source "$DEMO_DIR/flagsmith.env"

# guard NAME -> the FlagGuard as JSON
guard() { "$KUBECTL" get flagguard "$1" -n shop -o json; }
phase() { guard "$1" | jq -r '.status.phase // ""'; }

# require_settled_guard NAME: waits until Flagbreaker has checked the guard
# and it is Armed or Recovered, the states a test starts from. A guard left
# tripped by an earlier run recovers once its flag is off.
require_settled_guard() {
  local name=$1 i p
  for ((i = 0; i < 150; i++)); do
    p="$(phase "$name" 2>/dev/null || true)"
    case "$p" in
      Armed | Recovered) return 0 ;;
    esac
    sleep 1
  done
  guard "$name" | jq '.status' >&2 || true
  die "FlagGuard $name should be Armed or Recovered before the test, is '${p:-unchecked}'"
}

# The value bootstrap.sh gives checkout_config, and one the app can't parse.
GOOD_CONFIG='{"tax_rate": 0.08}'
# shellcheck disable=SC2034 # used by the scripts that source this file
BAD_CONFIG='{"tax_rate": "8%"}'

# reset_demo: new_pricing off, checkout_config good and the checkout app up,
# whatever an earlier (perhaps failed) run left behind.
reset_demo() {
  switch "$FLAGSMITH_NEW_PRICING_ID" off
  [[ -n "${FLAGSMITH_CONFIG_ID:-}" ]] && set_value "$FLAGSMITH_CONFIG_ID" "$GOOD_CONFIG"
  if ! "$KUBECTL" wait --for=condition=Available deployment/checkout -n shop --timeout=10s >/dev/null 2>&1; then
    log "the checkout app is down from an earlier run; restarting its pods"
    "$KUBECTL" delete pod -n shop -l app=checkout --wait=false >/dev/null
    "$KUBECTL" rollout status deployment/checkout -n shop --timeout=3m >/dev/null
  fi
}

# wait_for_phase GUARD PHASE TIMEOUT_SECONDS
wait_for_phase() {
  local name=$1 want=$2 timeout=$3 i
  for ((i = 0; i < timeout; i++)); do
    [[ "$(phase "$name")" == "$want" ]] && return 0
    sleep 1
  done
  guard "$name" | jq '.status' >&2
  die "FlagGuard $name did not reach $want within ${timeout}s"
}

# wait_for GUARD TIMEOUT_SECONDS JQ_FILTER: waits until the filter, run on
# the guard, prints true.
wait_for() {
  local name=$1 timeout=$2 filter=$3 i
  for ((i = 0; i < timeout; i++)); do
    [[ "$(guard "$name" | jq -r "$filter")" == "true" ]] && return 0
    sleep 1
  done
  guard "$name" | jq '.status' >&2
  die "FlagGuard $name: '$filter' did not become true within ${timeout}s"
}

# Flagsmith's admin API with the organisation key (what Flagbreaker uses).
fs() { curl -fsS -H "Authorization: Api-Key $FLAGSMITH_ORG_API_KEY" "$API_URL$1"; }

# As a person: the admin user's token, so Flagsmith records who did it.
as_person() {
  local method=$1 path=$2 body=$3
  curl -fsS -X "$method" -H "Authorization: Token $FLAGSMITH_ADMIN_TOKEN" -H "Content-Type: application/json" \
    --data "$body" "$API_URL$path"
}

flag_path() { printf '/api/__future__/environments/%s/features/%s/' "$FLAGSMITH_ENVIRONMENT_KEY" "$1"; }

# flag FEATURE_ID -> {"default": bool, "segments": {"<id>": bool}}
flag() {
  fs "$(flag_path "$1")" |
    jq -c '{default: .environment_default.enabled, segments: (.segment_overrides | map({key: (.segment.id | tostring), value: .enabled}) | from_entries)}'
}

# switch FEATURE_ID on|off, as a person
switch() {
  local on=false
  [[ "$2" == on ]] && on=true
  as_person PATCH "$(flag_path "$1")" "{\"environment_default\": {\"enabled\": $on}}" >/dev/null
}

# set_value FEATURE_ID STRING, as a person
set_value() {
  as_person PATCH "$(flag_path "$1")" \
    "$(jq -n --arg v "$2" '{environment_default: {value: {type: "string", value: $v}}}')" >/dev/null
}

# latest_health FEATURE_ID [PROVIDER] -> UNHEALTHY, HEALTHY or empty: the
# latest event in that provider's slot for the flag in Production.
latest_health() {
  local provider=${2:-Webhook}
  fs "/api/v1/projects/$FLAGSMITH_PROJECT_ID/feature-health/events/" |
    jq -r --argjson f "$1" --arg p "$provider" \
      '(.results? // .) | map(select(.feature == $f and .provider_name == $p)) | sort_by(.created_at) | last | .type // ""'
}

# wait_for_health FEATURE_ID TYPE TIMEOUT_SECONDS [PROVIDER]
wait_for_health() {
  local id=$1 want=$2 timeout=$3 provider=${4:-Webhook} i got
  for ((i = 0; i < timeout; i++)); do
    got="$(latest_health "$id" "$provider")"
    [[ "$got" == "$want" ]] && return 0
    sleep 1
  done
  die "Feature Health ($provider) should show $want, shows '$got'"
}

# epoch_of 2026-11-26T12:25:49Z -> seconds since 1970, with GNU date (Linux,
# Git Bash) or BSD date (macOS). jq's date functions don't work on Windows.
epoch_of() {
  local plain
  plain="$(sed 's/T/ /; s/Z$//' <<<"$1")"
  date -u -d "$1" +%s 2>/dev/null ||
    date -u -j -f "%Y-%m-%dT%H:%M:%SZ" "$1" +%s 2>/dev/null ||
    date -u -d "$plain" +%s 2>/dev/null || # busybox
    echo 0                                  # unknown date(1): don't wait
}

# wait_for_settled_rollout: Flagbreaker doesn't blame a flag switched on
# within 30s of a rollout (the deploy may be the cause), so a test that flips
# a flag right after `up.sh` deployed the app waits until that has passed.
wait_for_settled_rollout() {
  local newest age i
  for ((i = 0; i < 60; i++)); do
    # RFC 3339 timestamps sort as strings, so the max is the newest.
    newest="$("$KUBECTL" get rs -n shop -l app=checkout -o json | jq -r '[.items[].metadata.creationTimestamp] | max // empty')"
    [[ -n "$newest" ]] || return 0
    age=$(($(date -u +%s) - $(epoch_of "$newest")))
    ((age >= 35)) && return 0
    ((i == 0)) && log "waiting $((35 - age))s for the last rollout to settle (Flagbreaker won't blame a flag for a fresh deploy)"
    sleep 1
  done
}

print_timeline() {
  guard "$1" | jq -r '.status.timeline[]? | "  \(.time)  \(.message)"'
}
