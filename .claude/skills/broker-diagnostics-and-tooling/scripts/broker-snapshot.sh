#!/usr/bin/env bash
# broker-snapshot.sh — one-shot read-only health snapshot of the Ollama Resource Broker.
# Usage: BROKER_HOST=http://10.0.0.243 ./broker-snapshot.sh   (default host below)
# Read-only: GETs only. Never POSTs, never restarts anything.
set -u
HOST="${BROKER_HOST:-http://10.0.0.243}"
CTRL="$HOST:11437"
T=6

say() { printf '%s\n' "$*"; }
warn() { printf 'WARN: %s\n' "$*"; }

say "== broker snapshot: $CTRL $(date '+%Y-%m-%d %H:%M:%S') =="

hz=$(curl -s --max-time $T "$CTRL/healthz" 2>/dev/null)
if [ "$hz" != "ok" ]; then
  warn "healthz unreachable or not 'ok' (got: '${hz:-<no response>}') — is the broker up? (systemctl status ollama-broker)"
  exit 1
fi
say "healthz: ok"

status=$(curl -s --max-time $T "$CTRL/status" 2>/dev/null)
if command -v jq >/dev/null 2>&1; then
  say "$status" | jq .
  yielding=$(say "$status" | jq -r .yield.yielding)
  mode=$(say "$status" | jq -r .yield.mode)
  busy=$(say "$status" | jq -r .queue.busy)
  iq=$(say "$status" | jq -r .queue.interactive)
  bq=$(say "$status" | jq -r .queue.batch)
  [ "$yielding" = "true" ] && warn "broker is YIELDING (reason: $(say "$status" | jq -r .yield.reason)) — inference is refused right now"
  [ "$mode" != "auto" ] && warn "manual override active: mode=$mode (someone forced it via POST /control; auto is normal)"
  [ "$busy" = "true" ] && say "note: GPU slot in use (inflight request) — lane requests will queue; NOT an error"
  { [ "$iq" -gt 0 ] || [ "$bq" -gt 0 ]; } 2>/dev/null && warn "waiters queued (interactive=$iq batch=$bq) — long in-flight request or storm"
else
  say "$status"
  say "(install jq for parsed WARN checks)"
fi

say "-- key metrics --"
curl -s --max-time $T "$CTRL/metrics" 2>/dev/null | grep -E '^broker_(yielding|busy|inflight|max_inflight|queue_depth|requests_total|jobs\{)' || warn "metrics unreachable"

say "-- lane reachability (cheap GET /api/tags via each lane; may queue behind an in-flight generation) --"
for p in 11435 11436; do
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time $T "$HOST:$p/api/tags" 2>/dev/null)
  if [ "$code" = "200" ]; then
    say "  :$p OK"
  elif [ "$code" = "000" ]; then
    say "  :$p timeout ($T s) — usually the GPU slot is held by a long generation (check queue.busy above), not an outage"
  else
    say "  :$p HTTP $code"
  fi
done
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time $T "$HOST:11438/healthz" 2>/dev/null)
say "  :11438 (embed lane) HTTP $code — 000/refused means INFINITY_URL unset or lane down; 404 is fine (no /healthz there, but the listener answered)"
