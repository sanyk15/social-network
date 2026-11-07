#!/usr/bin/env bash
# Shared helpers for the replication load and failover scripts.
# Source this file from the other scripts; do not execute it directly.

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/.." && pwd)"
COMPOSE_REPL="$REPO/docker-compose.yml"
COMPOSE_BASE="$REPO/docker-compose.baseline.yml"
RESULTS="$HERE/results"
APP_BIN="$REPO/bin/server"
APP_LOG="$RESULTS/app.log"
APP_PID="$RESULTS/app.pid"

mkdir -p "$RESULTS"

log() { printf '%s\n' "$*" >&2; }
die() { log "$*"; exit 1; }

compose_repl() { docker compose -f "$COMPOSE_REPL" "$@"; }
compose_base() { docker compose -f "$COMPOSE_BASE" "$@"; }

psql_master() {
  compose_repl exec -T pg-master psql -U social -d social "$@"
}

psql_node() {
  local node="$1"
  shift
  compose_repl exec -T "$node" psql -U social -d social "$@"
}

wait_for_http() {
  local url="$1" timeout="${2:-30}"
  local deadline=$((SECONDS + timeout))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if curl -fsS -o /dev/null "$url" 2>/dev/null; then
      return 0
    fi
    sleep 1
  done
  return 1
}

start_app() {
  local master_url="$1" replica_urls="${2:-}"

  stop_app || true
  mkdir -p "$(dirname "$APP_BIN")"
  ( cd "$REPO" && go build -o "$APP_BIN" ./cmd/server )

  : > "$APP_LOG"
  (
    cd "$REPO"
    DATABASE_URL="$master_url" \
    DATABASE_REPLICA_URLS="$replica_urls" \
    DB_MAX_CONNS="${DB_MAX_CONNS:-16}" \
    ENABLE_TEST_API=true \
    LOG_REQUESTS=false \
    HTTP_ADDR=":8080" \
    nohup "$APP_BIN" >>"$APP_LOG" 2>&1 &
    echo $! > "$APP_PID"
  )

  if ! wait_for_http "http://127.0.0.1:8080/readyz" 30; then
    log "application did not become ready; last log lines:"
    tail -n 20 "$APP_LOG" >&2 || true
    return 1
  fi
}

stop_app() {
  if [ ! -f "$APP_PID" ]; then
    return 0
  fi

  local pid
  pid="$(cat "$APP_PID")"
  if kill -0 "$pid" 2>/dev/null; then
    kill "$pid" 2>/dev/null || true
    for _ in $(seq 1 20); do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.2
    done
    kill -9 "$pid" 2>/dev/null || true
  fi
  rm -f "$APP_PID"
}

# Wait until both replicas have replayed the master's current WAL position.
wait_replicas() {
  local timeout="${1:-120}"
  local deadline=$((SECONDS + timeout))
  local lsn node caught ok

  while [ "$SECONDS" -lt "$deadline" ]; do
    lsn="$(psql_master -tAc 'SELECT pg_current_wal_lsn()' 2>/dev/null | tr -d '[:space:]')" || lsn=""
    if [ -z "$lsn" ]; then
      sleep 1
      continue
    fi

    ok=1
    for node in pg-slave-1 pg-slave-2; do
      caught="$(psql_node "$node" -tAc \
        "SELECT (pg_last_wal_replay_lsn() >= '$lsn'::pg_lsn)" 2>/dev/null | tr -d '[:space:]')" || caught=""
      if [ "$caught" != "t" ]; then
        ok=0
      fi
    done

    if [ "$ok" = "1" ]; then
      return 0
    fi
    sleep 1
  done

  log "replicas did not catch up within ${timeout}s"
  return 1
}
