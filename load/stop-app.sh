#!/usr/bin/env bash
# Stop the natively running application started by start-app.sh.
set -euo pipefail

source "$(dirname "$0")/lib.sh"

stop_app
log "application stopped"
