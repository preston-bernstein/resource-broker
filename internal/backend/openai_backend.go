package backend

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"sync"

	"github.com/preston-bernstein/resource-broker/internal/config"
	"github.com/preston-bernstein/resource-broker/internal/openaicompat"
	"github.com/preston-bernstein/resource-broker/internal/proxy"
	"github.com/preston-bernstein/resource-broker/internal/yield"
)

// openaiBackend is the Backend implementation for an OpenAI-compatible
// upstream server (e.g. vLLM): a hand-written translating handler for the
// Synchronous path plus an *openaicompat.Client for the durable Job path.
// It has no VRAM control of its own (see Unloader below).
type openaiBackend struct {
	proxy       http.Handler
	c           *openaicompat.Client
	upstreamURL *url.URL
	apiKey      string
	http        *http.Client
	unloader    yield.Unloader
}

// newOpenAIBackend constructs the "openai" backend.
func newOpenAIBackend(cfg *config.Config) (Backend, error) {
	// unloader stays the true nil yield.Unloader zero value when no unit is
	// configured. When cfg.UpstreamUnitName is set, it MUST be built via
	// newSystemdUnitController (never a bare &systemdUnitController{} struct
	// literal), since only the constructor wires the run closure — see
	// systemdUnitController's doc comment and Unloader below.
	var unloader yield.Unloader
	if cfg.UpstreamUnitName != "" {
		unloader = newSystemdUnitController(cfg.UpstreamUnitName)
	}
	return &openaiBackend{
		proxy:       openaicompat.NewHandler(cfg.UpstreamURL, cfg.UpstreamAPIKey),
		c:           openaicompat.New(cfg.UpstreamURL, cfg.UpstreamAPIKey),
		upstreamURL: cfg.UpstreamURL,
		apiKey:      cfg.UpstreamAPIKey,
		// Reuse internal/proxy's connection-retry transport, same as
		// openaicompat.Client and openaicompat.NewHandler do internally, for
		// the same reasons documented there (Design decision #5).
		http:     &http.Client{Transport: proxy.Transport},
		unloader: unloader,
	}, nil
}

// Proxy returns the wrapped OpenAI-compatible translating handler.
func (b *openaiBackend) Proxy() http.Handler {
	return b.proxy
}

// Generate bridges the openaicompat client's streaming Generate to the Job
// worker's Generator interface, mirroring ollamaBackend.Generate.
func (b *openaiBackend) Generate(ctx context.Context, model, prompt string, options map[string]any, onTokens func(int)) (string, error) {
	out, _, err := b.c.Generate(ctx, openaicompat.GenerateRequest{Model: model, Prompt: prompt, Options: options}, onTokens)
	return out, err
}

// Reachable probes the upstream with GET {UPSTREAM_URL}/v1/models, the
// standard OpenAI-compatible model-listing endpoint, returning an error if
// the request fails or the upstream responds with a non-2xx status. Uses
// proper URL joining (url.JoinPath), never plain string concatenation,
// mirroring the pattern set by openaicompat.Client's own url() helper.
func (b *openaiBackend) Reachable(ctx context.Context) error {
	target := b.upstreamURL.JoinPath("v1", "models").String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	if b.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.apiKey)
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("openai upstream: status %d", resp.StatusCode)
	}
	return nil
}

// Unloader returns b.unloader: nil (the true interface zero value) when this
// backend was constructed with no UPSTREAM_UNIT_NAME configured, preserving
// today's behavior of no VRAM control; otherwise a real, fully-wired
// *systemdUnitController driving the configured systemd unit's stop/start
// lifecycle. See docs/adr/0014-vllm-yield-symmetric-stop-start.md.
//
// Whatever this method returns MUST never be a typed-nil-wrapped pointer —
// yield.go's nil-guard (`if c.unloader != nil { go c.doUnload() }`) only
// catches a true nil *interface* value; a typed nil (a nil pointer of some
// concrete type boxed into the interface) would pass that check and then
// panic on a nil-receiver call to Unload inside an unrecovered goroutine,
// crashing the whole broker process at the exact moment real gaming/Plex
// contention is detected. b.unloader is itself interface-typed and is only
// ever assigned either the true nil zero-value or a genuinely non-nil
// *systemdUnitController built by newSystemdUnitController (see
// newOpenAIBackend), so this method naturally satisfies that requirement
// without any manual nil check here.
func (b *openaiBackend) Unloader() yield.Unloader { return b.unloader }

// systemdUnitController drives a single systemd unit's stop/start lifecycle
// for a vLLM (or other OpenAI-compatible upstream) process managed as a
// systemd service, implementing yield.Unloader's symmetric Unload/Reload
// pair. See docs/adr/0014-vllm-yield-symmetric-stop-start.md.
//
// The command-runner seam (run) is deliberately narrow: it only ever takes
// "stop" or "start" as verb, always against the one unit baked into this
// controller at construction time — never a generic "run any command with
// any args" primitive. mu ensures Unload and Reload on the same instance can
// never run their systemctl commands concurrently (no in-flight overlap),
// but mu alone does NOT guarantee they run in the order the yield/clear
// transitions happened — two independent `go` spawns racing for mu could
// still let a later Reload's "start" acquire the lock before an earlier
// Unload's "stop" does. Ordering across a fast yield-clear/yield-start flap
// is instead enforced one layer up, by yield.Controller's actionDone
// handoff chain (see yield.go), which serializes doUnload/doReload calls
// into this controller in transition order before they ever reach mu.
type systemdUnitController struct {
	unit string
	run  func(ctx context.Context, verb string) error
	mu   sync.Mutex
}

// newSystemdUnitController constructs a systemdUnitController for unit,
// whose run closure invokes the real `systemctl <verb> <unit>` command.
func newSystemdUnitController(unit string) *systemdUnitController {
	return &systemdUnitController{
		unit: unit,
		run: func(ctx context.Context, verb string) error {
			return exec.CommandContext(ctx, "systemctl", verb, unit).Run()
		},
	}
}

// Unload stops the configured systemd unit.
func (u *systemdUnitController) Unload(ctx context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := u.run(ctx, "stop"); err != nil {
		return fmt.Errorf("systemctl stop %s: %w", u.unit, err)
	}
	return nil
}

// Reload starts the configured systemd unit.
func (u *systemdUnitController) Reload(ctx context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := u.run(ctx, "start"); err != nil {
		return fmt.Errorf("systemctl start %s: %w", u.unit, err)
	}
	return nil
}
