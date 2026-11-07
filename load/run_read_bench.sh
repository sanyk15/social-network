#!/usr/bin/env bash
# Run the read matrix for one phase against an already running application.
#
#   ./run_read_bench.sh baseline
#   ./run_read_bench.sh replicas
#
# Environment: VUS_LIST, RUNS, DURATION, GET_SHARE, ID_MAX, BASE_URL,
#              USERNAME, PASSWORD, OUT.
set -euo pipefail

source "$(dirname "$0")/lib.sh"

PHASE="${1:?usage: run_read_bench.sh <phase>}"
VUS_LIST="${VUS_LIST:-1 10 100 1000}"
RUNS="${RUNS:-3}"
DURATION="${DURATION:-30s}"
BASE_URL="${BASE_URL:-http://127.0.0.1:8080}"
USERNAME="${USERNAME:-user0000001}"
PASSWORD="${PASSWORD:-password123}"
GET_SHARE="${GET_SHARE:-0.5}"
ID_MAX="${ID_MAX:-1000000}"
OUT="${OUT:-$RESULTS/$PHASE}"

mkdir -p "$OUT"

log "phase=${PHASE} out=${OUT} vus=[${VUS_LIST}] runs=${RUNS} duration=${DURATION} get_share=${GET_SHARE}"

if ! curl -fsS -o /dev/null -X POST "${BASE_URL}/login" \
    -H 'Content-Type: application/json' \
    -d "{\"username\":\"${USERNAME}\",\"password\":\"${PASSWORD}\"}"; then
  die "login failed; is the database seeded and the application running?"
fi

curl -fsS "${BASE_URL}/test/replication" > "${OUT}/replication_before.json" 2>/dev/null || true

if [ "${WARMUP:-1}" = "1" ]; then
  log "warmup: vus=10 duration=15s"
  k6 run --quiet --system-tags=proto,status,method,expected_response \
    -e VUS=10 -e DURATION=15s -e GET_SHARE="${GET_SHARE}" -e ID_MAX="${ID_MAX}" \
    -e BASE_URL="${BASE_URL}" -e USERNAME="${USERNAME}" -e PASSWORD="${PASSWORD}" \
    "${HERE}/k6/reads.js"
fi

for VUS in ${VUS_LIST}; do
  if [ "${VUS}" -ge 1000 ]; then
    LEVEL_RAMP="${RAMP_1000:-5}"
    LEVEL_COOLDOWN="${COOLDOWN_1000:-10}"
  else
    LEVEL_RAMP="${RAMP:-0}"
    LEVEL_COOLDOWN="${COOLDOWN:-5}"
  fi

  for RUN in $(seq 1 "${RUNS}"); do
    log "--- phase=${PHASE} vus=${VUS} run=${RUN}/${RUNS} ramp=${LEVEL_RAMP}s"
    k6 run --quiet --system-tags=proto,status,method,expected_response \
      -e VUS="${VUS}" -e DURATION="${DURATION}" -e RAMP="${LEVEL_RAMP}" \
      -e GET_SHARE="${GET_SHARE}" -e ID_MAX="${ID_MAX}" \
      -e BASE_URL="${BASE_URL}" -e USERNAME="${USERNAME}" -e PASSWORD="${PASSWORD}" \
      -e OUT="${OUT}/vu_${VUS}_run_${RUN}.json" \
      "${HERE}/k6/reads.js"
    sleep "${LEVEL_COOLDOWN}"
  done
done

curl -fsS "${BASE_URL}/test/replication" > "${OUT}/replication_after.json" 2>/dev/null || true
log "done: ${OUT}"
