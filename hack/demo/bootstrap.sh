#!/usr/bin/env bash
# Sets up a fresh self-hosted Flagsmith through its REST API, so `make demo`
# needs no clicks in the dashboard. Safe to re-run: every step finds what it
# created last time before creating anything.
#
# What it creates:
#   - the first admin user (signed up through the API) and an org API key
#   - org "Acme", project "Checkout", environment "Production"
#   - flags: new_pricing, express_checkout and checkout_config (the demo
#     flags; checkout_config is remote config) plus the breaker's own
#     settings, flagbreaker_enabled, flagbreaker_mode and flagbreaker_policy
#   - segment beta_testers, with new_pricing switched on for it
#   - Feature Health "Webhook" (Flagbreaker's reports) and "Grafana" (alerts)
#     providers for the Checkout project
#   - a signed environment webhook that tells Flagbreaker about flag changes
#   - org "Flagsmith on Flagsmith" with a `feature_health` flag. The dashboard
#     reads its own feature flags from here, so the Feature Health screens show.
#
# Self-hosted Flagsmith puts every organisation on the free plan, which allows
# one project and one seat, so each project gets its own organisation.
#
# Writes the IDs and keys to .demo/flagsmith.env (git-ignored).

source "$(dirname "${BASH_SOURCE[0]}")/../lib.sh"
require curl jq

ADMIN_EMAIL="admin@flagbreaker.local"
PASSWORD_FILE="$DEMO_DIR/admin-password"
ENV_FILE="$DEMO_DIR/flagsmith.env"

if [[ ! -s "$PASSWORD_FILE" ]]; then
  # Random letters and digits; no openssl needed.
  LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 24 >"$PASSWORD_FILE" || true
fi
ADMIN_PASSWORD="$(cat "$PASSWORD_FILE")"

AUTH_HEADER=""

# api METHOD PATH [JSON] -> prints the response body; fails on HTTP >= 300.
api() {
  local method=$1 path=$2 data=${3:-} out code
  local args=(-sS -X "$method" -H "Content-Type: application/json" -w $'\n%{http_code}')
  [[ -n "$AUTH_HEADER" ]] && args+=(-H "Authorization: $AUTH_HEADER")
  [[ -n "$data" ]] && args+=(--data "$data")
  out="$(curl "${args[@]}" "$API_URL$path")"
  code="${out##*$'\n'}"
  out="${out%$'\n'*}"
  if ((code >= 300)); then
    echo "Flagsmith API: $method $path returned $code: $out" >&2
    return 1
  fi
  printf '%s' "$out"
}

# Picks the id of the item named $2 from a list response, paginated or not.
id_by_name() { jq -r --arg n "$2" '(.results? // .) | map(select(.name == $n)) | .[0].id // empty' <<<"$1"; }

wait_for_api() {
  log "waiting for the Flagsmith API at $API_URL"
  for _ in $(seq 1 120); do
    curl -fsS "$API_URL/health/liveness/" >/dev/null 2>&1 && return 0
    sleep 5
  done
  die "Flagsmith API did not become healthy"
}

login() {
  local body
  body="$(jq -n --arg e "$ADMIN_EMAIL" --arg p "$ADMIN_PASSWORD" '{email: $e, password: $p}')"
  # A fresh install has no users, so the first sign-up may become superuser.
  local signup
  signup="$(jq -n --arg e "$ADMIN_EMAIL" --arg p "$ADMIN_PASSWORD" \
    '{email: $e, password: $p, first_name: "Demo", last_name: "Admin", superuser: true}')"
  local token
  if token="$(api POST /api/v1/auth/users/ "$signup" 2>/dev/null | jq -r '.key // empty')" && [[ -n "$token" ]]; then
    log "signed up $ADMIN_EMAIL"
  else
    token="$(api POST /api/v1/auth/login/ "$body" | jq -r '.key // empty')" ||
      die "could not log in as $ADMIN_EMAIL (delete .demo/ if you reinstalled Flagsmith)"
  fi
  [[ -n "$token" ]] || die "no token for $ADMIN_EMAIL"
  AUTH_HEADER="Token $token"
  ADMIN_TOKEN="$token"
}

# ensure_org NAME -> id
ensure_org() {
  local id
  id="$(id_by_name "$(api GET /api/v1/organisations/)" "$1")"
  [[ -n "$id" ]] || id="$(api POST /api/v1/organisations/ "$(jq -n --arg n "$1" '{name: $n}')" | jq -r .id)"
  printf '%s' "$id"
}

