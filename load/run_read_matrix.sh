#!/usr/bin/env bash
# Full read benchmark: single-node baseline, then master + 2 replicas.
# The application and k6 run natively; only PostgreSQL runs in Docker, so the
# app does not compete with three database processes for the Docker VM CPU.
set -euo pipefail

source "$(dirname "$0")/lib.sh"

SEED_CSV="${SEED_CSV:-$(cd "$REPO/.." && pwd)/people.v2.csv}"
SEED_N="${SEED_N:-1000000}"
export SEED_CSV SEED_N

if [ ! -f "$SEED_CSV" ]; then
  die "seed CSV not found: $SEED_CSV (set SEED_CSV)"
fi

# Both topologies publish port 5432, so make sure nothing from a previous run
# is still holding it.
stop_app
compose_repl down -v --remove-orphans >/dev/null 2>&1 || true
compose_base down -v --remove-orphans >/dev/null 2>&1 || true

run_phase() {
  local phase="$1"

  if [ "$phase" = "baseline" ]; then
    log "=== baseline: single-node PostgreSQL ==="
    compose_base down -v --remove-orphans >/dev/null 2>&1 || true
    compose_base up -d db
    compose_base --profile seed run --rm seed
    start_app "postgres://social:social@127.0.0.1:5432/social?sslmode=disable" ""
  else
    log "=== replicas: master + 2 streaming replicas ==="
    compose_repl down -v --remove-orphans >/dev/null 2>&1 || true
    compose_repl up -d pg-master pg-slave-1 pg-slave-2
    wait_replicas 180
    compose_repl --profile seed run --rm seed
    wait_replicas 300
    start_app \
      "postgres://social:social@127.0.0.1:5432/social?sslmode=disable" \
      "postgres://social:social@127.0.0.1:5433/social?sslmode=disable,postgres://social:social@127.0.0.1:5434/social?sslmode=disable"
  fi

  OUT="$RESULTS/$phase" "$HERE/run_read_bench.sh" "$phase"

  stop_app
  if [ "$phase" = "baseline" ]; then
    compose_base down -v --remove-orphans
  else
    compose_repl down -v --remove-orphans
  fi
}

run_phase baseline
run_phase replicas

log "read matrix complete; results in $RESULTS"
