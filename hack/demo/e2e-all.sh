#!/usr/bin/env bash
# Runs every end-to-end check, one after the other: act mode with a complete
# switch-off and a restore, watch mode set from Flagsmith, following a
# Grafana alert on an annotated workload, putting back a bad remote config
# value, and failing requests while every pod stays up.

source "$(dirname "${BASH_SOURCE[0]}")/../lib.sh"

for t in e2e e2e-watch e2e-alert e2e-value e2e-errors; do
  log "=== $t ==="
  bash "$ROOT_DIR/hack/demo/$t.sh"
done
log "all end-to-end checks passed"
