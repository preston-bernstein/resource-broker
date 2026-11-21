package broker_test

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/preston-bernstein/resource-broker/internal/config"
	"github.com/preston-bernstein/resource-broker/internal/openaicompat"
	"github.com/preston-bernstein/resource-broker/internal/testhttp"
)

// --- AC-1: default (UPSTREAM_BACKEND unset) behavior is unchanged ---

// TestAcceptance_AC1_OllamaBackendSanity is a light sanity check that this
// acceptance suite itself is wired correctly for the default "ollama"
// backend — it does NOT re-run the rest of the repo's existing test suite
// (that's `go test ./...`, run separately). It proves a Synchronous request
// through the full stack (interactive Gate -> ollama backend's real
// httputil.ReverseProxy -> a mock Ollama upstream) still passes through
// unchanged, and carries the broker's own response headers.
func TestAcceptance_AC1_OllamaBackendSanity(t *testing.T) {
	upstream := pathUpstream(t, "/api/generate", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"model": "m", "response": "hi", "done": true})
	})
	r := newRig(t, ollamaCfg(t, upstream.URL))

	resp := post(t, r.interactive.URL+"/api/generate", `{"model":"m","prompt":"hi"}`)
	requireStatus(t, resp, http.StatusOK)
	if got := resp.Header.Get("X-Broker-Request-Id"); got == "" {
		t.Error("X-Broker-Request-Id header missing: request did not go through the Gate")
	}
	if body := decodeObject(t, resp.Body); body["response"] != "hi" {
		t.Errorf("response = %v, want pass-through %q", body["response"], "hi")
	}
}

// --- AC-2/AC-3: config.Load() validation ---

func TestAcceptance_AC2_InvalidUpstreamBackendRejected(t *testing.T) {
	t.Setenv("UPSTREAM_BACKEND", "bogus")

	cfg, err := config.Load()
	if err == nil {
		t.Fatal("config.Load() err = nil, want a non-nil error naming the invalid value")
	}
	if cfg != nil {
		t.Errorf("config.Load() cfg = %+v, want nil on error", cfg)
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error %q does not name the invalid value %q", err.Error(), "bogus")
	}
}

func TestAcceptance_AC3_OpenAIBackendRequiresUpstreamURL(t *testing.T) {
	t.Setenv("UPSTREAM_BACKEND", "openai")
	t.Setenv("UPSTREAM_URL", "")

	cfg, err := config.Load()
	if err == nil {
		t.Fatal("config.Load() err = nil, want a non-nil error: UPSTREAM_BACKEND=openai with no UPSTREAM_URL")
	}
	if cfg != nil {
		t.Errorf("config.Load() cfg = %+v, want nil on error", cfg)
	}
}

// --- AC-4: streaming chat/generate end to end ---

// TestAcceptance_AC4_StreamingChatEndToEnd proves a Synchronous stream:true
// request through the full stack (interactive Gate -> openai backend's real
// translating handler -> a mock OpenAI-compatible upstream) is relayed to the
// client as incrementally flushed Ollama-shaped NDJSON, not buffered until
// the upstream finishes. The mock upstream withholds its second chunk until
// this test has actually read the first NDJSON line off the live HTTP
// response — if the full stack buffered anywhere along the way, reading line
// 1 would hang and readLineWithin's deadline would fail the test.
func TestAcceptance_AC4_StreamingChatEndToEnd(t *testing.T) {
	release := make(chan struct{})
	upstream := pathUpstream(t, "/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("mock upstream ResponseWriter is not a Flusher")
			return
		}
		writeSSE(w, sseDelta("Hello"))
		fl.Flush()
		<-release // withhold the final chunk until the client has read line 1
		writeSSE(w, sseStopWithUsage(2), sseDone)
		fl.Flush()
	})
	r := newRig(t, openaiCfg(t, upstream.URL))

	url := r.interactive.URL + "/api/generate"
	err := testhttp.WithPost(t.Context(), url, strings.NewReader(`{"model":"acceptance-model","prompt":"hi","stream":true}`), func(resp *http.Response) {
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if got := resp.Header.Get("Content-Type"); got != "application/x-ndjson" {
			t.Errorf("Content-Type = %q, want application/x-ndjson", got)
		}
		if got := resp.Header.Get("X-Broker-Request-Id"); got == "" {
			t.Error("X-Broker-Request-Id header missing: request did not go through the Gate")
		}

		br := bufio.NewReader(resp.Body)
		// line1 carries the actual delta text as it arrives (matching real
		// Ollama's own progressive-streaming convention) rather than an empty
		// placeholder — a client rendering tokens live sees "Hello" as soon as
		// this line flushes.
		requireNDJSONLine(t, br, ndjsonLine{done: false, response: "Hello", what: "(an intermediate flush; full stack likely buffered if this hangs)"})

		close(release) // now allow the final chunk

		// The final, done:true line carries an empty response: "Hello" was
		// already relayed on line1, so repeating it here would double the text
		// for any client that concatenates every line's "response" field.
		requireNDJSONLine(t, br, ndjsonLine{done: true, response: "", what: "(the final line)"})
	})
	if err != nil {
		t.Fatalf("%v", err)
	}
}

