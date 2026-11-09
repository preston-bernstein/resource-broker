package httpx

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestRequestModelRestoresBody(t *testing.T) {
	cases := map[string]string{
		`{"model":"bge-m3-cpu","input":["a"]}`: "bge-m3-cpu",
		`{"input":["a"],"model":"qwen2.5"}`:    "qwen2.5",
		`{"input":["a"]}`:                      "",
		`not json`:                             "",
	}
	for body, want := range cases {
		req, _ := http.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		if got := RequestModel(req); got != want {
			t.Errorf("RequestModel(%q) = %q, want %q", body, got, want)
		}
		rest, _ := io.ReadAll(req.Body)
		if string(rest) != body {
			t.Errorf("body after peek = %q, want it restored to %q", rest, body)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	if got := RequestModel(req); got != "" {
		t.Errorf("RequestModel(no body) = %q, want empty", got)
	}
}