# ensure_project ORG_ID NAME -> id
ensure_project() {
  local id
  id="$(id_by_name "$(api GET "/api/v1/projects/?organisation=$1")" "$2")"
  [[ -n "$id" ]] || id="$(api POST /api/v1/projects/ \
    "$(jq -n --arg n "$2" --argjson o "$1" '{name: $n, organisation: $o}')" | jq -r .id)"
  printf '%s' "$id"
}

# ensure_environment PROJECT_ID NAME -> client-side environment key
ensure_environment() {
  local list key
  list="$(api GET "/api/v1/environments/?project=$1")"
  key="$(jq -r --arg n "$2" '(.results? // .) | map(select(.name == $n)) | .[0].api_key // empty' <<<"$list")"
  [[ -n "$key" ]] || key="$(api POST /api/v1/environments/ \
    "$(jq -n --arg n "$2" --argjson p "$1" '{name: $n, project: $p}')" | jq -r .api_key)"
  printf '%s' "$key"
}

# ensure_feature PROJECT_ID NAME ENABLED VALUE DESCRIPTION -> id
ensure_feature() {
  local id
  id="$(id_by_name "$(api GET "/api/v1/projects/$1/features/?search=$2")" "$2")"
  if [[ -z "$id" ]]; then
    id="$(api POST "/api/v1/projects/$1/features/" "$(jq -n \
      --arg n "$2" --argjson on "$3" --arg v "$4" --arg d "$5" \
      '{name: $n, default_enabled: $on, initial_value: (if $v == "" then null else $v end), description: $d}')" |
      jq -r .id)"
  fi
  printf '%s' "$id"
}

# ensure_health_provider PROJECT_ID NAME -> webhook path (/api/v1/feature-health/...)
# NAME is Webhook (Flagbreaker reports there) or Grafana (also takes
# Prometheus Alertmanager alerts).
ensure_health_provider() {
  local url
  url="$(api GET "/api/v1/projects/$1/feature-health/providers/" |
    jq -r --arg n "$2" '(.results? // .) | map(select(.name == $n)) | .[0].webhook_url // empty')"
  [[ -n "$url" ]] || url="$(api POST "/api/v1/projects/$1/feature-health/providers/" "$(jq -n --arg n "$2" '{name: $n}')" |
    jq -r .webhook_url)"
  # Flagsmith builds the URL from the request's host (localhost here). Keep
  # only the path, so in-cluster clients can prefix the Service address.
  sed -E 's#^https?://[^/]+##' <<<"$url"
}

# ensure_segment PROJECT_ID NAME TRAIT VALUE -> id. The segment matches
# identities whose trait TRAIT equals VALUE.
ensure_segment() {
  local id
  id="$(id_by_name "$(api GET "/api/v1/projects/$1/segments/")" "$2")"
  [[ -n "$id" ]] || id="$(api POST "/api/v1/projects/$1/segments/" "$(jq -n \
    --arg n "$2" --argjson p "$1" --arg t "$3" --arg v "$4" \
    '{name: $n, project: $p, rules: [{type: "ALL", rules: [{type: "ANY", rules: [],
      conditions: [{property: $t, operator: "EQUAL", value: $v}]}], conditions: []}]}')" | jq -r .id)"
  printf '%s' "$id"
}

# ensure_segment_override ENV_KEY FEATURE_ID SEGMENT_ID: the segment gets
# the flag switched on, unless it already has an override.
ensure_segment_override() {
  local path="/api/__future__/environments/$1/features/$2/"
  if ! api GET "$path" | jq -e --argjson s "$3" 'any(.segment_overrides[]?; .segment.id == $s)' >/dev/null; then
    api PATCH "$path" "$(jq -n --argjson s "$3" '{segment_overrides: [{segment: {id: $s}, enabled: true}]}')" >/dev/null
  fi
}

