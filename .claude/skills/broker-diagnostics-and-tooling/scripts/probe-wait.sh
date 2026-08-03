#!/usr/bin/env bash
# probe-wait.sh — measure admission latency per broker lane WITHOUT generating tokens.
# Sends a cheap GET /api/tags (proxied to Ollama) through the interactive and batch
# lanes and prints X-Broker-Wait-Ms, X-Broker-Status, and wall time.
# Usage: BROKER_HOST=http://desktop.example.internal ./probe-wait.sh [max-wait-seconds, default 12]
# Read-only. NOTE: even this metadata read must acquire the GPU slot (the Gate wraps
# every request on a lane port), so a long in-flight generation delays it — that IS
# the measurement.
set -u
HOST="${BROKER_HOST:-http://desktop.example.internal}"
T="${1:-12}"

for p in 11435 11436; do
  lane=interactive; [ "$p" = 11436 ] && lane=batch
  printf '%-12s :%s  ' "$lane" "$p"
  hdr=$(mktemp)
  start=$(date +%s)
  code=$(curl -s -D "$hdr" -o /dev/null -w '%{http_code}' --max-time "$T" "$HOST:$p/api/tags" 2>/dev/null)
  end=$(date +%s)
  wait_ms=$(grep -i '^x-broker-wait-ms:' "$hdr" | tr -d '\r' | awk '{print $2}')
  st=$(grep -i '^x-broker-status:' "$hdr" | tr -d '\r' | awk '{print $2}')
  retry=$(grep -i '^retry-after:' "$hdr" | tr -d '\r' | awk '{print $2}')
  rm -f "$hdr"
  case "$code" in
    200) printf 'served  wait_ms=%s wall=%ss\n' "${wait_ms:-?}" "$((end-start))" ;;
    503) printf '503 %s (Retry-After=%ss, wait_ms=%s) — yielding or wait budget exceeded\n' "${st:-deferred}" "${retry:-?}" "${wait_ms:-?}" ;;
    000) printf 'TIMEOUT after %ss — slot held longer than probe budget; check /status queue.busy\n' "$T" ;;
    *)   printf 'HTTP %s (X-Broker-Status=%s)\n' "$code" "${st:-?}" ;;
  esac
done
printf 'Interpretation: wait_ms is time queued for the GPU slot. Healthy idle: ~0.\n'
printf 'Large wait_ms on interactive while batch is mid-generation is bounded by BROKER_BATCH_QUANTUM for Jobs;\n'
printf 'a long SYNCHRONOUS batch call is NOT preemptible and holds the slot to completion.\n'
