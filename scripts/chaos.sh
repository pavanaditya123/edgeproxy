#!/usr/bin/env bash
# Chaos test: steady load through both proxies while things break, failing if
# more than MAX_ERROR_RATE of requests fail. Needs the compose stack running
# (docker compose up -d --build).
#
#   t=10s  kill backend-a outright (crash)
#   t=20s  add 500ms latency to backend-c (via Toxiproxy)
#   t=30s  stop Redis (rate limiter must fall back)
#   t=40s  bring everything back
#   t=60s  load ends; k6 checks the error rate
set -euo pipefail
# Stop Git Bash on Windows from rewriting container paths like /scripts/load.js.
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/.."

RATE=${RATE:-500}
DURATION=${DURATION:-60s}
MAX_ERROR_RATE=${MAX_ERROR_RATE:-0.001}
TOXIPROXY=${TOXIPROXY:-http://localhost:8474}
mkdir -p loadtest/results
# k6 runs as a non-root user inside its container and writes its summary here.
chmod a+w loadtest/results

log() { echo "[chaos $(date +%T)] $*"; }

restore() {
  docker compose start backend-a redis >/dev/null 2>&1 || true
  curl -fsS -X DELETE "$TOXIPROXY/proxies/backend-c/toxics/slow" >/dev/null 2>&1 || true
}
trap restore EXIT

log "starting ${RATE} req/s for ${DURATION}"
docker compose run --rm -T -e RATE="$RATE" -e DURATION="$DURATION" -e LABEL=chaos \
  -e MAX_ERROR_RATE="$MAX_ERROR_RATE" -e MAX_P99_MS=2000 \
  k6 run --quiet /scripts/load.js > loadtest/results/chaos.log 2>&1 &
k6_pid=$!

sleep 10
log "killing backend-a"
docker compose kill backend-a >/dev/null

sleep 10
log "adding 500ms latency to backend-c"
curl -fsS -X POST "$TOXIPROXY/proxies/backend-c/toxics" -H 'Content-Type: application/json' \
  -d '{"name":"slow","type":"latency","stream":"downstream","attributes":{"latency":500}}' >/dev/null

sleep 10
log "stopping redis"
docker compose stop redis >/dev/null

sleep 10
log "restoring backend-a, backend-c and redis"
restore

status=0
wait "$k6_pid" || status=$?
cat loadtest/results/chaos.json 2>/dev/null || true
if [ "$status" -ne 0 ]; then
  log "FAILED: thresholds crossed (see loadtest/results/chaos.log)"
  tail -n 30 loadtest/results/chaos.log
  exit "$status"
fi
log "passed: error rate stayed under ${MAX_ERROR_RATE}"
