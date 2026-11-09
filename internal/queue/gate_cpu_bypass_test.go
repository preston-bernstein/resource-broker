package queue

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A model configured as CPU-only (BROKER_CPU_MODELS) never touches the GPU,
// so it must not wait on the single GPU slot. Found live 2026-09-29:
// lightrag-trading's bge-m3-cpu embeds held the batch slot for 80-180s each
// while the GPU sat at 0% busy, and every interactive request behind them
// got "GPU busy: wait budget exceeded".

// holdSlot occupies s's only slot through srv and waits until it is held.
func holdSlot(t *testing.T, s *Scheduler, srv *httptest.Server) {
	t.Helper()
	go func() {
		resp, err := http.Post(srv.URL+"/hold", "application/json", strings.NewReader(`{"model":"qwen3.6:35b-a3b"}`))
		if err == nil {
			resp.Body.Close()
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for !s.Stats().Busy && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !s.Stats().Busy {
		t.Fatal("holder never acquired the slot")
	}
}

func cpuBypassUpstream(release <-chan struct{}, gotBody *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hold" {
			<-release
			return
		}
		b, _ := io.ReadAll(r.Body)
		*gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	})
}

func TestGateCPUModelBypassesHeldSlot(t *testing.T) {
	release := make(chan struct{})
	var gotBody string

	s := New()
	s.SetCPUModels([]string{"bge-m3-cpu"})
	srv := httptest.NewServer(s.Gate(Batch, 100*time.Millisecond, 0, alwaysServe{}, nil, cpuBypassUpstream(release, &gotBody)))
	// Release the holder before Close, which waits for in-flight handlers.
	defer func() { close(release); srv.Close() }()
	holdSlot(t, s, srv)

	// ":latest" is how Ollama names the same model, so it must match too.
	for _, model := range []string{"bge-m3-cpu", "bge-m3-cpu:latest"} {
		body := `{"model":"` + model + `","input":["hello"]}`
		resp, err := http.Post(srv.URL+"/api/embed", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("%s: post: %v", model, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200 (CPU model must not wait on the GPU slot)", model, resp.StatusCode)
		}
		if gotBody != body {
			t.Fatalf("%s: upstream body = %q, want the client's body unchanged %q", model, gotBody, body)
		}
	}
}

func TestGateGPUModelStillWaitsOnHeldSlot(t *testing.T) {
	release := make(chan struct{})
	var gotBody string

	s := New()
	s.SetCPUModels([]string{"bge-m3-cpu"})
	srv := httptest.NewServer(s.Gate(Batch, 100*time.Millisecond, 0, alwaysServe{}, nil, cpuBypassUpstream(release, &gotBody)))
	// Release the holder before Close, which waits for in-flight handlers.
	defer func() { close(release); srv.Close() }()
	holdSlot(t, s, srv)

	resp, err := http.Post(srv.URL+"/api/embed", "application/json", strings.NewReader(`{"model":"bge-m3","input":["hello"]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (a GPU model must still wait for the slot)", resp.StatusCode)
	}
}

func TestGateCPUModelServedWhileYielding(t *testing.T) {
	var gotBody string
	s := New()
	s.SetCPUModels([]string{"bge-m3-cpu"})
	srv := httptest.NewServer(s.Gate(Interactive, 100*time.Millisecond, 0, alwaysYield{}, nil, cpuBypassUpstream(nil, &gotBody)))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/embed", "application/json", strings.NewReader(`{"model":"bge-m3-cpu","input":["x"]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (gaming/Plex contention is GPU-only; a CPU model is unaffected)", resp.StatusCode)
	}
}
