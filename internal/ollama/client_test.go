package ollama

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"testing"
)

func TestLoadedModelsAndUnload(t *testing.T) {
	var unloaded []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ps":
			io.WriteString(w, `{"models":[{"name":"a:latest"},{"name":"b:7b"}]}`)
		case "/api/generate":
			var body struct {
				Model     string `json:"model"`
				KeepAlive int    `json:"keep_alive"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.KeepAlive != 0 {
				t.Errorf("keep_alive = %d, want 0", body.KeepAlive)
			}
			unloaded = append(unloaded, body.Model)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	base, _ := url.Parse(srv.URL)
	c := New(base)

	names, err := c.LoadedModels(context.Background())
	if err != nil {
		t.Fatalf("LoadedModels: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("names = %v", names)
	}

	if err := c.Unload(context.Background()); err != nil {
		t.Fatalf("Unload: %v", err)
	}
	sort.Strings(unloaded)
	if len(unloaded) != 2 || unloaded[0] != "a:latest" || unloaded[1] != "b:7b" {
		t.Fatalf("unloaded = %v, want [a:latest b:7b]", unloaded)
	}
}

func TestUnloadNoModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"models":[]}`)
	}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	if err := New(base).Unload(context.Background()); err != nil {
		t.Fatalf("Unload with no models: %v", err)
	}
}
