#!/usr/bin/env bash
# watch-jobs.sh — poll the durable Job queue and scheduler state; compact drain view.
# Usage: BROKER_HOST=http://10.0.0.243 ./watch-jobs.sh [interval-seconds, default 5]
# Read-only: GETs only. Ctrl-C to stop. jq recommended.
set -u
HOST="${BROKER_HOST:-http://10.0.0.243}"
CTRL="$HOST:11437"
N="${1:-5}"

while :; do
  ts=$(date '+%H:%M:%S')
  st=$(curl -s --max-time 5 "$CTRL/status" 2>/dev/null)
  if [ -z "$st" ]; then echo "$ts control plane unreachable"; sleep "$N"; continue; fi
  if command -v jq >/dev/null 2>&1; then
    line=$(echo "$st" | jq -r '"yield=\(.yield.yielding)(\(.yield.mode)) inflight=\(.queue.inflight)/\(.queue.max_inflight) waiters(i/b)=\(.queue.interactive)/\(.queue.batch) jobs Q=\(.jobs.Queued // 0) R=\(.jobs.Running // 0) ok=\(.jobs.Succeeded // 0) fail=\(.jobs.Failed // 0)"')
    echo "$ts $line"
    queued=$(curl -s --max-time 5 "$CTRL/jobs?state=QUEUED" 2>/dev/null)
    running=$(curl -s --max-time 5 "$CTRL/jobs?state=RUNNING" 2>/dev/null)
    for s in "$queued" "$running"; do
      [ -n "$s" ] && echo "$s" | jq -r '.jobs[]? | "  \(.state)\tpos=\(.position // "-")\tattempts=\(.attempts)\t\(.id)\tsrc=\(.source)"' 2>/dev/null
    done
  else
    echo "$ts $st"
  fi
  sleep "$N"
done