# ensure_org_api_key ORG_ID NAME -> key (only readable when created)
ensure_org_api_key() {
  local saved="$DEMO_DIR/org-api-key"
  # A key saved for an earlier Flagsmith (the cluster was recreated without
  # down.sh) no longer works: make a new one.
  if [[ -s "$saved" ]] && curl -fsS -o /dev/null -H "Authorization: Api-Key $(cat "$saved")" "$API_URL/api/v1/organisations/" 2>/dev/null; then
    cat "$saved"
    return
  fi
  api POST "/api/v1/organisations/$1/master-api-keys/" \
    "$(jq -n --arg n "$2" '{name: $n, is_admin: true}')" | jq -r .key | tee "$saved"
}

# need NAME VALUE: stops here if a step returned nothing. A failure inside
# $(...) doesn't stop the script by itself.
need() { [[ -n "$2" && "$2" != null ]] || die "bootstrap: no $1 from the Flagsmith API (see the error above)"; }

wait_for_api
login

log "creating org Acme, project Checkout, environment Production"
ORG_ID="$(ensure_org "Acme")" && need "organisation ID" "$ORG_ID"
PROJECT_ID="$(ensure_project "$ORG_ID" "Checkout")" && need "project ID" "$PROJECT_ID"
ENVIRONMENT_KEY="$(ensure_environment "$PROJECT_ID" "Production")" && need "environment key" "$ENVIRONMENT_KEY"

log "creating flags"
NEW_PRICING_ID="$(ensure_feature "$PROJECT_ID" new_pricing false "" \
  "New pricing engine for checkout. The demo app crashes when this is on.")"
EXPRESS_CHECKOUT_ID="$(ensure_feature "$PROJECT_ID" express_checkout false "" \
  "One-click checkout. The demo app runs fine with it; a (staged) Grafana alert says it raises the error rate.")"
CONFIG_ID="$(ensure_feature "$PROJECT_ID" checkout_config true '{"tax_rate": 0.08}' \
  "Checkout settings as JSON (remote config). The demo app reads tax_rate and crashes on a value it can't parse.")"
ensure_feature "$PROJECT_ID" flagbreaker_enabled true "" \
  "Master switch for Flagbreaker. Off means it watches nothing and changes nothing." >/dev/null
MODE_ID="$(ensure_feature "$PROJECT_ID" flagbreaker_mode true "act" \
  "watch: only report to Feature Health. act: also switch broken flags off.")"
POLICY_ID="$(ensure_feature "$PROJECT_ID" flagbreaker_policy true "{}" \
  'A mode per flag, as JSON: {"new_pricing": {"mode": "watch"}}. Modes: watch, act, off. The most cautious setting wins.')"
need "new_pricing ID" "$NEW_PRICING_ID"
need "express_checkout ID" "$EXPRESS_CHECKOUT_ID"
need "checkout_config ID" "$CONFIG_ID"
need "flagbreaker_mode ID" "$MODE_ID"
need "flagbreaker_policy ID" "$POLICY_ID"

# Beta testers already get new_pricing, through a segment override. A
# complete switch-off has to turn that off too, and a restore back on.
log "giving the beta_testers segment new_pricing"
BETA_SEGMENT_ID="$(ensure_segment "$PROJECT_ID" beta_testers plan beta)" && need "segment ID" "$BETA_SEGMENT_ID"
ensure_segment_override "$ENVIRONMENT_KEY" "$NEW_PRICING_ID" "$BETA_SEGMENT_ID"

log "adding Feature Health providers: Webhook (Flagbreaker) and Grafana (alerts)"
HEALTH_WEBHOOK_PATH="$(ensure_health_provider "$PROJECT_ID" Webhook)" && need "Webhook provider path" "$HEALTH_WEBHOOK_PATH"
GRAFANA_WEBHOOK_PATH="$(ensure_health_provider "$PROJECT_ID" Grafana)" && need "Grafana provider path" "$GRAFANA_WEBHOOK_PATH"
ORG_API_KEY="$(ensure_org_api_key "$ORG_ID" "flagbreaker")" && need "organisation API key" "$ORG_API_KEY"

