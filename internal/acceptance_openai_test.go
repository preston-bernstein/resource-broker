package broker_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/preston-bernstein/resource-broker/internal/config"
)

// --- AC-13..AC-24: error mapping, isolation, and the post-spec-challenge
// requirements ---
//
// Most of what follows has substantial unit-level coverage already
// (internal/openaicompat/*_test.go, internal/backend/*_test.go,
// internal/job/worker_test.go, internal/config/config_test.go) — these tests
// stay focused on proving the full wiring through the real rig rather than
// re-deriving unit-level translation logic.

// TestAcceptance_AC13_SynchronousUpstreamErrorEndToEnd proves that with
// UPSTREAM_BACKEND=openai, a Synchronous request whose mock upstream returns
// a non-2xx status before any response bytes are sent produces a non-2xx
// error response through the real interactive Gate — the same error-response
// contract the ollama backend's errorHandler produces today (both route
// through proxy.WriteUpstreamError/its 502 fallback; see
// internal/openaicompat/handler_request_test.go's TestServeChat_
// PreResponseUpstream500_Returns502 for the exhaustive handler-level proof
// this only re-derives through the full stack).
func TestAcceptance_AC13_SynchronousUpstreamErrorEndToEnd(t *testing.T) {
	upstream := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	r := newRig(t, openaiCfg(t, upstream.URL))

	resp := post(t, r.interactive.URL+"/api/generate", `{"model":"acceptance-model","prompt":"hi"}`)
	if resp.StatusCode < 400 {
		t.Fatalf("status = %d, want a non-2xx error response consistent with the ollama backend's upstream-error handling", resp.StatusCode)
	}
	if msg, _ := decodeObject(t, resp.Body)["error"].(string); msg == "" {
		t.Error("error response body carries no error message")
	}
}

// TestAcceptance_AC14_NoRealNetworkCallAudit is a regression guard for a
// manual audit already performed for this task: every mock upstream this
// feature's test suite (internal/openaicompat, internal/backend, internal/
// job, and this suite) talks to is an in-process httptest.Server bound to a
// loopback address, generated at runtime — grepping every _test.go source
// file in those packages for a literal http(s):// URL turns up nothing but
// 127.0.0.1/localhost. This test pins that fact so a future test can't
// silently reintroduce a hardcoded external host; it does not re-run or
// rebuild the audit itself.
func TestAcceptance_AC14_NoRealNetworkCallAudit(t *testing.T) {
	// Relative to internal/ (this file's own directory, the go test cwd).
	for _, dir := range []string{".", "openaicompat", "backend", "job"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir(%s): %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			path := dir + "/" + e.Name()
			for _, u := range nonLocalURLs(t, path) {
				t.Errorf("%s: literal non-local URL %q — every mock upstream in this feature's test suite must be an in-process httptest.Server (AC-14)", path, u)
			}
		}
	}
}

// nonLocalURLs returns each literal http(s):// URL in the file at path that
// does not point at loopback.
func nonLocalURLs(t *testing.T, path string) []string {
	t.Helper()
	localOnly := regexp.MustCompile(`^https?://(127\.0\.0\.1|localhost)(:[0-9]+)?(/|$)`)
	urlRe := regexp.MustCompile(`https?://[^\s"'` + "`" + `]+`)
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	var found []string
	for _, m := range urlRe.FindAllString(string(raw), -1) {
		if !localOnly.MatchString(m) {
			found = append(found, m)
		}
	}
	return found
}

// AC-15 (structural parity: UPSTREAM_BACKEND=ollama constructs the exact
// pre-feature httputil.ReverseProxy, no dispatch/translation layer
// interposed) is already proven by
// internal/backend/parity_test.go's TestOllamaBackendProxyStructuralParity —
// confirmed present and passing; not duplicated here since it is not an
// end-to-end HTTP-surface concern, it's a construction-time type assertion the
// acceptance rig has no way to observe (newRig only ever sees the
// backend.Backend interface, never the concrete ollamaBackend/ReverseProxy
// types that test asserts on).

