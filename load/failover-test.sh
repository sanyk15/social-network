#!/usr/bin/env bash
# Master failover test under write load.
#
#   ./failover-test.sh async
#   ./failover-test.sh sync
#
# Requires the replicated stack (pg-master, pg-slave-1, pg-slave-2) and the
# natively running application. Steps:
#   1. switch replication mode, truncate write_load, wait for replicas;
#   2. start k6 write load;
#   3. SIGKILL the master;
#   4. finish the load;
#   5. promote the freshest replica;
#   6. repoint the other replica to the new master;
#   7. restart the application on the new master;
#   8. compare acknowledged writes with rows on the new master.
set -euo pipefail

source "$(dirname "$0")/lib.sh"

MODE="${1:?usage: failover-test.sh async|sync}"
RATE="${RATE:-200}"
DURATION="${DURATION:-20s}"
KILL_AFTER="${KILL_AFTER:-12}"
OUT="${OUT:-$RESULTS/failover-$MODE}"

mkdir -p "$OUT"

command -v k6 >/dev/null || die "k6 is not installed"
command -v python3 >/dev/null || die "python3 is not installed"

curl -fsS -o /dev/null "http://127.0.0.1:8080/readyz" || die "application is not running; use start-app.sh first"

log "=== failover mode=${MODE} rate=${RATE}/s duration=${DURATION} kill_after=${KILL_AFTER}s ==="

"$HERE/switch-replication.sh" "$MODE"

psql_master -c "TRUNCATE write_load RESTART IDENTITY" >/dev/null
wait_replicas 60

curl -fsS "http://127.0.0.1:8080/test/replication" > "$OUT/replication_before.json" 2>/dev/null || true
psql_master -tAc "SELECT count(*) FROM write_load" > "$OUT/rows_before.txt"

# 2. write load
log "starting write load"
k6 run --quiet \
  -e RATE="${RATE}" -e DURATION="${DURATION}" -e OUT="${OUT}/writes.json" \
  "${HERE}/k6/writes.js" &
K6_PID=$!

sleep "$KILL_AFTER"

# Informational snapshot just before the master dies.
psql_master -tAc "SELECT count(*) FROM write_load" > "$OUT/rows_at_kill.txt" 2>/dev/null || true
psql_master -tAc "SELECT pg_current_wal_lsn()" > "$OUT/lsn_at_kill.txt" 2>/dev/null || true

# 3. kill the master
log "killing pg-master with SIGKILL"
compose_repl kill -s SIGKILL pg-master

# 4. let the load finish; requests after the kill should fail
set +e
wait "$K6_PID"
K6_EXIT=$?
set -e
log "write load finished (k6 exit ${K6_EXIT})"

# 5. pick the freshest replica
freshest=""
best_lsn=""
for node in pg-slave-1 pg-slave-2; do
  lsn="$(compose_repl exec -T "$node" psql -U social -d social -tAc \
    'SELECT pg_last_wal_replay_lsn()' 2>/dev/null | tr -d '[:space:]')" || lsn=""
  if [ -z "$lsn" ]; then
    log "replica ${node} is not reachable"
    continue
  fi
  log "replica ${node} replay_lsn=${lsn}"
  if [ -z "$best_lsn" ]; then
    freshest="$node"
    best_lsn="$lsn"
  else
    newer="$(compose_repl exec -T "$node" psql -U social -d social -tAc \
      "SELECT ('$lsn'::pg_lsn > '$best_lsn'::pg_lsn)" | tr -d '[:space:]')"
    if [ "$newer" = "t" ]; then
      freshest="$node"
      best_lsn="$lsn"
    fi
  fi
done
[ -n "$freshest" ] || die "no replica is reachable"

if [ "$freshest" = "pg-slave-1" ]; then
  follower="pg-slave-2"
  new_master_port=5433
  new_follower_port=5434
else
  follower="pg-slave-1"
  new_master_port=5434
  new_follower_port=5433
fi

printf '%s\n' "$freshest" > "$OUT/promoted.txt"
printf '%s\n' "$best_lsn" > "$OUT/promoted_lsn.txt"

# 5a. promote
log "promoting ${freshest} (${best_lsn})"
compose_repl exec -T -u postgres "$freshest" bash -c 'pg_ctl promote -D "$PGDATA"'

