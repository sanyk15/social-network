#!/usr/bin/env bash
# Bring up the replicated stack and the native application, optionally seeding.
#
#   ./start-stack.sh                 # master + 2 replicas, seed 1M, app on :8080
#   SKIP_SEED=1 ./start-stack.sh     # reuse existing data (failover / write bench)
#
# Environment: SEED_CSV, SEED_N, DB_MAX_CONNS.
set -euo pipefail

source "$(dirname "$0")/lib.sh"

SEED_CSV="${SEED_CSV:-$(cd "$REPO/.." && pwd)/people.v2.csv}"
SEED_N="${SEED_N:-1000000}"
export SEED_CSV SEED_N

if [ "${SKIP_SEED:-0}" != "1" ] && [ ! -f "$SEED_CSV" ]; then
  die "seed CSV not found: $SEED_CSV (set SEED_CSV or SKIP_SEED=1)"
fi

stop_app
compose_repl up -d pg-master pg-slave-1 pg-slave-2
wait_replicas 180

if [ "${SKIP_SEED:-0}" != "1" ]; then
  log "seeding ${SEED_N} profiles"
  compose_repl --profile seed run --rm seed
  wait_replicas 300
fi

start_app \
  "postgres://social:social@127.0.0.1:5432/social?sslmode=disable" \
  "postgres://social:social@127.0.0.1:5433/social?sslmode=disable,postgres://social:social@127.0.0.1:5434/social?sslmode=disable"

log "stack is ready: app on http://127.0.0.1:8080"