// ndjsonLine is what a streamed NDJSON line must carry.
type ndjsonLine struct {
	done     bool
	response string
	what     string // names the line in failure messages
}

// requireNDJSONLine reads the next line within 3s and checks its done flag and
// response text.
func requireNDJSONLine(t *testing.T, br *bufio.Reader, want ndjsonLine) {
	t.Helper()
	line, err := readLineWithin(br, 3*time.Second)
	if err != nil {
		t.Fatalf("reading NDJSON line %s: %v", want.what, err)
	}
	chunk := parseLine(t, line)
	if done, _ := chunk["done"].(bool); done != want.done {
		t.Fatalf("line done = %v, want %v %s", chunk["done"], want.done, want.what)
	}
	if chunk["response"] != want.response {
		t.Errorf("line response = %v, want %q relayed %s", chunk["response"], want.response, want.what)
	}
}

// --- AC-5: embeddings end to end ---

// TestAcceptance_AC5_EmbedEndToEnd proves a Synchronous /api/embed request
// through the full stack (batch Gate -> openai backend's real translating
// handler -> a mock OpenAI-compatible /v1/embeddings upstream) produces an
// Ollama-shaped embeddings response.
func TestAcceptance_AC5_EmbedEndToEnd(t *testing.T) {
	upstream := pathUpstream(t, "/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"model": "acceptance-model",
			"data": []map[string]any{
				{"embedding": []float64{0.1, 0.2}, "index": 0},
				{"embedding": []float64{0.3, 0.4}, "index": 1},
			},
		})
	})
	r := newRig(t, openaiCfg(t, upstream.URL))

	resp := post(t, r.batch.URL+"/api/embed", `{"model":"acceptance-model","input":["a","b"]}`)
	requireStatus(t, resp, http.StatusOK)
	var out openaicompat.EmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode embed response: %v", err)
	}
	want := [][]float64{{0.1, 0.2}, {0.3, 0.4}}
	if len(out.Embeddings) != len(want) {
		t.Fatalf("len(embeddings) = %d, want %d", len(out.Embeddings), len(want))
	}
	for i, w := range want {
		if got := out.Embeddings[i]; len(got) != 2 || got[0] != w[0] || got[1] != w[1] {
			t.Errorf("embeddings[%d] = %v, want %v", i, got, w)
		}
	}
}

// --- AC-9: /healthz reflects openai backend upstream reachability ---

// TestAcceptance_AC9_HealthzReflectsUpstreamReachabilityEndToEnd proves GET
// /healthz through the real admin control-plane handler returns
// 200 {"status":"ok"} while the mock OpenAI-compatible upstream is reachable,
// and 503 naming "upstream" as the failed dependency once the upstream goes
// away (its httptest.Server closed, producing a real connection-refused
// error) — exercising the real health.Reachable probe this rig wires
// identically to cmd/broker/main.go's healthCheck (see newRig).
func TestAcceptance_AC9_HealthzReflectsUpstreamReachabilityEndToEnd(t *testing.T) {
	upstream := pathUpstream(t, "/v1/models", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": []map[string]any{{"id": "acceptance-model"}}})
	})
	r := newRig(t, openaiCfg(t, upstream.URL))

	resp := get(t, r.control.URL+"/healthz")
	requireStatus(t, resp, http.StatusOK)
	if status := decodeObject(t, resp.Body)["status"]; status != "ok" {
		t.Errorf(`healthz body status = %q, want "ok"`, status)
	}

	upstream.Close() // upstream now unreachable: connection refused

	resp2 := get(t, r.control.URL+"/healthz")
	requireStatus(t, resp2, http.StatusServiceUnavailable)
	if msg, _ := decodeObject(t, resp2.Body)["error"].(string); !strings.Contains(msg, "upstream") {
		t.Errorf("healthz error %q does not name the failed dependency (want it to mention %q)", msg, "upstream")
	}
}

