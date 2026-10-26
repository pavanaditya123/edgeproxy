#!/usr/bin/env bash
# Benchmarks every balancing strategy at the same request rate and prints a
# Markdown table. Needs the compose stack running. Each strategy is switched
# in with a hot reload (POST /-/reload), not a restart.
#
#   RATE=2000 DURATION=30s scripts/bench.sh
set -euo pipefail
# Stop Git Bash on Windows from rewriting container paths like /scripts/load.js.
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/.."

RATE=${RATE:-2000}
DURATION=${DURATION:-30s}
STRATEGIES=${STRATEGIES:-"round_robin weighted_round_robin least_conn p2c consistent_hash"}
CONFIG=deploy/compose/edgeproxy.json
mkdir -p loadtest/results

original=$(mktemp)
cp "$CONFIG" "$original"
trap 'cp "$original" "$CONFIG"; rm -f "$original"; for port in 9090 9091; do curl -fsS -X POST "localhost:$port/-/reload" >/dev/null || true; done' EXIT

# Warm up connection pools and k6's workers first, so whichever strategy
# runs first isn't penalised for a cold start.
docker compose run --rm -T -e RATE="$RATE" -e DURATION=5s -e LABEL=warmup -e MAX_P99_MS=5000 \
  k6 run --quiet /scripts/load.js > /dev/null 2>&1 || true

for strategy in $STRATEGIES; do
  sed -E "s/\"strategy\": \"[a-z_]+\"/\"strategy\": \"$strategy\"/" "$original" > "$CONFIG"
  for port in 9090 9091; do
    curl -fsS -X POST "localhost:$port/-/reload" >/dev/null
  done
  echo "== $strategy" >&2
  docker compose run --rm -T -e RATE="$RATE" -e DURATION="$DURATION" -e LABEL="$strategy" \
    -e MAX_P99_MS=5000 k6 run --quiet /scripts/load.js > "loadtest/results/$strategy.log" 2>&1 || true
done

echo
echo "| Strategy | Target req/s | Achieved req/s | Errors | p50 | p95 | p99 |"
echo "|---|---|---|---|---|---|---|"
for strategy in $STRATEGIES; do
  f="loadtest/results/$strategy.json"
  if [ ! -f "$f" ]; then
    echo "| $strategy | $RATE | (failed, see loadtest/results/$strategy.log) | | | | |"
    continue
  fi
  # Pull fields out of the k6 summary without needing jq.
  get() { grep -o "\"$1\": [0-9.e-]*" "$f" | head -1 | awk '{print $2}'; }
  printf "| %s | %s | %s | %.3f%% | %.2f ms | %.2f ms | %.2f ms |\n" \
    "$strategy" "$RATE" "$(get rps)" "$(awk "BEGIN{print $(get error_rate)*100}")" \
    "$(get p50_ms)" "$(get p95_ms)" "$(get p99_ms)"
done
