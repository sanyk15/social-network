#!/usr/bin/env bash
# Start the Go application natively (outside Docker) for measurements.
#
#   ./start-app.sh <master-dsn> [replica-dsn,replica-dsn]
set -euo pipefail

source "$(dirname "$0")/lib.sh"

start_app "${1:?usage: start-app.sh <master-dsn> [replica-dsn,...]}" "${2:-}"
log "application is ready on http://127.0.0.1:8080 (pid $(cat "$APP_PID"))"
