# Steps: OpenAI-Compatible Upstream Backend

## Prerequisites
None. This feature requires no external setup or prior completion — all work is contained within the repo and validated against in-process mock servers.

## Implementation steps

### Step 1: Add upstream-backend config fields and validation
**What**: Add `UPSTREAM_BACKEND`, `UPSTREAM_URL`, and `UPSTREAM_API_KEY` env-var parsing to `internal/config/config.go`, with validation that ensures `UPSTREAM_BACKEND` is one of `ollama` or `openai`, `OLLAMA_URL` is required only when `UPSTREAM_BACKEND=ollama` (not unconditionally), `UPSTREAM_URL` is parsed/validated only when backend is `openai`, and `UPSTREAM_API_KEY` is rejected at config load if it contains CR/LF control characters. Implement logging of config initialization that never logs `UPSTREAM_API_KEY`'s value.
**Files**: `internal/config/config.go`
**Test**: Run `UPSTREAM_BACKEND=bogus go run ./cmd/broker` and verify a fatal error names the invalid backend; run `UPSTREAM_BACKEND=openai go run ./cmd/broker` without `UPSTREAM_URL` and verify a fatal error; run with `UPSTREAM_BACKEND=openai UPSTREAM_URL=http://127.0.0.1:8000` and verify startup succeeds past config load; verify that `UPSTREAM_API_KEY` value never appears in logs even when set (code-review assertion or test capturing log output); run `UPSTREAM_BACKEND=openai UPSTREAM_URL=http://127.0.0.1:8000 UPSTREAM_API_KEY=$'bad\r\nkey' go run ./cmd/broker` and verify fatal error on CR/LF; run `UPSTREAM_BACKEND=ollama go run ./cmd/broker` without `OLLAMA_URL` and verify fatal error.
**Depends on**: none
**Parallelizable**: Yes

### Step 2: Extract error handling from proxy into a reusable helper
**What**: Refactor `internal/proxy/proxy.go`'s `errorHandler` to extract the context.Canceled and context.DeadlineExceeded branches into an exported `WriteUpstreamError(w http.ResponseWriter, r *http.Request, err error) bool` function that returns true if it handled the error (context case), leaving the 502 fallback for the caller.
**Files**: `internal/proxy/proxy.go`
**Test**: Run existing `internal/proxy` tests and verify all pass (byte-identical behavior for the `ollama` backend).
**Depends on**: none
**Parallelizable**: Yes

### Step 3: Define the Backend abstraction interface
**What**: Create `internal/backend/backend.go` with a `Backend` interface exposing `Proxy() http.Handler`, `Generate(ctx, model, prompt string, options map[string]any, onTokens func(int)) (string, error)`, `Reachable(ctx context.Context) error`, `Unloader() yield.Unloader`, and a factory `New(cfg *config.Config) (Backend, error)` that branches on `cfg.UpstreamBackend`.
**Files**: `internal/backend/backend.go`
**Test**: Verify the package compiles; the `Backend` interface exists with all four methods; the `New()` factory function signature is correct and accepts `*config.Config`.
**Depends on**: Step 1
**Parallelizable**: No (depends on config validation)

### Step 4: Implement the Ollama backend wrapper
**What**: Create `internal/backend/ollama_backend.go` with an `ollamaBackend` struct wrapping the existing `proxy.New(cfg.OllamaURL)` pass-through and a fresh `ollama.Client`; implement the `Generate()` method body using the existing `genAdapter.Generate` logic from `cmd/broker/main.go` (copy the logic, do NOT delete from main.go yet — deletion happens in Step 11); `Reachable()` calls `oc.LoadedModels(ctx)`; `Unloader()` returns the `*ollama.Client`.
**Files**: `internal/backend/ollama_backend.go`
**Test**: Wire the ollama backend into a test fixture, send a mock Ollama request through `Proxy()`, verify a pass-through to the upstream; call `Generate()` with a mock Ollama endpoint, verify it returns a concatenated response string; call `Reachable()` and verify it succeeds against a live/mock Ollama upstream and fails when unreachable.
**Depends on**: Step 3
**Parallelizable**: No (depends on Backend interface)