// --- AC-10/AC-12: observability and the embed lane ---

// TestAcceptance_AC10_BrokerHeadersAndTrailerEndToEnd is a light confirmation
// that a Synchronous streamed chat/generate request through the FULL rig
// (real HTTP client, openai backend, real Gate) still carries
// X-Broker-Request-Id and X-Broker-Wait-Ms as response headers, and
// X-Broker-Status as both a header and (once the stream completes) a
// trailer with the "served" outcome. internal/openaicompat/handler_gate_test.go
// already proves this contract in depth at the Gate+handler level; this test
// only re-derives it through the real end-to-end HTTP path, so it stays
// intentionally short.
func TestAcceptance_AC10_BrokerHeadersAndTrailerEndToEnd(t *testing.T) {
	r := newRig(t, openaiCfg(t, chatUpstream(t, sseDelta("hi"), sseStop, sseDone).URL))

	resp := post(t, r.interactive.URL+"/api/generate", `{"model":"acceptance-model","prompt":"hi","stream":true}`)
	requireStatus(t, resp, http.StatusOK)
	for _, name := range []string{"X-Broker-Request-Id", "X-Broker-Wait-Ms"} {
		if got := resp.Header.Get(name); got == "" {
			t.Errorf("%s header missing", name)
		}
	}
	if got := resp.Header.Get("X-Broker-Status"); got != "served" {
		t.Errorf("X-Broker-Status header = %q, want %q", got, "served")
	}

	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("drain streamed body (to reach the trailer): %v", err)
	}
	if got := resp.Trailer.Get("X-Broker-Status"); got != "served" {
		t.Errorf("X-Broker-Status trailer = %q, want %q", got, "served")
	}
}

// TestAcceptance_AC12_EmbedLaneUnaffectedByUpstreamBackendEndToEnd proves a
// request to the image-embedding embed lane (INFINITY_URL/BROKER_EMBED_ADDR)
// reaches a mock Infinity server through the full rig, identically regardless
// of UPSTREAM_BACKEND's value — the embed lane bypasses backend.New()/
// be.Proxy() entirely (see newRig, mirroring cmd/broker/main.go's own
// InfinityURL-gated block) and always fronts Infinity directly.
func TestAcceptance_AC12_EmbedLaneUnaffectedByUpstreamBackendEndToEnd(t *testing.T) {
	cfgs := map[string]func(*testing.T, string) *config.Config{"ollama": ollamaCfg, "openai": openaiCfg}
	for backendKind, newCfg := range cfgs {
		t.Run(backendKind, func(t *testing.T) {
			var gotPath, gotBody atomic.Value
			infinity := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				gotPath.Store(r.URL.Path)
				gotBody.Store(string(raw))
				writeJSON(w, map[string]any{"data": []map[string]any{{"embedding": []float64{0.5}}}})
			}))

			// The Ollama/openai upstream is deliberately never reached by this
			// test (nothing here should call it); a closed server proves that
			// if the embed lane were mistakenly routed through backend.New(),
			// the request would fail loudly instead of silently succeeding.
			deadUpstream := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("the ollama/openai upstream must never be reached by an embed-lane request")
			}))
			deadUpstream.Close()

			cfg := newCfg(t, deadUpstream.URL)
			cfg.InfinityURL = mustParseURL(t, infinity.URL)
			r := newRig(t, cfg)
			if r.embed == nil {
				t.Fatal("rig embed lane not wired despite cfg.InfinityURL set")
			}

			resp := post(t, r.embed.URL+"/v1/embeddings", `{"input":["a picture"]}`)
			requireStatus(t, resp, http.StatusOK)
			if got := gotPath.Load(); got != "/embeddings_image" {
				t.Errorf("mock Infinity server saw path %q, want %q (embed lane's own route rewrite)", got, "/embeddings_image")
			}
			if got := gotBody.Load(); got != `{"input":["a picture"]}` {
				t.Errorf("mock Infinity server saw body %q, want pass-through of the request body", got)
			}
		})
	}
}