// TestAcceptance_AC16_ConfigLoadBackendURLRequirementEndToEnd proves, through
// config.Load() (not a hand-built config.Config literal like ollamaCfg/
// openaiCfg use) followed by actually starting the full rig, that
// UPSTREAM_BACKEND=openai with OLLAMA_URL unset both loads successfully and
// serves a real request — the end-to-end value internal/config's
// TestLoadUpstreamBackendOpenAIWithValidUpstreamURL can't provide on its own
// (it never starts a rig). The reverse case (UPSTREAM_BACKEND=ollama with
// OLLAMA_URL invalid fails Load()) has no rig to start, so it's asserted
// directly against config.Load(), matching config's existing
// TestLoadUpstreamBackendOllamaWithoutOllamaURL — included here only for a
// single coherent AC-16 proof, not because it adds new coverage.
func TestAcceptance_AC16_ConfigLoadBackendURLRequirementEndToEnd(t *testing.T) {
	t.Run("openai backend starts and serves without OLLAMA_URL", func(t *testing.T) {
		upstream := chatUpstream(t, sseDelta("hi"), sseStop, sseDone)

		t.Setenv("UPSTREAM_BACKEND", "openai")
		t.Setenv("UPSTREAM_URL", upstream.URL)
		t.Setenv("OLLAMA_URL", "") // explicitly unset

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("config.Load() with UPSTREAM_BACKEND=openai and OLLAMA_URL unset: %v", err)
		}
		if cfg.OllamaURL != nil {
			t.Errorf("cfg.OllamaURL = %v, want nil when UPSTREAM_BACKEND=openai", cfg.OllamaURL)
		}

		r := newRig(t, cfg) // must not fail: proves the full production wiring path (config.Load -> backend.New -> Gate) tolerates a nil OllamaURL
		requireStatus(t, post(t, r.interactive.URL+"/api/generate", `{"model":"acceptance-model","prompt":"hi","stream":false}`), http.StatusOK)
	})

	t.Run("ollama backend fails Load() without a valid OLLAMA_URL", func(t *testing.T) {
		t.Setenv("UPSTREAM_BACKEND", "ollama")
		t.Setenv("OLLAMA_URL", "not-a-url")
		if _, err := config.Load(); err == nil {
			t.Fatal("config.Load() err = nil, want a non-nil error: UPSTREAM_BACKEND=ollama with an invalid OLLAMA_URL")
		}
	})
}

// authCapturingUpstream is a chat upstream that records the Authorization
// header of the request it receives.
func authCapturingUpstream(t *testing.T, gotAuth *atomic.Value) string {
	t.Helper()
	return startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		writeSSE(w, sseDelta("hi"), sseStop, sseDone)
	})).URL
}

// TestAcceptance_AC17_APIKeyHeaderAndLogSafetyEndToEnd proves, through the
// real rig, that UPSTREAM_API_KEY reaches the mock upstream as an
// Authorization: Bearer <value> header when non-empty and is never sent when
// empty, and that the raw key never appears in a captured slog stream during
// a real request — the CR/LF-rejected-at-Load() half of AC-17 is already
// pinned by internal/config's TestLoadUpstreamAPIKeyControlCharRejected and
// not duplicated here.
func TestAcceptance_AC17_APIKeyHeaderAndLogSafetyEndToEnd(t *testing.T) {
	const canaryKey = "sk-acceptance-9f3c7b2a-secret"

	t.Run("non-empty key sent as Authorization header and never logged", func(t *testing.T) {
		var gotAuth atomic.Value
		cfg := openaiCfg(t, authCapturingUpstream(t, &gotAuth))
		cfg.UpstreamAPIKey = canaryKey
		r := newRig(t, cfg)

		var logBuf bytes.Buffer
		prevLogger := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
		defer slog.SetDefault(prevLogger)

		postDrained(t, r.interactive.URL+"/api/generate", `{"model":"acceptance-model","prompt":"hi"}`)

		if got, _ := gotAuth.Load().(string); got != "Bearer "+canaryKey {
			t.Errorf("mock upstream saw Authorization = %q, want %q", got, "Bearer "+canaryKey)
		}
		if strings.Contains(logBuf.String(), canaryKey) {
			t.Errorf("raw UPSTREAM_API_KEY leaked into logs:\n%s", logBuf.String())
		}
	})

	t.Run("empty key sends no Authorization header", func(t *testing.T) {
		var gotAuth atomic.Value
		r := newRig(t, openaiCfg(t, authCapturingUpstream(t, &gotAuth))) // cfg.UpstreamAPIKey left at its zero value ""

		postDrained(t, r.interactive.URL+"/api/generate", `{"model":"acceptance-model","prompt":"hi"}`)

		if got, _ := gotAuth.Load().(string); got != "" {
			t.Errorf("mock upstream saw Authorization = %q, want no header sent", got)
		}
	})
}