recovery=""
for _ in $(seq 1 60); do
  recovery="$(compose_repl exec -T "$freshest" psql -U social -d social -tAc \
    'SELECT pg_is_in_recovery()' 2>/dev/null | tr -d '[:space:]')" || recovery=""
  [ "$recovery" = "f" ] && break
  sleep 1
done
[ "$recovery" = "f" ] || die "promotion of ${freshest} did not finish"
compose_repl exec -T -u postgres "$freshest" bash -c 'rm -f "$PGDATA/standby.signal"'

# 6. repoint the second replica to the new master
log "repointing ${follower} to ${freshest}"
compose_repl exec -T -u postgres "$follower" bash -c "
    sed -i 's|host=[^ ]*|host=${freshest}|' \"\$PGDATA/postgresql.auto.conf\"
    pg_ctl reload -D \"\$PGDATA\"
"

receiver=""
for _ in $(seq 1 90); do
  receiver="$(compose_repl exec -T "$follower" psql -U social -d social -tAc \
    'SELECT status FROM pg_stat_wal_receiver' 2>/dev/null | tr -d '[:space:]')" || receiver=""
  [ "$receiver" = "streaming" ] && break
  sleep 1
done
[ "$receiver" = "streaming" ] || die "${follower} did not start streaming from ${freshest}"

# ALTER SYSTEM writes postgresql.auto.conf locally and is not replicated, so the
# promoted node has no synchronous_standby_names of its own. In sync mode we
# re-establish quorum for the surviving topology: the old master required ANY 2,
# only one standby is left.
if [ "$MODE" = "sync" ]; then
  log "new master quorum: ANY 1 (${follower})"
  compose_repl exec -T "$freshest" psql -U social -d social -v ON_ERROR_STOP=1 <<SQL
ALTER SYSTEM SET synchronous_standby_names = 'ANY 1 ("${follower}")';
SELECT pg_reload_conf();
SQL
  for _ in $(seq 1 60); do
    n="$(compose_repl exec -T "$freshest" psql -U social -d social -tAc \
      "SELECT count(*) FROM pg_stat_replication WHERE sync_state IN ('sync', 'quorum')" | tr -d '[:space:]')"
    [ "$n" = "1" ] && break
    sleep 1
  done
fi

compose_repl exec -T "$freshest" psql -U social -d social -c "
    SELECT application_name, state, sync_state
    FROM pg_stat_replication
    ORDER BY application_name;" > "$OUT/replication_after_promotion.txt"

# 7. restart the application on the new master
log "restarting application on ${freshest}"
start_app \
  "postgres://social:social@127.0.0.1:${new_master_port}/social?sslmode=disable" \
  "postgres://social:social@127.0.0.1:${new_follower_port}/social?sslmode=disable"

# 8. compare acknowledged writes with rows on the promoted node
curl -fsS "http://127.0.0.1:8080/test/write/count" > "$OUT/count_after.json"
curl -fsS "http://127.0.0.1:8080/test/replication" > "$OUT/replication_after.json" 2>/dev/null || true

python3 - "$OUT" "$MODE" "$freshest" "$follower" <<'PY'
import json
import os
import sys

out, mode, promoted, follower = sys.argv[1:5]

with open(os.path.join(out, "writes.json"), encoding="utf-8") as f:
    metrics = json.load(f)["metrics"]

acked = int(metrics.get("writes_ok", {}).get("values", {}).get("count", 0))
failed = int(metrics.get("writes_failed", {}).get("values", {}).get("count", 0))

with open(os.path.join(out, "count_after.json"), encoding="utf-8") as f:
    rows = int(json.load(f)["count"])

lost = max(0, acked - rows)
unconfirmed = max(0, rows - acked)

summary = {
    "mode": mode,
    "promoted": promoted,
    "follower": follower,
    "acked_writes": acked,
    "failed_writes": failed,
    "rows_after_failover": rows,
    "lost_acked_writes": lost,
    "unconfirmed_rows": unconfirmed,
}

with open(os.path.join(out, "summary.json"), "w", encoding="utf-8") as f:
    json.dump(summary, f, indent=2)
    f.write("\n")

with open(os.path.join(out, "summary.md"), "w", encoding="utf-8") as f:
    f.write("| mode | promoted | acked | failed | rows | lost | unconfirmed |\n")
    f.write("|---|---|---:|---:|---:|---:|---:|\n")
    f.write(f"| {mode} | {promoted} | {acked} | {failed} | {rows} | {lost} | {unconfirmed} |\n")

print(json.dumps(summary, indent=2))
PY

log "done: ${OUT}"
