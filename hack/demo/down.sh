#!/usr/bin/env bash
# Deletes the demo cluster and the local state in .demo/.

source "$(dirname "${BASH_SOURCE[0]}")/../lib.sh"

if [[ -x "$KIND" ]] && "$KIND" get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"; then
  log "deleting kind cluster $CLUSTER_NAME"
  "$KIND" delete cluster --name "$CLUSTER_NAME" --kubeconfig "$KUBECONFIG"
fi
rm -rf "$DEMO_DIR"
log "demo removed"
