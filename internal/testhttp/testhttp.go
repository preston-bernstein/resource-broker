// Package testhttp holds the HTTP client calls the broker's tests share: the
// same shape as http.Get and http.Post, but every request carries a context,
// as the noctx linter requires.
package testhttp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
)

// Get is http.Get with a background context.
func Get(url string) (*http.Response, error) {
	return Do(context.Background(), http.MethodGet, url, nil)
}

// Post is http.Post with a background context.
func Post(url, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, body)
	if err != nil {
		return nil, fmt.Errorf("build POST %s: %w", url, err)
	}
	req.Header.Set("Content-Type", contentType)
	return send(req)
}

// Do sends one request on http.DefaultClient.
func Do(ctx context.Context, method, url string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("build %s %s: %w", method, url, err)
	}
	return send(req)
}

func send(req *http.Request) (*http.Response, error) {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", req.Method, req.URL, err)
	}
	return resp, nil
}

// WithPost sends one POST and hands the open response to use, so a test can
// read a streamed body as it arrives. The body closes when use returns.
func WithPost(ctx context.Context, url string, body io.Reader, use func(*http.Response)) error {
	resp, err := Do(ctx, http.MethodPost, url, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	use(resp)
	return nil
}

// Reply is a response read to its end, with the body closed. Its Body reads
// from memory, so tests can treat it like an http.Response, and Trailer is
// filled in.
type Reply struct {
	StatusCode int
	Header     http.Header
	Trailer    http.Header
	Body       *bytes.Reader
}

// GetReply is Get, read to the end.
func GetReply(url string) (Reply, error) {
	return readReply(http.MethodGet, url, "", nil)
}

// PostReply is Post, read to the end.
func PostReply(url, contentType string, body io.Reader) (Reply, error) {
	return readReply(http.MethodPost, url, contentType, body)
}

func readReply(method, url, contentType string, body io.Reader) (Reply, error) {
	req, err := http.NewRequestWithContext(context.Background(), method, url, body)
	if err != nil {
		return Reply{}, fmt.Errorf("build %s %s: %w", method, url, err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Reply{}, fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Reply{}, fmt.Errorf("read %s %s: %w", method, url, err)
	}
	return Reply{StatusCode: resp.StatusCode, Header: resp.Header, Trailer: resp.Trailer, Body: bytes.NewReader(raw)}, nil
}
