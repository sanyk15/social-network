#!/usr/bin/env bash
# Print the master's view of its standbys and each replica's receiver state.
set -euo pipefail

source "$(dirname "$0")/lib.sh"

echo "=== master: pg_stat_replication ==="
psql_master -c "
    SELECT application_name, state, sync_state,
           sent_lsn, write_lsn, flush_lsn, replay_lsn,
           pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn) AS lag_bytes
    FROM pg_stat_replication
    ORDER BY application_name;"

for node in pg-slave-1 pg-slave-2; do
  echo "=== ${node}: recovery and WAL receiver ==="
  psql_node "$node" -c "
      SELECT pg_is_in_recovery() AS in_recovery,
             pg_last_wal_replay_lsn() AS replay_lsn,
             pg_last_xact_replay_timestamp() AS last_replay;"
  psql_node "$node" -c "
      SELECT status, sender_host, sender_port, written_lsn, flushed_lsn
      FROM pg_stat_wal_receiver;"
done