// TestAcceptance_AC18_StreamFalseSingleBufferedResponseEndToEnd proves that
// with UPSTREAM_BACKEND=openai, stream:false through the full rig produces a
// single buffered application/json response body — not NDJSON — complementing
// AC-4's stream:true proof.
func TestAcceptance_AC18_StreamFalseSingleBufferedResponseEndToEnd(t *testing.T) {
	r := newRig(t, openaiCfg(t, chatUpstream(t, sseDelta("Hello"), sseStopWithUsage(1), sseDone).URL))

	resp := post(t, r.interactive.URL+"/api/generate", `{"model":"acceptance-model","prompt":"hi","stream":false}`)
	requireStatus(t, resp, http.StatusOK)
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (not NDJSON)", got)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if n := strings.Count(strings.TrimSpace(string(raw)), "\n"); n != 0 {
		t.Errorf("body contains %d newline(s), want a single buffered JSON object (no NDJSON framing): %s", n, raw)
	}
	body := parseLine(t, string(raw))
	if body["done"] != true {
		t.Errorf("done = %v, want true", body["done"])
	}
	if body["response"] != "Hello" {
		t.Errorf("response = %v, want %q", body["response"], "Hello")
	}
}

// TestAcceptance_AC20_ImagesFieldRejected400EndToEnd proves that with
// UPSTREAM_BACKEND=openai, a /api/chat request whose message carries an
// "images" field is rejected 400 through the full rig, and that the mock
// upstream is never contacted.
func TestAcceptance_AC20_ImagesFieldRejected400EndToEnd(t *testing.T) {
	var upstreamCalled atomic.Bool
	upstream := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	r := newRig(t, openaiCfg(t, upstream.URL))

	resp := post(t, r.interactive.URL+"/api/chat", `{"model":"acceptance-model","messages":[{"role":"user","content":"describe this","images":["base64data"]}]}`)
	requireStatus(t, resp, http.StatusBadRequest)
	if upstreamCalled.Load() {
		t.Error("mock upstream was contacted despite the unsupported images field")
	}
}

// TestAcceptance_AC21_NonChatEndpointsEndToEnd proves that with
// UPSTREAM_BACKEND=openai, /api/tags, /api/show, /api/ps, and /api/pull each
// receive 404 through the full rig without ever reaching the mock upstream,
// while the same requests pass through unchanged under UPSTREAM_BACKEND=
// ollama (which this rig supports directly via ollamaCfg — no rig extension
// needed).
func TestAcceptance_AC21_NonChatEndpointsEndToEnd(t *testing.T) {
	paths := []string{"/api/tags", "/api/show", "/api/ps", "/api/pull"}

	t.Run("openai backend returns 404 without contacting upstream", func(t *testing.T) {
		var upstreamCalled atomic.Bool
		upstream := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upstreamCalled.Store(true)
			w.WriteHeader(http.StatusOK)
		}))
		r := newRig(t, openaiCfg(t, upstream.URL))
		for _, p := range paths {
			if got := get(t, r.interactive.URL+p).StatusCode; got != http.StatusNotFound {
				t.Errorf("openai backend GET %s status = %d, want 404", p, got)
			}
		}
		if upstreamCalled.Load() {
			t.Error("mock upstream was contacted for a non-chat/embed endpoint under the openai backend")
		}
	})

	t.Run("ollama backend passes through unchanged", func(t *testing.T) {
		var mu sync.Mutex
		seen := map[string]bool{}
		upstream := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen[r.URL.Path] = true
			mu.Unlock()
			writeJSON(w, map[string]any{})
		}))
		r := newRig(t, ollamaCfg(t, upstream.URL))
		for _, p := range paths {
			if got := get(t, r.interactive.URL+p).StatusCode; got != http.StatusOK {
				t.Errorf("ollama backend GET %s status = %d, want 200 (pass-through)", p, got)
			}
		}
		mu.Lock()
		defer mu.Unlock()
		for _, p := range paths {
			if !seen[p] {
				t.Errorf("ollama backend never forwarded %s to the upstream", p)
			}
		}
	})
}

