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
	"strings"
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

// audioSessionElement is the element name Plex uses for a music session.
// Verified against a real /status/sessions capture from the desktop server
// on 2026-09-22; the fixture is in plex_audio_test.go.
const audioSessionElement = "Track"

// session is one entry in the container. Only the element name is read --
// it is what distinguishes a music session (<Track>) from everything else
// (<Video>, <Photo>, ...).
type session struct {
	XMLName xml.Name
}

// mediaContainer mirrors just enough of Plex's /status/sessions XML shape:
// size is the count of active sessions (0 when nothing is playing), and
// Sessions collects whatever child elements the container listed.
type mediaContainer struct {
	Size     int       `xml:"size,attr"`
	Sessions []session `xml:",any"`
}

// ActiveSession reports whether Plex currently has playback that can
// contend for the GPU. False positives from background maintenance never
// appear here — Plex scopes this endpoint to real "Now Playing" activity.
//
// Audio-only playback is excluded. CONTEXT.md defines Contention as "gaming,
// or Plex video transcoding"; music decoding never touches the GPU. Counting
// it cost real work: on 2026-09-22 every Yield in a 48h window was
// reason="plex" while the GPU sat at 3%, because someone was listening to
// music and Plex happened to be running its "Plex Transcoder" binary for
// background audio work. Each Yield ended in-flight embed calls and unloaded
// VRAM, which had stalled algo-corpus ingest since 2026-07-27.
//
// The exclusion is deliberately narrow and fails toward reporting Contention.
// Only a container whose sessions are ALL <Track>, and which listed as many
// sessions as it declared, is treated as GPU-free. A video or photo session, a
// mixed container, an element this code does not recognise, or a count that
// does not match the listing all report Contention. The yield-to-gaming
// invariant (ADR-0003, ADR-0004) is not weakened: a missed Yield can stutter
// someone's game, a spurious one only delays batch work, so every
// classification error resolves toward Yielding.
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
	if mc.Size <= 0 {
		return false, nil
	}
	// A container that did not list every session it declared cannot be
	// judged; fail toward Contention.
	if len(mc.Sessions) != mc.Size {
		return true, nil
	}
	for _, s := range mc.Sessions {
		if !strings.EqualFold(s.XMLName.Local, audioSessionElement) {
			return true, nil
		}
	}
	return false, nil
}
