#!/usr/bin/env bash
# Asks Flagbreaker to restore a flag it switched off, exactly as it was
# (environment default and segment overrides), once the incident is over.
#
#   hack/demo/restore.sh                            # checkout-new-pricing
#   hack/demo/restore.sh checkout-express-checkout

source "$(dirname "${BASH_SOURCE[0]}")/../lib.sh"

guard=${1:-checkout-new-pricing}
log "annotating FlagGuard $guard with flagbreaker.dev/restore"
"$KUBECTL" annotate flagguard "$guard" -n shop flagbreaker.dev/restore="$(date +%s)" --overwrite
