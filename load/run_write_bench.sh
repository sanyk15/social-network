#!/usr/bin/env bash
# Clean write benchmark (no failover), async vs sync.
#
#   ./run_write_bench.sh async
#   ./run_write_bench.sh sync
#
# Requires the replicated stack and the natively running application.
set -euo pipefail

source "$(dirname "$0")/lib.sh"

MODE="${1:?usage: run_write_bench.sh async|sync}"
RATE="${RATE:-200}"
DURATION="${DURATION:-20s}"
OUT="${OUT:-$RESULTS/write-$MODE}"

mkdir -p "$OUT"

curl -fsS -o /dev/null "http://127.0.0.1:8080/readyz" || die "application is not running; use start-app.sh first"

"$HERE/switch-replication.sh" "$MODE"

psql_master -c "TRUNCATE write_load RESTART IDENTITY" >/dev/null
wait_replicas 60

curl -fsS "http://127.0.0.1:8080/test/replication" > "$OUT/replication_before.json" 2>/dev/null || true

log "write benchmark mode=${MODE} rate=${RATE}/s duration=${DURATION}"
k6 run --quiet \
  -e RATE="${RATE}" -e DURATION="${DURATION}" -e OUT="${OUT}/summary.json" \
  "${HERE}/k6/writes.js"

curl -fsS "http://127.0.0.1:8080/test/write/count" > "$OUT/count.json"
log "done: $OUT"
