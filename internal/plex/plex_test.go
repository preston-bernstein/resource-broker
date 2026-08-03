package plex

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestActiveSessionTrue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Plex-Token"); got != "tok" {
			t.Errorf("token header = %q, want %q", got, "tok")
		}
		w.Write([]byte(`<MediaContainer size="1"></MediaContainer>`))
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	active, err := c.ActiveSession()
	if err != nil {
		t.Fatalf("ActiveSession() err = %v", err)
	}
	if !active {
		t.Fatal("want active session")
	}
}

func TestActiveSessionFalseWhenEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<MediaContainer size="0"></MediaContainer>`))
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	active, err := c.ActiveSession()
	if err != nil {
		t.Fatalf("ActiveSession() err = %v", err)
	}
	if active {
		t.Fatal("want no active session on empty container — this is the background-maintenance case")
	}
}

func TestActiveSessionErrorsOnUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := New(srv.URL, "bad-token")
	if _, err := c.ActiveSession(); err == nil {
		t.Fatal("want error on 401")
	}
}