// AC-22 (an /api/embed request with multiple inputs returns embeddings in the
// same order) is already fully proven end-to-end by AC-5
// (TestAcceptance_AC5_EmbedEndToEnd), which uses two inputs ("a","b") mapped
// to distinguishable mock vectors ([0.1 0.2] and [0.3 0.4]) and asserts each
// lands at its corresponding output index — not duplicated here. Unit-level
// order preservation is additionally pinned by internal/openaicompat/
// embed_test.go's TestClientEmbed_PreservesInputOrder.

// TestAcceptance_AC23_AC24_GenerateFieldHandlingAndModelPassthroughEndToEnd
// proves, in one full-rig /api/generate request, that: the context field is
// accepted without error and has no effect on the outbound request; the
// system field becomes a prepended system-role message; the template field is
// ignored without error; and the model value reaches the mock upstream byte-
// identical to what the Consumer supplied (AC-24) — combined into a single
// wiring proof since all four assertions come from inspecting one captured
// outbound request body. Unit-level coverage of each field individually
// already exists in internal/openaicompat/handler_request_test.go's
// TestServeGenerate_ContextFieldAccepted/_SystemFieldMappedToSystemMessage/
// _TemplateFieldIgnored.
func TestAcceptance_AC23_AC24_GenerateFieldHandlingAndModelPassthroughEndToEnd(t *testing.T) {
	const wantModel = "acceptance/exact-model-name:v1.2-rc3"
	var gotBody atomic.Value
	upstream := pathUpstream(t, "/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody.Store(string(raw))
		writeSSE(w, sseDelta("ok"), sseStop, sseDone)
	})
	r := newRig(t, openaiCfg(t, upstream.URL))

	reqBody := fmt.Sprintf(`{"model":%q,"prompt":"hi","stream":false,"system":"be terse","template":"{{ .System }}{{ .Prompt }}","context":[1,2,3]}`, wantModel)
	status, _ := postDrained(t, r.interactive.URL+"/api/generate", reqBody)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (context/template must be accepted without error, AC-23)", status)
	}

	raw, _ := gotBody.Load().(string)
	var outbound struct {
		Model    string           `json:"model"`
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal([]byte(raw), &outbound); err != nil {
		t.Fatalf("decode captured outbound request body %q: %v", raw, err)
	}

	if outbound.Model != wantModel {
		t.Errorf("outbound model = %q, want byte-identical %q (AC-24)", outbound.Model, wantModel)
	}
	if len(outbound.Messages) < 2 {
		t.Fatalf("outbound messages = %v, want a system-role message prepended before the prompt-derived user message (AC-23)", outbound.Messages)
	}
	if outbound.Messages[0]["role"] != "system" || outbound.Messages[0]["content"] != "be terse" {
		t.Errorf("outbound messages[0] = %v, want {role:system content:%q} (AC-23 system field mapping)", outbound.Messages[0], "be terse")
	}
	if strings.Contains(raw, "template") || strings.Contains(raw, `"context"`) {
		t.Errorf("outbound request body leaked the ignored template/context fields (AC-23: neither has an OpenAI-compatible equivalent and must never be forwarded): %s", raw)
	}
}
