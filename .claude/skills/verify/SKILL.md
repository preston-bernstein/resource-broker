---
name: verify
description: How to actually run resource-broker locally and drive real HTTP requests through it, for verifying a change end-to-end rather than trusting go test alone. Load before claiming a change "works" when it touches cmd/broker/main.go's wiring or anything request-path-adjacent.
---

# Verify: running the real broker locally

`go test ./...` (including `-race`) proves individual packages behave correctly in isolation. It does **not** prove `cmd/broker/main.go`'s own wiring is correct — `main()` has almost no test seams, and a bug can live entirely in how `main()` connects already-correct pieces together. This was proven directly during the vLLM idle-unload feature (2026-08-15): `buildBroker()` was fully unit-tested and correct, `go test ./... -race` was green, two `parallel-review` passes found nothing — and `main()` still silently used a stale, undecorated backend for `interactiveProxy`/`batchProxy` instead of the one `buildBroker` actually returned, because it referenced its own pre-call `be` variable instead of the returned `activeBackend`. The bug was found only by building the real binary and sending a real HTTP request through it.

**Rule: any change touching `cmd/broker/main.go`'s wiring is not verified until you've run the real binary and driven a real request through the exact code path you changed.** Package-level tests are necessary, not sufficient.

## Build

```sh
make build   # -> bin/resource-broker (static, CGO disabled)
```

## Run against a fake upstream

Point `UPSTREAM_URL` at anything that speaks HTTP — a `net/http/httptest.NewServer` (from a Go test), or a quick fake for manual/shell verification:

```python
# scratch fake_upstream.py — returns a canned OpenAI-shaped response to any POST
import http.server
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length", 0)))
        self.send_response(200); self.send_header("Content-Type","application/json"); self.end_headers()
        self.wfile.write(b'{"choices":[{"message":{"content":"ok"}}]}')
    def do_GET(self):
        self.send_response(200); self.end_headers()
    def log_message(self, *a): pass
http.server.HTTPServer(("127.0.0.1", 19191), H).serve_forever()
```
```sh
python3 fake_upstream.py &   # background it, kill when done
```

Then run the broker itself, pointed at it, with distinct high ports (avoid the real deploy defaults):

```sh
UPSTREAM_BACKEND=openai \
UPSTREAM_URL=http://127.0.0.1:19191 \
UPSTREAM_UNIT_NAME=verify-fake-unit.service \
UPSTREAM_IDLE_TIMEOUT=2s \
BROKER_DETECT_INTERVAL=1s \
BROKER_INTERACTIVE_ADDR=127.0.0.1:19295 \
BROKER_BATCH_ADDR=127.0.0.1:19296 \
BROKER_CONTROL_ADDR=127.0.0.1:19297 \
./bin/resource-broker &
```

Notes:
- `UPSTREAM_UNIT_NAME` doesn't need a real systemd unit for local verification — `systemctl` doesn't exist on macOS at all, and even on a real Linux box an unowned/nonexistent unit just produces a WARN log line (`"vram unload failed"` / `"vram reload failed"`), never a crash. That WARN firing with the right `trigger` field IS the proof the code path ran for real.
- A short `UPSTREAM_IDLE_TIMEOUT` + short `BROKER_DETECT_INTERVAL` makes idle-unload observable in seconds instead of the real default (`1h`/`3s`).
- `cd` into the repo root first — the binary writes `broker-jobs.db` (SQLite) into the current directory; delete it after (`rm -f broker-jobs.db`, gitignored but still clutter).

## Drive real requests

The openai backend only accepts `/api/generate`, `/api/chat`, `/api/embed` (Ollama-shaped paths, translated internally) — not `/v1/chat/completions`:

```sh
curl -s http://127.0.0.1:19297/status | python3 -m json.tool   # check state
curl -s -X POST http://127.0.0.1:19295/api/generate \
  -d '{"model":"fake-model","prompt":"hi"}' -H "Content-Type: application/json"
```

`/status`'s `"idle"` section (when any instance has `UPSTREAM_IDLE_TIMEOUT`/`BROKER_ROUTE_<N>_IDLE_TIMEOUT` set) is the fastest way to prove request-path wiring end-to-end: `idle_unloaded` and `since_last_dispatch` only move if the request actually passed through `yield.Controller.TrackActivity`'s wrapped handler — exactly the thing the shipped bug above silently bypassed.

## Cleanup

```sh
kill %1 %2 2>/dev/null   # broker + fake upstream background jobs
rm -f broker-jobs.db
```

## Automating this as a regression test

`cmd/broker/main_test.go`'s `TestMainInteractiveProxyTracksActivityOnNoRoutesPath` automates exactly this manual recipe (real binary subprocess, real fake upstream via `httptest.NewServer`, real HTTP requests, asserting on real `/status` output) — read it as the canonical example before writing a similar test for a different `main()` wiring change. Its key discipline, learned the hard way while writing it: **a loose time-based assertion threshold can pass in both the buggy and fixed case if the whole test runs fast enough** — the test deliberately sleeps past the configured idle timeout by a wide margin *before* sending its own request, so "reset to near-zero" and "never reset" are unambiguous by more than 2x, not a coin-flip on test-runner speed. Always prove a new discriminating test actually fails against the bug (temporarily revert the fix, run the test, confirm red) before trusting it as a regression guard — this exact test passed against the reintroduced bug on its first draft because its threshold was too loose, and only failed correctly after tightening it.
