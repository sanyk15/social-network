#!/usr/bin/env bash
# Two failover runs on freshly created stacks: asynchronous and synchronous.
#
#   ./run_failover_matrix.sh
#
# Each run: fresh volumes -> master + 2 replicas -> native app -> write load ->
# kill -9 the master -> promote -> repoint -> restart app -> count rows.
# The stack is recreated between runs because a failover changes the topology.
set -euo pipefail

source "$(dirname "$0")/lib.sh"

MODES="${MODES:-async sync}"

for mode in ${MODES}; do
  log "=== failover matrix: ${mode} ==="

  stop_app
  compose_repl down -v --remove-orphans >/dev/null 2>&1 || true
  compose_repl up -d pg-master pg-slave-1 pg-slave-2

  if ! wait_replicas 180; then
    compose_repl ps -a >&2 || true
    compose_repl logs --tail 50 pg-master pg-slave-1 pg-slave-2 >&2 || true
    die "replicas did not start for ${mode}"
  fi

  # Migrations create write_load; no seed data is needed for the write path.
  start_app \
    "postgres://social:social@127.0.0.1:5432/social?sslmode=disable" \
    "postgres://social:social@127.0.0.1:5433/social?sslmode=disable,postgres://social:social@127.0.0.1:5434/social?sslmode=disable"

  OUT="$RESULTS/failover-$mode" "$HERE/failover-test.sh" "$mode"

  stop_app
  compose_repl down -v --remove-orphans >/dev/null 2>&1 || true
done

log "failover matrix complete; results in $RESULTS"
