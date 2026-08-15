// Package detect identifies GPU contention from high-priority local processes
// (gaming, Plex transcoding) by inspecting process command lines. The match
// patterns are ported verbatim from the Bash V3 resource-manager so behaviour
// does not regress.
//
// The "Plex Transcoder" process name alone is not a reliable signal: Plex
// runs that same binary for background maintenance (Skip Intro/Credits
// detection, chapter-thumbnail generation) on its own schedule, independent
// of anyone actually watching something
// (https://support.plex.tv/articles/201697383-why-is-plex-using-my-cpu/).
// When a PlexSessionChecker is configured, a process-name match is
// corroborated against Plex's own /status/sessions before being reported as
// contention — see package plex.
package detect

import (
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// Process is the minimal view of a running process the detector needs.
type Process struct {
	Comm    string // /proc/<pid>/comm
	Cmdline string // /proc/<pid>/cmdline, NULs replaced by spaces
}

// Lister returns the currently running processes.
type Lister func() ([]Process, error)

// rule maps a contention reason to a matcher over a process command line.
type rule struct {
	reason string
	match  func(cmd string) bool
}

var lutrisRe = regexp.MustCompile(`lutris.*runner`)
var heroicRe = regexp.MustCompile(`heroic.*game`)
var wineRe = regexp.MustCompile(`wine.*\.exe`)

// wineSystemDirRe matches Wine's own bootstrapped runtime executables
// (winedevice.exe, services.exe, plugplay.exe, etc.), which always run from
// the prefix's windows\system32 directory regardless of which application
// created the prefix. These start automatically with *any* Wine prefix —
// including non-game tools like the Norgate Data Updater — so wineRe alone
// false-positives on them; a game's own .exe always runs from the app's own
// install path, never from system32.
var wineSystemDirRe = regexp.MustCompile(`(?i)windows[\\/]system32`)

const plexReason = "plex"

// plexProcess matches Plex's transcoder binary. On its own this is not a
// reliable contention signal — see the PlexSessionChecker doc comment.
const plexProcess = "Plex Transcoder"

// gamingRules are evaluated in order; first match wins. Plex is checked
// separately (and first, preserving its historical priority) since it needs
// PlexSessionChecker corroboration, not a plain per-process match.
var gamingRules = []rule{
	{"gaming-steam", func(c string) bool { return strings.Contains(c, "SteamLaunch AppId=") }},
	{"gaming-lutris", func(c string) bool { return lutrisRe.MatchString(c) }},
	{"gaming-heroic", func(c string) bool { return heroicRe.MatchString(c) }},
	{"gaming-wine", func(c string) bool {
		return wineRe.MatchString(c) &&
			!strings.Contains(c, "protonmail") &&
			!strings.Contains(c, "protonvpn") &&
			!wineSystemDirRe.MatchString(c)
	}},
}

// ErrorRecorder tallies detection failures for observability (the
// broker_detect_errors_total counter). Optional; a nil ErrorRecorder just
// skips the metric — the WARN log line below still fires either way.
type ErrorRecorder interface {
	IncDetectError()
}

// PlexSessionChecker reports whether Plex currently has an active playback
// session, as opposed to background maintenance (Skip Intro/Credits
// detection, chapter-thumbnail generation) that runs the same "Plex
// Transcoder" binary independent of playback. See package
// github.com/preston-bernstein/ollama-resource-broker/internal/plex.
type PlexSessionChecker interface {
	ActiveSession() (bool, error)
}

// Detector reports contention from a process Lister.
type Detector struct {
	list        Lister
	errs        ErrorRecorder      // optional; nil = metric recording disabled
	plexChecker PlexSessionChecker // nil: plain process-match, no corroboration
}

// New returns a Detector backed by list.
func New(list Lister) *Detector { return &Detector{list: list} }

// SetErrorRecorder registers where Detect() reports listing failures. Safe to
// call before Detect() is first invoked; not safe to call concurrently with
// Detect() (matches the SetGPUManager/SetPlexChecker convention elsewhere in
// this repo — configure once at startup, then run).
func (d *Detector) SetErrorRecorder(r ErrorRecorder) { d.errs = r }

// SetPlexChecker enables Plex session corroboration: a "Plex Transcoder"
// process match is only reported as contention once checker confirms an
// active playback session. Call before the Detector starts polling.
func (d *Detector) SetPlexChecker(checker PlexSessionChecker) {
	d.plexChecker = checker
}

// Detect returns a contention reason and true if any high-priority process is
// running. On a listing error it returns ("", false) — fail OPEN, never block
// inference because we couldn't read /proc — but that fail-open is not
// allowed to be silent: a WARN log line and the broker_detect_errors_total
// counter both fire, because a Detector that can never report contention
// (e.g. a systemd hardening addition like ProtectProc=/hidepid= taking away
// /proc visibility from the ollama-broker user) is otherwise indistinguishable
// from an idle machine — see the 2026-08-01 fleet observability audit.
func (d *Detector) Detect() (string, bool) {
	procs, err := d.list()
	if err != nil {
		slog.Warn("detect: process list failed, failing open (reporting no contention)", "err", err)
		if d.errs != nil {
			d.errs.IncDetectError()
		}
		return "", false
	}

	if plexTranscoderRunning(procs) && d.plexIsRealSession() {
		return plexReason, true
	}

	for _, r := range gamingRules {
		for _, p := range procs {
			if r.match(p.Cmdline) {
				return r.reason, true
			}
		}
	}
	return "", false
}

// goos is runtime.GOOS, indirected so tests can simulate a non-Linux host
// without needing to actually run on one.
var goos = runtime.GOOS

func plexTranscoderRunning(procs []Process) bool {
	for _, p := range procs {
		if strings.Contains(p.Cmdline, plexProcess) {
			return true
		}
	}
	return false
}

// plexIsRealSession corroborates a "Plex Transcoder" process match against
// Plex's own session API. No checker configured: process match alone is
// treated as contention (unchanged legacy behavior). Checker error: fail
// SAFE toward yielding — an unreachable Plex API must never silently hide
// real contention, unlike the /proc listing error above (which fails open
// because /proc reads essentially never fail on Linux; a network call can).
func (d *Detector) plexIsRealSession() bool {
	if d.plexChecker == nil {
		return true
	}
	active, err := d.plexChecker.ActiveSession()
	if err != nil {
		return true
	}
	return active
}

// ProcLister reads /proc and returns running processes. Linux-only; on other
// platforms it returns (nil, nil) — detection is disabled by design there,
// which is NOT the failure Detect() needs to hear about. A real read failure
// on Linux (e.g. /proc made unreadable by a hardening change) is a different
// story and is returned as an error so Detect() logs and counts it instead of
// silently behaving like an idle machine.
func ProcLister() ([]Process, error) {
	if goos != "linux" {
		return nil, nil // not Linux / no procfs: detection intentionally disabled
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err // real failure on a platform where /proc should exist
	}
	var procs []Process
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue // not a pid dir
		}
		base := filepath.Join("/proc", e.Name())
		cmd, err := os.ReadFile(filepath.Join(base, "cmdline"))
		if err != nil {
			continue // process gone or unreadable
		}
		comm, _ := os.ReadFile(filepath.Join(base, "comm"))
		procs = append(procs, Process{
			Comm:    strings.TrimSpace(string(comm)),
			Cmdline: strings.ReplaceAll(string(cmd), "\x00", " "),
		})
	}
	return procs, nil
}