# FLAG_UPDATED webhooks tell Flagbreaker exactly when a flag changed and who
# changed it. Flagsmith signs each one with this secret (HMAC-SHA256).
log "adding a signed environment webhook for Flagbreaker"
WEBHOOK_SECRET_FILE="$DEMO_DIR/webhook-secret"
[[ -s "$WEBHOOK_SECRET_FILE" ]] || LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 32 >"$WEBHOOK_SECRET_FILE" || true
WEBHOOK_SECRET="$(cat "$WEBHOOK_SECRET_FILE")"
WEBHOOK_URL="http://flagbreaker-webhook.flagbreaker-system.svc:8082/webhooks/flagsmith"
if ! api GET "/api/v1/environments/$ENVIRONMENT_KEY/webhooks/" | jq -e --arg u "$WEBHOOK_URL" '(.results? // .) | any(.url == $u)' >/dev/null; then
  api POST "/api/v1/environments/$ENVIRONMENT_KEY/webhooks/" \
    "$(jq -n --arg u "$WEBHOOK_URL" --arg s "$WEBHOOK_SECRET" '{url: $u, enabled: true, secret: $s}')" >/dev/null
fi

log "setting up Flagsmith on Flagsmith, so the dashboard shows Feature Health"
FOF_ORG_ID="$(ensure_org "Flagsmith on Flagsmith")"
FOF_PROJECT_ID="$(ensure_project "$FOF_ORG_ID" "Dashboard")"
FOF_ENVIRONMENT_KEY="$(ensure_environment "$FOF_PROJECT_ID" "Self-hosted")"
ensure_feature "$FOF_PROJECT_ID" feature_health true "" \
  "Shows the Feature Health settings, tags and warnings in this dashboard." >/dev/null
# On in Flagsmith's own defaults for self-hosted dashboards; keeps the
# feature versioning settings visible.
ensure_feature "$FOF_PROJECT_ID" feature_versioning true "" \
  "Shows the feature versioning settings in this dashboard." >/dev/null

cat >"$ENV_FILE" <<EOF
# Written by hack/demo/bootstrap.sh. Local only; do not commit.
FLAGSMITH_API_URL=$API_URL
FLAGSMITH_UI_URL=$UI_URL
FLAGSMITH_ADMIN_EMAIL=$ADMIN_EMAIL
FLAGSMITH_ADMIN_PASSWORD=$ADMIN_PASSWORD
FLAGSMITH_ADMIN_TOKEN=$ADMIN_TOKEN
FLAGSMITH_ORG_API_KEY=$ORG_API_KEY
FLAGSMITH_ORG_ID=$ORG_ID
FLAGSMITH_PROJECT_ID=$PROJECT_ID
FLAGSMITH_ENVIRONMENT_KEY=$ENVIRONMENT_KEY
FLAGSMITH_NEW_PRICING_ID=$NEW_PRICING_ID
FLAGSMITH_EXPRESS_CHECKOUT_ID=$EXPRESS_CHECKOUT_ID
FLAGSMITH_CONFIG_ID=$CONFIG_ID
FLAGSMITH_MODE_ID=$MODE_ID
FLAGSMITH_POLICY_ID=$POLICY_ID
FLAGSMITH_BETA_SEGMENT_ID=$BETA_SEGMENT_ID
FLAGSMITH_HEALTH_WEBHOOK_PATH=$HEALTH_WEBHOOK_PATH
FLAGSMITH_GRAFANA_WEBHOOK_PATH=$GRAFANA_WEBHOOK_PATH
FLAGSMITH_WEBHOOK_SECRET=$WEBHOOK_SECRET
FLAGSMITH_FOF_ENVIRONMENT_KEY=$FOF_ENVIRONMENT_KEY
EOF

# Values for the Flagbreaker chart. The breaker's own flags live in the same
# Production environment as the flag it guards, and annotated workloads use
# that environment unless they name another.
cat >"$DEMO_DIR/flagbreaker-values.yaml" <<EOF
flagsmith:
  url: http://flagsmith-api.flagsmith:8000
  settingsEnvironmentKey: "$ENVIRONMENT_KEY"
  defaultEnvironmentKey: "$ENVIRONMENT_KEY"
  apiKey: "$ORG_API_KEY"
  healthWebhookPath: "$HEALTH_WEBHOOK_PATH"
  webhookSecret: "$WEBHOOK_SECRET"
EOF

# Values for the second Helm pass: point the dashboard at its own flags.
cat >"$DEMO_DIR/flagsmith-on-flagsmith.yaml" <<EOF
frontend:
  extraEnv:
    FLAGSMITH_ON_FLAGSMITH_API_KEY: "$FOF_ENVIRONMENT_KEY"
    FLAGSMITH_ON_FLAGSMITH_API_URL: "$API_URL/api/v1/"
EOF

log "Flagsmith is set up (details in .demo/flagsmith.env)"