### Step 5a: Implement the OpenAI client—request construction and error mapping
**What**: Create `internal/openaicompat/client.go` with a `Client` struct wrapping `*url.URL` and an API key; implement request construction for `/v1/chat/completions`: decode input, build the JSON request body with `stream:true` and `stream_options:{include_usage:true}`, POST to the upstream using proper URL joining (not string concatenation), include the API key as `Authorization: Bearer` header, and map non-2xx responses to the error format returned by `Generate()`. Use a reusable `http.Client` with a `retryTransport` pattern (mirror `internal/proxy`'s existing retry logic where possible) rather than a bare client.
**Files**: `internal/openaicompat/client.go`
**Test**: Create an `httptest.Server` mock that returns a 500 error, wire it into the client, call `Generate()` on it, and verify the error is returned correctly (not panicked); verify that a connection-level failure is retried before the first byte arrives (check that the mock is called multiple times); verify that the URL to the upstream is properly joined (log or inspect the actual request URL sent to the mock, assert it matches `{baseURL}/v1/chat/completions`).
**Depends on**: Step 1
**Parallelizable**: Yes

### Step 5b: Implement the OpenAI client—SSE parsing and token counting
**What**: Create `internal/openaicompat/stream.go` with SSE parsing logic: read the HTTP response body line-by-line (with a configurable buffer-size fix to handle multi-line `data:` fields), split on `data: ` prefix, parse each event as JSON, call `onTokens` for each chunk's token count (immediately, not buffered), accumulate response text, and handle the `[DONE]` sentinel. Implement a fallback: if `usage` is missing in the final event, use a running per-chunk token count that mirrors `ollama.Client.Generate`'s behavior. Detect mid-stream errors (non-200 status codes in error events) and return them.
**Files**: `internal/openaicompat/stream.go`
**Test**: Create an `httptest.Server` mock that emits OpenAI-shaped SSE chunks, wire it into the client, call `Generate()`, and verify: (1) multiple `onTokens` calls arrive during streaming (not buffered until the end); (2) token counts are correct across chunks; (3) final response text concatenates all chunks correctly; (4) a mock that streams 2 valid chunks then sends a malformed/error event — assert the client receives the valid chunks, the running token count is correct, and the error is returned.
**Depends on**: Step 5a
**Parallelizable**: No (depends on client.go)

### Step 6: Implement OpenAI embeddings translation
**What**: Create `internal/openaicompat/embed.go` with request/response reshaping for `/api/embed` ↔ `/v1/embeddings`: decode Ollama's `{model, input, ...}`, POST to `/v1/embeddings` on the upstream, reshape the response from OpenAI's `{data: [{embedding: [...]}], ...}` back into Ollama's `{embeddings: [[...], ...], ...}`, and return as a single JSON response (no streaming).
**Files**: `internal/openaicompat/embed.go`
**Test**: Create a mock OpenAI `/v1/embeddings` endpoint, call through the handler at `/api/embed` with Ollama request shape, verify the response is Ollama embeddings shape and identical to what a direct OpenAI embeddings call returns (reshaped).
**Depends on**: Step 5b
**Parallelizable**: No (depends on client.go for HTTP construction pattern)

### Step 7a: Implement the OpenAI handler—core translation and streaming
**What**: Create `internal/openaicompat/handler.go` with `NewHandler(base *url.URL, apiKey string) http.Handler`. Implement the `/api/generate` and `/api/chat` path routing: decode the Ollama request, construct an OpenAI `/v1/chat/completions` request. Implement stream:true path: POST to upstream, read response as SSE, write Ollama NDJSON lines as chunks arrive, call `ResponseWriter.Flush()` after each line to ensure streaming (matching `FlushInterval: -1`). Implement stream:false path: buffer the entire response as a single JSON object and write it once. Hard-reject requests that include unsupported fields (e.g., `images` on a message) by returning 400 before calling the upstream.
**Files**: `internal/openaicompat/handler.go`
**Test**: Wire the handler into an `httptest.Server`, send Ollama-shaped chat/generate requests with `stream:true`, verify: (1) the response is Ollama NDJSON format; (2) response is streamed (multiple flushes observed, not one final write); (3) for stream:false, verify a single JSON object is returned, not NDJSON. Send a request with `images` field, verify 400 response before the mock upstream is called.
**Depends on**: Steps 2, 5b, 6
**Parallelizable**: No (depends on error helper, client, and embed)

### Step 7b: Implement the OpenAI handler—error handling and recovery
**What**: Add error handling to the openai handler: on pre-response error (upstream unreachable, non-2xx before streaming, or `r.Context()` canceled), call `proxy.WriteUpstreamError()` and check its bool return. If it returns false (error not handled, e.g. some other error type), write a 502 response directly. For mid-stream errors (error event received after streaming has begun), write one final NDJSON line with the error field and the Ollama error shape, without attempting to write another HTTP status. Add a `recover()` around the streaming write loop to catch panics from a bad ResponseWriter, log them, and return without crashing.
**Files**: `internal/openaicompat/handler.go` (extends 7a)
**Test**: Mock an upstream that returns a 500 error before streaming, assert the handler calls `WriteUpstreamError()` and writes appropriate error response; create a mock upstream that streams 2 valid chunks then sends an error event, assert the client sees the chunks followed by one final NDJSON error line (no WriteHeader panic, no duplicate status write); create a mock ResponseWriter that panics on Write, assert the handler recovers and logs the panic instead of crashing.
**Depends on**: Step 7a
**Parallelizable**: No (depends on 7a)

### Step 7c: Implement the OpenAI handler—hard-reject unsupported fields
**What**: Implement field validation in the openai handler: before constructing the upstream request, scan the decoded Ollama request for unsupported OpenAI-compatible fields (e.g., `images` in chat messages) and return 400 with a descriptive error message if found. This prevents silent degradation and documents the limitation to callers.
**Files**: `internal/openaicompat/handler.go` (extends 7b)
**Test**: POST a chat request with an `images` field on a message, assert 400 response and a descriptive error body; verify the upstream mock was never called (no request reached it). POST a valid request without unsupported fields, assert it succeeds.
**Depends on**: Step 7b
**Parallelizable**: No (depends on 7b)

### Step 8: Add X-Broker-*/trailer headers for openai backend
**What**: Create a new step that integrates the openai handler response headers with the broker's header/trailer contract: wire the openai handler through a test that uses the real `queue.Gate` (not called in isolation), send both a successful streamed request and a deferred/error request, and verify that `X-Broker-Request-Id`, `X-Broker-Wait-Ms`, and `X-Broker-Status` (both as a response header and as a trailer) appear with correct values.
**Files**: `internal/openaicompat/handler_test.go` (or extend existing test)
**Test**: Create an integration test that wires `queue.Gate` → openai handler → mock upstream, send a request, capture the response headers and trailers, and verify `X-Broker-Request-Id`, `X-Broker-Wait-Ms`, and `X-Broker-Status` are present with correct values (request ID is a UUID, wait is non-negative, status is "queued"/"running"/etc.).
**Depends on**: Step 7c
**Parallelizable**: No (depends on handler complete)

### Step 9: Add recover() in yield.go's doUnload()
**What**: Modify `internal/yield/yield.go`'s `doUnload()` goroutine to wrap the `Unloader.Unload()` call in a `recover()` block: catch any panic, log it with context (the unloader type and panic value), and allow the goroutine to exit gracefully instead of crashing the broker process. This is defensive against the typed-nil `Unloader` panic risk.
**Files**: `internal/yield/yield.go`
**Test**: Create a unit test with a fake `Unloader` whose `Unload()` method panics with a known value, wire it into `yield.NewWithConfirm()`, trigger the unload, and verify: (1) the broker/goroutine does not crash; (2) the panic is logged (capture logs and assert the panic message appears); (3) the goroutine exits cleanly.
**Depends on**: none
**Parallelizable**: Yes

### Step 10: Add openai backend wrapper
**What**: Create `internal/backend/openai_backend.go` with an `openaiBackend` struct wrapping `openaicompat.NewHandler()` and `openaicompat.Client`; `Proxy()` returns the handler; `Generate()` wraps `openaicompat.Client.Generate()`; `Reachable()` GETs `{UPSTREAM_URL}/v1/models` (via proper URL joining) to probe the upstream; `Unloader()` must `return nil` as a direct, literal interface-typed return with NO intermediate typed variable (e.g. `func (b *openaiBackend) Unloader() yield.Unloader { return nil }`) — a typed-nil concrete pointer boxed into the interface would defeat `yield.go`'s nil-guard at line 216 and panic `doUnload()` on a nil receiver; Step 9's `recover()` is defense-in-depth, not a substitute for getting this literal return right.
**Files**: `internal/backend/openai_backend.go`
**Test**: Wire the openai backend into a test fixture with a mock upstream, send Synchronous and Job-path requests through it, verify `Reachable()` returns true when the upstream is reachable and error when unreachable.
**Depends on**: Steps 3, 5b, 6, 7c
**Parallelizable**: No (depends on all openaicompat pieces)

### Step 11: Update main.go to wire backends
**What**: Replace the inline `proxy.New()`, `ollama.Client`, and `genAdapter` in `cmd/broker/main.go` with `backend.New(cfg)`, wiring `be.Proxy()` into both Ollama-API Gate instances (interactive `:11435`, batch `:11436`), `be` as the job.Worker Generator, `be.Reachable()` into the `healthCheck` closure, and `be.Unloader()` into `yield.NewWithConfirm()`. Explicitly note in code comments that the embed lane (`internal/proxy.NewEmbed`, `INFINITY_URL`, its own scheduler) is NEVER touched by `backend.New()` and continues to front Infinity directly regardless of `UPSTREAM_BACKEND`'s value.
**Files**: `cmd/broker/main.go`
**Test**: Run the Broker with `UPSTREAM_BACKEND=ollama` (default, unset), verify it starts and `/healthz` returns 200; run with `UPSTREAM_BACKEND=openai UPSTREAM_URL=http://127.0.0.1:8000` and a mock upstream, verify startup succeeds and `/healthz` probes the openai backend; send a request to the embed lane (to Infinity URL), verify it reaches the Infinity mock/real upstream completely unaffected by `UPSTREAM_BACKEND` setting.
**Depends on**: Steps 4, 10
**Parallelizable**: No (depends on both backends)

### Step 12: Add backend factory tests
**What**: Create `internal/backend/backend_test.go` that calls `backend.New(cfg)` with configs for each backend type (ollama and openai), verifies the returned concrete type, and checks that `Reachable()` and `Unloader()` behave as expected (ollama returns a non-nil Unloader, openai returns nil). Add a test that explicitly verifies Step 3's dependency-reduced Test: call `New()` with both backends, confirm non-nil Backends are returned for each.
**Files**: `internal/backend/backend_test.go`
**Test**: Run `go test ./internal/backend` and verify all cases pass.
**Depends on**: Steps 3, 4, 10
**Parallelizable**: No (depends on both backends)

### Step 13: Add OpenAI client and handler integration tests
**What**: Extend `internal/openaicompat/` with comprehensive test fixtures: `client_test.go` covering chat/generate streaming, token count accuracy, and error handling; `handler_test.go` covering chat/generate and embeddings round-trips with multiple flushed writes for streaming verification, error responses, context cancellation, stream:false buffering, hard-reject field validation, and mid-stream error handling. Both use `httptest.NewServer` as a mock OpenAI-compatible upstream.
**Files**: `internal/openaicompat/client_test.go`, `internal/openaicompat/handler_test.go`
**Test**: Run `go test ./internal/openaicompat` and verify all tests pass; observe multiple `io.WriteCloser` writes in the streaming tests (not buffered into one final write).
**Depends on**: Steps 5b, 6, 7c
**Parallelizable**: No (depends on openaicompat package complete)

### Step 14: Parameterize existing job-worker and proxy tests for both backends
**What**: Identify a small number of existing tests in `internal/job/worker_test.go` and `internal/proxy/proxy_test.go` that exercise core behavior (preemption mid-Job, `/healthz` failure reporting, Yield/cancellation) and parameterize or duplicate them to run against both the ollama and openai mock upstreams, verifying identical outcomes (acceptance criteria AC-7/8/9).
**Files**: `internal/job/worker_test.go`, `internal/proxy/proxy_test.go` (read/verify fixture patterns first, then extend)
**Test**: Run `go test ./internal/job ./internal/proxy` and verify parameterized tests pass for both backends. Explicitly verify: AC-7 (Job preempted mid-Yield requeues to front) — assert the Job's position becomes exactly 1 in QUEUED state for both backends; AC-8 (interactive preempts batch past BROKER_BATCH_QUANTUM) — assert the specific preemption trigger timing is identical for both backends; AC-9 (/healthz names the correct failed dependency) — assert the exact JSON error body names "upstream" for both backends when their respective mock is unreachable.
**Depends on**: Steps 4, 10, 13
**Parallelizable**: No (depends on all pieces)

### Step 15: Verify ollama backend structural parity with existing codebase
**What**: Run the full existing test suite (`go test ./...`) with `UPSTREAM_BACKEND=ollama` (default, unset), confirm all tests pass unmodified. Perform a structural assertion (via reflection, type-checking, or code-level inspection, NOT a wall-clock benchmark) confirming that `UPSTREAM_BACKEND=ollama` produces a `Backend` whose `Proxy()` returns the exact same `httputil.ReverseProxy`-based handler type/construction as the pre-feature code, i.e. no additional wrapping/dispatch layer is invoked for the ollama backend.
**Files**: (read-only verification)
**Test**: `go test ./...` with `UPSTREAM_BACKEND` unset; all tests pass. Create a test that instantiates both `backend.New(cfg)` with ollama and the old `proxy.New()` side-by-side, and via reflection/type inspection, assert they produce handlers of the same underlying type (not wrapped).
**Depends on**: Steps 1–14 (all features completed)
**Parallelizable**: No (full validation gate)

### Step 16a: Write acceptance test suite—streaming, embeddings, job lifecycle, config
**What**: Create a comprehensive integration test file (e.g., `internal/acceptance_test.go`) that validates: AC-1 (existing tests pass unmodified with UPSTREAM_BACKEND unset), AC-2 (UPSTREAM_BACKEND=bogus fails config.Load()), AC-3 (openai backend missing UPSTREAM_URL fails config.Load()), AC-4 (streaming chat/generate with stream:true, NDJSON, multiple flushed writes), AC-5 (text-embeddings round-trip, Ollama shape preserved), AC-6 (Job runs to SUCCEEDED with increasing token count via GET/SSE).
**Files**: `internal/acceptance_test.go` (new)
**Test**: Run `go test ./internal/acceptance_test.go -run TestAcceptance -v` and verify AC-1 through AC-6 pass.
**Depends on**: Steps 1–15 (all features, foundational validation gates)
**Parallelizable**: No (depends on prior validation)

### Step 16b: Write acceptance test suite—preemption, quantum, yield, healthz
**What**: Extend the acceptance test suite to validate: AC-7 (Job preempted mid-Yield requeues to front of queue, position 1, QUEUED), AC-8 (interactive-lane request preempts a batch job past BROKER_BATCH_QUANTUM time, identical timing to ollama backend), AC-9 (/healthz returns 200 when upstream reachable, 503 naming the upstream when unreachable, for the openai backend).
**Files**: `internal/acceptance_test.go` (extends 16a)
**Test**: Run tests for AC-7/8/9; verify exact queue positions, preemption timing, and healthz JSON shapes match the contract.
**Depends on**: Step 16a
**Parallelizable**: No (depends on 16a)

### Step 16c: Write acceptance test suite—observability
**What**: Extend the acceptance test suite to validate: AC-10 (X-Broker-Request-Id/Wait-Ms/Status header and trailer present and correct under the openai backend), AC-11 (/metrics continues to expose broker_requests_total{class,outcome} and broker_job_outcomes_total{outcome} with existing label values unchanged), AC-12 (the image-embedding embed lane is unaffected by UPSTREAM_BACKEND's value).
**Files**: `internal/acceptance_test.go` (extends 16b)
**Test**: Run tests for AC-10/11/12; capture response headers/trailers, verify presence and format of X-Broker-* fields; scrape /metrics and verify metric names match a known baseline; confirm the embed lane reaches Infinity unaffected by UPSTREAM_BACKEND.
**Depends on**: Step 16b
**Parallelizable**: No (depends on 16b)

### Step 16d: Write acceptance test suite—error mapping, isolation, and the new (post-challenge) requirements
**What**: Extend the acceptance test suite to validate: AC-13 (upstream error → Job FAILED or Synchronous 503/error, consistent with ollama backend), AC-14 (no test makes a real network call, all mock/local), AC-15 (ollama backend is structurally the same httputil.ReverseProxy type — type assertion, not a benchmark), AC-16 (OLLAMA_URL not required when UPSTREAM_BACKEND=openai; still required when ollama), AC-17 (UPSTREAM_API_KEY: Authorization header sent only when non-empty, never logged, CR/LF rejected at config load), AC-18 (stream:false yields a single buffered JSON response, not NDJSON), AC-19 (usage-omitted fallback: Job token count still correct when mock upstream omits usage on its final chunk; separate assertion that outbound request includes stream_options:{include_usage:true}), AC-20 (images field on /api/chat → 400, upstream never called), AC-21 (/api/tags, /api/show, /api/ps, /api/pull → 404 under openai backend, pass through under ollama backend), AC-22 (/api/embed preserves input-to-output embedding order), AC-23 (/api/generate's context field is accepted but has no effect; system field becomes a system-role message; template field is ignored), AC-24 (model name passed through byte-identical to the upstream).
**Files**: `internal/acceptance_test.go` (extends 16c)
**Test**: Run tests for AC-13 through AC-24, one assertion block per criterion as described above; verify embed-lane isolation by sending a request with `UPSTREAM_BACKEND=openai` and confirming Infinity is hit, not the openai mock; verify error codes and shapes; verify stream:false produces single JSON; verify hard-reject on images field; verify logs do not contain API key value; verify 404s on non-chat/embed endpoints; verify embedding order and model-name passthrough.
**Depends on**: Step 16c
**Parallelizable**: No (final validation suite)

## Rollback plan
All steps reversible via git. The feature is additive (new files + config fields + minimal changes to `proxy.go` and `main.go`):
- Revert Step 1 (config): drop env-var parsing.
- Revert Step 2 (proxy): restore original `errorHandler`, inline the Canceled/DeadlineExceeded branches.
- Revert Step 9 (yield): remove the `recover()` block from `doUnload()`.
- Revert Steps 3–10 (backend/openaicompat packages): delete `internal/backend/` and `internal/openaicompat/` directories.
- Revert Step 11 (main.go): restore original `proxy.New()`, `ollama.Client`, `genAdapter` inline.
- Revert Steps 4, 12–16 (in-place edits + test files): revert via `git diff`/`git checkout -- internal/job/worker_test.go internal/proxy/proxy_test.go` (or equivalent) to restore pre-feature versions, then delete new test files (`internal/acceptance_test.go`, `internal/backend/backend_test.go`, `internal/openaicompat/client_test.go`, `internal/openaicompat/handler_test.go`).
- Run full test suite and confirm all original tests pass — byte-identical to pre-feature baseline.
