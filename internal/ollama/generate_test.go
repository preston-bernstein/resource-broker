package ollama

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestGenerateStreamsAndAccumulates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		fl, _ := w.(http.Flusher)
		for _, chunk := range []string{
			`{"response":"Hello","done":false}`,
			`{"response":", ","done":false}`,
			`{"response":"world","done":false}`,
			`{"response":"","done":true,"eval_count":3}`,
		} {
			io.WriteString(w, chunk+"\n")
			if fl != nil {
				fl.Flush()
			}
		}
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	c := New(u)

	var ticks []int
	out, tokens, err := c.Generate(context.Background(),
		GenerateRequest{Model: "m", Prompt: "hi"},
		func(n int) { ticks = append(ticks, n) })
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if out != "Hello, world" {
		t.Fatalf("output = %q", out)
	}
	if tokens != 3 { // final eval_count wins
		t.Fatalf("tokens = %d, want 3", tokens)
	}
	if len(ticks) != 3 {
		t.Fatalf("progress ticks = %v, want 3", ticks)
	}
}

func TestGenerateUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"error":"model not found"}`+"\n")
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := New(u)

	_, _, err := c.Generate(context.Background(), GenerateRequest{Model: "ghost"}, nil)
	if err == nil || !strings.Contains(err.Error(), "model not found") {
		t.Fatalf("err = %v, want model-not-found", err)
	}
}

func TestGenerateContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, _ := w.(http.Flusher)
		io.WriteString(w, `{"response":"partial","done":false}`+"\n")
		if fl != nil {
			fl.Flush()
		}
		<-r.Context().Done() // never sends done
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := New(u)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { cancel() }()
	_, _, err := c.Generate(ctx, GenerateRequest{Model: "m"}, nil)
	if err == nil {
		t.Fatal("expected cancellation error")
	}
}
