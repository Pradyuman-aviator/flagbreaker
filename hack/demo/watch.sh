#!/usr/bin/env bash
# A live view of the demo in the terminal, to follow along or record next to
# the Flagsmith dashboard. It shows:
#   - what Flagsmith serves: the flags, the breaker's mode and policy;
#   - whether the shop page answers;
#   - the checkout pods;
#   - each FlagGuard's phase and its last step;
#   - what Feature Health says about each flag.
#
#   hack/demo/watch.sh      # refreshes every 2 seconds; Ctrl-C quits
#   hack/demo/watch.sh 1    # every second

source "$(dirname "${BASH_SOURCE[0]}")/../lib.sh"
source "$ROOT_DIR/hack/demo/common.sh"
interval=${1:-2}

if [[ -t 1 ]]; then
  BOLD=$'\033[1m' DIM=$'\033[2m' RED=$'\033[31m' GREEN=$'\033[32m' BLUE=$'\033[34m' OFF=$'\033[0m'
else
  BOLD="" DIM="" RED="" GREEN="" BLUE="" OFF=""
fi

# paint COLOUR WIDTH TEXT: TEXT padded to WIDTH, in COLOUR.
paint() { printf '%s%-*s%s' "$1" "$2" "$3" "$OFF"; }

onoff() {
  if [[ "$1" == true ]]; then paint "$GREEN" "${2:-4}" on; else paint "$DIM" "${2:-4}" off; fi
}

phase_colour() {
  case "$1" in
    Armed) printf '%s' "$GREEN" ;;
    Tripped) printf '%s' "$RED" ;;
    Recovered) printf '%s' "$BLUE" ;;
    *) printf '%s' "$DIM" ;;
  esac
}

health_colour() {
  case "$1" in
    UNHEALTHY) printf '%s' "$RED" ;;
    HEALTHY) printf '%s' "$GREEN" ;;
    *) printf '%s' "$DIM" ;;
  esac
}

value_of() { jq -r '.environment_default.value.value // ""' <<<"$1"; }
enabled_of() { jq -r '.environment_default.enabled // false' <<<"$1"; }

