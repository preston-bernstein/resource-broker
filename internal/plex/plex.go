// Package plex is a thin client for checking whether the local Plex Media
// Server has an active playback session.
//
// Plex runs its "Plex Transcoder" binary for background maintenance too —
// Skip Intro/Credits detection, chapter-thumbnail generation, loudness
// analysis — on its own server-scheduled cadence, completely independent of
// anyone actually watching something
// (https://support.plex.tv/articles/201697383-why-is-plex-using-my-cpu/,
// https://support.plex.tv/articles/credits-detection/). A process-name match
// on "Plex Transcoder" alone cannot tell the two apart. Plex's own
// /status/sessions endpoint is scoped to "Now Playing" only
// (https://support.plex.tv/articles/200871837-status-and-dashboard/), so
// querying it is the corroborating signal that fixes the false positive.
package plex

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"time"
)

// Client checks session state against a local Plex Media Server.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New returns a Client for the Plex server at baseURL (e.g.
// "http://localhost:32400"), authenticating with token.
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: 2 * time.Second},
	}
}

// mediaContainer mirrors just enough of Plex's /status/sessions XML shape:
// size is the count of active sessions (0 when nothing is playing).
type mediaContainer struct {
	Size int `xml:"size,attr"`
}

// ActiveSession reports whether Plex currently has at least one active
// playback session. False positives from background maintenance never
// appear here — Plex scopes this endpoint to real "Now Playing" activity.
func (c *Client) ActiveSession() (bool, error) {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/status/sessions", nil)
	if err != nil {
		return false, fmt.Errorf("plex: new request: %w", err)
	}
	req.Header.Set("X-Plex-Token", c.token)
	req.Header.Set("Accept", "application/xml")

	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("plex: status/sessions: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("plex: status/sessions: status %d", resp.StatusCode)
	}

	var mc mediaContainer
	if err := xml.NewDecoder(resp.Body).Decode(&mc); err != nil {
		return false, fmt.Errorf("plex: decode: %w", err)
	}
	return mc.Size > 0, nil
}
