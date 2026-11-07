#!/usr/bin/env bash
# Switch the master between asynchronous and quorum synchronous replication.
#
#   ./switch-replication.sh sync    # ANY 2 (pg-slave-1, pg-slave-2)
#   ./switch-replication.sh async
#
# The setting is applied with ALTER SYSTEM + pg_reload_conf, so it survives
# configuration reloads. It is written to the local postgresql.auto.conf and is
# not streamed to replicas: a promoted node has to set it again.
set -euo pipefail

source "$(dirname "$0")/lib.sh"

MODE="${1:-sync}"

case "$MODE" in
  sync)
    # Application names contain '-', so PostgreSQL requires them quoted.
    psql_master -v ON_ERROR_STOP=1 <<'SQL'
ALTER SYSTEM SET synchronous_standby_names = 'ANY 2 ("pg-slave-1", "pg-slave-2")';
SELECT pg_reload_conf();
SQL
    ;;
  async)
    psql_master -v ON_ERROR_STOP=1 <<'SQL'
ALTER SYSTEM SET synchronous_standby_names = '';
SELECT pg_reload_conf();
SQL
    ;;
  *)
    die "usage: $0 sync|async"
    ;;
esac

if [ "$MODE" = "sync" ]; then
  for _ in $(seq 1 60); do
    n="$(psql_master -tAc \
      "SELECT count(*) FROM pg_stat_replication WHERE sync_state IN ('sync', 'quorum')" | tr -d '[:space:]')"
    if [ "$n" = "2" ]; then
      log "quorum synchronous replication active: 2 standbys"
      exit 0
    fi
    sleep 1
  done
  die "timed out waiting for two synchronous standbys"
fi

log "asynchronous replication active"
