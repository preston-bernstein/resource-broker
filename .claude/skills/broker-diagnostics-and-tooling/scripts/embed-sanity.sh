#!/usr/bin/env bash
# embed-sanity.sh — verify the embed lane (:11438) returns IMAGE-tower embeddings,
# not the text-tower trap (ADR-0008: Infinity's unified /embeddings tokenizes a
# base64 data: URI as TEXT, making every image embed near-identical).
# Method: embed a red and a blue 8x8 PNG; cosine similarity must be well below ~0.99.
# Usage: BROKER_HOST=http://desktop.example.internal ./embed-sanity.sh
# Read-only in spirit: sends two tiny embedding requests through the lane (CPU-side,
# yield-gated); no state is written, no GPU touched. Requires python3.
set -u
HOST="${BROKER_HOST:-http://desktop.example.internal}"
URL="$HOST:11438/embeddings"
MODEL="siglip-so400m-patch14-384"
RED="iVBORw0KGgoAAAANSUhEUgAAAAgAAAAICAIAAABLbSncAAAAEklEQVR4nGP4z8CAFWEXHbQSACj/P8Fu7N9hAAAAAElFTkSuQmCC"
BLUE="iVBORw0KGgoAAAANSUhEUgAAAAgAAAAICAIAAABLbSncAAAAEElEQVR4nGNgYPiPAw0pCQCpcD/BFMrqcwAAAABJRU5ErkJggg=="

embed() {
  curl -s --max-time 60 "$URL" -H 'Content-Type: application/json' \
    -d "{\"model\":\"$MODEL\",\"input\":[\"data:image/png;base64,$1\"]}"
}

r1=$(embed "$RED");  rc1=$?
r2=$(embed "$BLUE"); rc2=$?
if [ $rc1 -ne 0 ] || [ $rc2 -ne 0 ] || [ -z "$r1" ] || [ -z "$r2" ]; then
  echo "FAIL: embed lane unreachable at $URL — INFINITY_URL likely unset (lane not started) or Infinity down on 127.0.0.1:7997."
  echo "Check: curl -s \$BROKER_HOST:11437/status ; journalctl -u resource-broker | grep 'embed lane enabled'"
  exit 1
fi
case "$r1" in *'"error"'*) echo "FAIL: lane returned an error: $r1"; exit 1;; esac
case "$r1" in *'503'*|*yielding*|*"GPU busy"*) echo "DEFER: broker refused admission (yielding to gaming/Plex, or wait budget exceeded) — rerun when idle."; exit 2;; esac

python3 - "$r1" "$r2" <<'PY'
import json, sys, math
def vec(s):
    d = json.loads(s)
    return d["data"][0]["embedding"]
try:
    a, b = vec(sys.argv[1]), vec(sys.argv[2])
except Exception as e:
    print(f"FAIL: could not parse embedding responses ({e}). Raw1: {sys.argv[1][:200]}")
    sys.exit(1)
dot = sum(x*y for x, y in zip(a, b))
na, nb = math.sqrt(sum(x*x for x in a)), math.sqrt(sum(x*x for x in b))
cos = dot / (na * nb)
print(f"cosine(red, blue) = {cos:.4f}  (dim={len(a)})")
if cos > 0.99:
    print("FAIL: vectors near-identical -> text-tower trap or lane misroute.")
    print("The request is NOT reaching Infinity's /embeddings_image. Verify you hit the")
    print("BROKER :11438 (which rewrites the path), not Infinity :7997 directly.")
    sys.exit(1)
print("PASS: distinct images produce distinct image-tower embeddings.")
PY