# flag_health EVENTS FEATURE_ID -> "<Flagbreaker's latest> <alerts firing>"
flag_health() {
  jq -r --argjson f "$2" '
    ([.[] | select(.feature == $f and .provider_name == "Webhook")] | sort_by(.created_at) | last | .type // "none") + " " +
    ([.[] | select(.feature == $f and .provider_name != "Webhook" and .type == "UNHEALTHY")] | length | tostring)' <<<"$1"
}

body() {
  local cols width np ec mode policy events code pods guards
  cols="$(tput cols 2>/dev/null || echo 120)"
  width=$((cols - 8))
  ((width < 40)) && width=40

  np="$(fs "$(flag_path "$FLAGSMITH_NEW_PRICING_ID")" 2>/dev/null || echo '{}')"
  ec="$(fs "$(flag_path "$FLAGSMITH_EXPRESS_CHECKOUT_ID")" 2>/dev/null || echo '{}')"
  mode="$(value_of "$(fs "$(flag_path "$FLAGSMITH_MODE_ID")" 2>/dev/null || echo '{}')")"
  policy="$(value_of "$(fs "$(flag_path "$FLAGSMITH_POLICY_ID")" 2>/dev/null || echo '{}')")"
  events="$(fs "/api/v1/projects/$FLAGSMITH_PROJECT_ID/feature-health/events/" 2>/dev/null || echo '[]')"
  code="$(curl -s -o /dev/null -w '%{http_code}' -m 1 "$APP_URL" 2>/dev/null || true)"
  pods="$("$KUBECTL" get pods -n shop -l app=checkout -o json 2>/dev/null || echo '{"items":[]}')"
  guards="$("$KUBECTL" get flagguards -n shop -o json 2>/dev/null || echo '{"items":[]}')"

  printf ' %sFlagbreaker demo%s   %s UTC   %s(refreshes every %ss; Ctrl-C quits)%s\n\n' \
    "$BOLD" "$OFF" "$(date -u +%H:%M:%S)" "$DIM" "$interval" "$OFF"

  printf ' %sFLAGSMITH%s  Production   flagbreaker_mode %s%s%s   flagbreaker_policy %s%s%s\n' \
    "$BOLD" "$OFF" "$BOLD" "${mode:-?}" "$OFF" "$BOLD" "${policy:-?}" "$OFF"
  local seg h alerts
  seg="$(jq -r --argjson s "$FLAGSMITH_BETA_SEGMENT_ID" '[.segment_overrides[]? | select(.segment.id == $s) | .enabled][0] // "none"' <<<"$np")"
  read -r h alerts <<<"$(flag_health "$events" "$FLAGSMITH_NEW_PRICING_ID")"
  printf '   %-18s %s  beta_testers %s   Feature Health %s' new_pricing "$(onoff "$(enabled_of "$np")")" "$(onoff "$seg")" "$(paint "$(health_colour "$h")" 9 "$h")"
  printf '\n'
  read -r h alerts <<<"$(flag_health "$events" "$FLAGSMITH_EXPRESS_CHECKOUT_ID")"
  printf '   %-18s %s                      Feature Health %s' express_checkout "$(onoff "$(enabled_of "$ec")")" "$(paint "$(health_colour "$h")" 9 "$h")"
  ((alerts > 0)) && printf '  %s%s alert firing%s' "$RED" "$alerts" "$OFF"
  printf '\n\n'

  if [[ "$code" == 200 ]]; then
    printf ' %sSHOP%s       %s   %s● up%s\n\n' "$BOLD" "$OFF" "$APP_URL" "$GREEN" "$OFF"
  else
    printf ' %sSHOP%s       %s   %s● down%s %s(no ready checkout pod answers)%s\n\n' "$BOLD" "$OFF" "$APP_URL" "$RED" "$OFF" "$DIM" "$OFF"
  fi

  printf ' %sPODS%s       shop/checkout\n' "$BOLD" "$OFF"
  local name state ready restarts colour
  while IFS=$'\t' read -r name state ready restarts; do
    [[ -n "$name" ]] || continue
    colour="$GREEN"
    [[ "$ready" == True ]] || colour="$RED"
    [[ "$state" == Running || "$state" == Terminating ]] || colour="$RED"
    [[ "$state" == Terminating ]] && colour="$DIM"
    printf '   %-30s %s  %s  restarts %s\n' "$name" "$(paint "$colour" 18 "$state")" \
      "$(if [[ "$ready" == True ]]; then paint "$GREEN" 9 ready; else paint "$RED" 9 "not ready"; fi)" "$restarts"
  done < <(jq -r '.items[] | [
      .metadata.name,
      (if .metadata.deletionTimestamp then "Terminating"
       else (.status.containerStatuses[0].state // {} |
             if .waiting then .waiting.reason elif .terminated then .terminated.reason elif .running then "Running" else "Pending" end) end),
      ([.status.conditions[]? | select(.type == "Ready") | .status][0] // "False"),
      (.status.containerStatuses[0].restartCount // 0 | tostring)
    ] | @tsv' <<<"$pods")
  printf '\n'

  printf ' %sFLAGGUARDS%s\n' "$BOLD" "$OFF"
  local flag gmode phase last
  while IFS=$'\t' read -r name flag gmode phase last; do
    [[ -n "$name" ]] || continue
    printf '   %-28s %-18s %-6s %s\n' "$name" "$flag" "$gmode" "$(paint "$(phase_colour "$phase")" 10 "$phase")"
    [[ -n "$last" ]] && printf '     %s%s%s\n' "$DIM" "${last:0:$width}" "$OFF"
  done < <(jq -r '.items[] | [
      .metadata.name, .spec.flag, (.status.mode // "-"), (.status.phase // "unchecked"),
      ([(.status.timeline // [])[] | select(.event != "RestoreAvailable" and .event != "HealthyNotReported")] | last
        | if . then (.time[11:19] + "  " + .message) else "" end)
    ] | @tsv' <<<"$guards")
}

trap 'printf "%s\n" "$OFF"; exit 0' INT TERM
while true; do
  frame="$(body)"
  printf '\033[H\033[2J%s\n' "$frame"
  sleep "$interval"
done
