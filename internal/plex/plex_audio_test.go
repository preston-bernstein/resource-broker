package plex

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// realAudioSessionXML is a REAL capture from the desktop's Plex server
// (2026-09-22, during the incident), truncated after the first element's
// attributes and closed by hand. Identifiers are left as captured; no token
// appears in this endpoint's body.
//
// It is here because the shape is the whole point: a music session is a
// <Track> child of <MediaContainer>, and nothing in it is a video stream.
// Writing this fixture from memory instead of from the wire is how a stub
// ends up confirming its author's assumption rather than the server's
// behavior.
const realAudioSessionXML = `<?xml version="1.0" encoding="UTF-8"?>
<MediaContainer size="1">
<Track addedAt="1761570422" duration="274944" grandparentKey="/library/metadata/22812" grandparentRatingKey="22812" grandparentTitle="The Pop Group" index="1" key="/library/metadata/22813" type="track"></Track>
</MediaContainer>`

func serving(t *testing.T, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "tok")
}

// TestActiveSessionFalseForAudioOnlyPlayback is the regression.
//
// Someone listening to music made the Broker report Contention and Yield the
// GPU: `internal/detect` requires a "Plex Transcoder" process match AND this
// corroboration, and Plex runs that same binary for background audio work
// (loudness analysis). Both signals went true while the GPU sat at 3%, so the
// Broker cancelled LightRAG's in-flight embed calls and unloaded VRAM. 86 of
// 86 Yields in one 48h window were reason="plex"; every one of the 151
// preempted embed calls fell inside a Yield window. algo-corpus ingest had
// been stalled behind it since 2026-07-27.
//
// Audio playback cannot contend for the GPU. It must not trigger a Yield.
func TestActiveSessionFalseForAudioOnlyPlayback(t *testing.T) {
	c := serving(t, realAudioSessionXML)

	active, err := c.ActiveSession()
	if err != nil {
		t.Fatalf("ActiveSession() err = %v", err)
	}
	if active {
		t.Fatal("audio-only playback reported as Contention; a music stream never uses the GPU, " +
			"and treating it as Contention is what stalled algo-corpus ingest for weeks")
	}
}

// TestActiveSessionTrueForNonAudio pins the fail-safe direction: only a
// session proven to be audio-only suppresses Contention. Anything else --
// video, an element this code has never seen, or a container that declares
// sessions without listing them -- reports Contention.
//
// The asymmetry is deliberate and is the yield-to-gaming invariant
// (ADR-0003/ADR-0004): a missed Yield can stutter playback, a spurious one
// only costs batch throughput. Every classifier error fails toward Yielding.
func TestActiveSessionTrueForNonAudio(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "video session",
			body: `<MediaContainer size="1"><Video type="episode" title="x"></Video></MediaContainer>`,
		},
		{
			name: "photo session",
			body: `<MediaContainer size="1"><Photo type="photo"></Photo></MediaContainer>`,
		},
		{
			name: "element this code has never seen",
			body: `<MediaContainer size="1"><Hologram type="future"></Hologram></MediaContainer>`,
		},
		{
			name: "sessions declared but not listed",
			body: `<MediaContainer size="1"></MediaContainer>`,
		},
		{
			name: "music playing while something also streams video",
			body: `<MediaContainer size="2"><Track type="track"></Track><Video type="movie"></Video></MediaContainer>`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := serving(t, tc.body)

			active, err := c.ActiveSession()
			if err != nil {
				t.Fatalf("ActiveSession() err = %v", err)
			}
			if !active {
				t.Fatal("want Contention: only a session proven audio-only may suppress a Yield")
			}
		})
	}
}

// TestActiveSessionFalseForMultipleAudioSessions: two people listening to
// music is still nobody using the GPU.
func TestActiveSessionFalseForMultipleAudioSessions(t *testing.T) {
	c := serving(t, `<MediaContainer size="2"><Track type="track"></Track><Track type="track"></Track></MediaContainer>`)

	active, err := c.ActiveSession()
	if err != nil {
		t.Fatalf("ActiveSession() err = %v", err)
	}
	if active {
		t.Fatal("two audio sessions are still zero GPU demand")
	}
}
