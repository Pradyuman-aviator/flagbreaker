#!/usr/bin/env bash
# Brings up the whole demo: a kind cluster, self-hosted Flagsmith from the
# official Helm chart, a scripted first-time setup, Flagbreaker from
# charts/flagbreaker and the checkout demo app. Safe to re-run.
#
# Needs Docker (with about 3 GB of memory free), curl and jq. Set API_PORT
# and UI_PORT if 8000 or 8080 is taken on your machine.

source "$(dirname "${BASH_SOURCE[0]}")/../lib.sh"
require docker curl jq
DEMO_SRC="$ROOT_DIR/hack/demo"
started=$(date +%s)

docker info >/dev/null 2>&1 || die "Docker is not running"
bash "$ROOT_DIR/hack/tools.sh" >/dev/null

port_in_use() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }

if "$KIND" get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"; then
  log "kind cluster $CLUSTER_NAME already exists"
  "$KIND" export kubeconfig --name "$CLUSTER_NAME" --kubeconfig "$KUBECONFIG" >/dev/null
else
  for port in "$API_PORT" "$UI_PORT"; do
    if port_in_use "$port"; then
      die "port $port is already in use on this machine. Free it, or pick others: API_PORT=18000 UI_PORT=18080 bash hack/demo/up.sh"
    fi
  done
  node_image="$KIND_NODE_IMAGE"
  if [[ "$(docker info --format '{{.CgroupVersion}}' 2>/dev/null)" == "1" ]]; then
    node_image="$KIND_NODE_IMAGE_CGROUP_V1"
    log "Docker uses cgroup v1, so the cluster runs Kubernetes 1.34 (1.35+ needs cgroup v2)"
  fi
  log "creating kind cluster $CLUSTER_NAME"
  cat >"$DEMO_DIR/kind-config.yaml" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    image: $node_image
    extraPortMappings:
      - containerPort: 30080 # Flagsmith API (hack/demo/nodeports.yaml)
        hostPort: $API_PORT
        listenAddress: "127.0.0.1"
      - containerPort: 30081 # Flagsmith dashboard
        hostPort: $UI_PORT
        listenAddress: "127.0.0.1"
EOF
  "$KIND" create cluster --name "$CLUSTER_NAME" --kubeconfig "$KUBECONFIG" \
    --config "$(native_path "$DEMO_DIR/kind-config.yaml")" --wait 2m
fi

helm_flagsmith() {
  # The second values file exists only after bootstrap. (Not an array: an
  # empty array trips `set -u` in the bash 3.2 that macOS ships.)
  local fof=""
  [[ -f "$DEMO_DIR/flagsmith-on-flagsmith.yaml" ]] && fof="$(native_path "$DEMO_DIR/flagsmith-on-flagsmith.yaml")"
  "$HELM" upgrade --install "$FLAGSMITH_RELEASE" flagsmith \
    --repo "$FLAGSMITH_CHART_REPO" --version "$FLAGSMITH_CHART_VERSION" \
    --namespace "$FLAGSMITH_NAMESPACE" --create-namespace \
    -f "$(native_path "$DEMO_SRC/flagsmith-values.yaml")" ${fof:+-f "$fof"} \
    --set api.image.tag="$FLAGSMITH_VERSION" \
    --set frontend.image.tag="$FLAGSMITH_VERSION" \
    --set taskProcessor.image.tag="$FLAGSMITH_VERSION" \
    --set-string frontend.extraEnv.FLAGSMITH_API_URL="$API_URL/api/v1/" \
    --wait --timeout 15m
}

log "installing Flagsmith $FLAGSMITH_VERSION (chart $FLAGSMITH_CHART_VERSION); the first pull takes a few minutes"
helm_flagsmith
"$KUBECTL" apply -f "$(native_path "$DEMO_SRC/nodeports.yaml")"

bash "$DEMO_SRC/bootstrap.sh"

log "restarting the dashboard with Flagsmith on Flagsmith"
helm_flagsmith

# shellcheck disable=SC1091
source "$DEMO_DIR/flagsmith.env"

log "building the Flagbreaker and demo app images"
# build TAG [ARGS...]: retried, because the first `go mod download` on a new
# machine sometimes hits a network timeout.
build() {
  local tag=$1 try
  shift
  for try in 1 2 3; do
    docker build -q "$@" -t "$tag" "$ROOT_DIR" >/dev/null && return 0
    log "building $tag failed (attempt $try of 3); retrying"
    sleep 5
  done
  die "could not build $tag; run 'docker build -t $tag .' to see why"
}
build flagbreaker:dev
build checkout-demo:dev --build-arg CMD=./demo/checkout
"$KIND" load docker-image --name "$CLUSTER_NAME" flagbreaker:dev checkout-demo:dev

log "installing Flagbreaker from charts/flagbreaker"
# Helm installs CRDs from crds/ only once and never upgrades them.
"$KUBECTL" apply --server-side --force-conflicts -f "$(native_path "$ROOT_DIR/charts/flagbreaker/crds")" >/dev/null
"$HELM" upgrade --install flagbreaker "$(native_path "$ROOT_DIR/charts/flagbreaker")" \
  --namespace flagbreaker-system --create-namespace \
  -f "$(native_path "$DEMO_DIR/flagbreaker-values.yaml")" \
  --set image.repository=flagbreaker --set image.tag=dev --set image.pullPolicy=Never \
  --wait --timeout 5m
# A rebuilt image has the same tag, so restart to pick it up.
"$KUBECTL" rollout restart deployment/flagbreaker -n flagbreaker-system >/dev/null
"$KUBECTL" rollout status deployment/flagbreaker -n flagbreaker-system --timeout 2m

log "deploying the demo app and its FlagGuards"
sed "s/__ENVIRONMENT_KEY__/$FLAGSMITH_ENVIRONMENT_KEY/g" "$DEMO_SRC/checkout.yaml" >"$DEMO_DIR/checkout.yaml"
"$KUBECTL" apply -f "$(native_path "$DEMO_DIR/checkout.yaml")"
"$KUBECTL" rollout status deployment/checkout -n shop --timeout 3m
# Wait until both guards exist (one comes from the Deployment's annotation)
# and Flagbreaker has checked each once.
for _ in $(seq 1 90); do
  ready="$("$KUBECTL" get flagguards -n shop -o json 2>/dev/null |
    jq '[.items[] | select((.status.phase // "") != "")] | length' 2>/dev/null || echo 0)"
  [[ "$ready" -ge 2 ]] && break
  sleep 1
done
[[ "${ready:-0}" -ge 2 ]] || die "the FlagGuards in namespace shop were not checked within 90s; see: KUBECONFIG=.demo/kubeconfig bin/kubectl logs -n flagbreaker-system deployment/flagbreaker"
"$KUBECTL" get flagguards -n shop
cat <<EOF

  Ready in $(($(date +%s) - started))s.

  Flagsmith dashboard: $UI_URL
  Log in as:           $FLAGSMITH_ADMIN_EMAIL
  Password:            $FLAGSMITH_ADMIN_PASSWORD
  kubectl:             KUBECONFIG=.demo/kubeconfig bin/kubectl get flagguards -A

  Break it:            bash hack/demo/break.sh      (switches new_pricing on, as a person would)
  Fire an alert:       bash hack/demo/alert.sh fire (Grafana alert for express_checkout)
  Check everything:    bash hack/demo/e2e-all.sh

EOF
