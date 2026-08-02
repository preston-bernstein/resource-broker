// Package detect identifies GPU contention from high-priority local processes
// (gaming, Plex transcoding) by inspecting process command lines. The match
// patterns are ported verbatim from the Bash V3 resource-manager so behaviour
// does not regress.
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

// rules are evaluated in order; first match wins (Plex highest priority),
// mirroring resource-manager-v3.sh detect_resource_contention().
var rules = []rule{
	{"plex", func(c string) bool { return strings.Contains(c, "Plex Transcoder") }},
	{"gaming-steam", func(c string) bool { return strings.Contains(c, "SteamLaunch AppId=") }},
	{"gaming-lutris", func(c string) bool { return lutrisRe.MatchString(c) }},
	{"gaming-heroic", func(c string) bool { return heroicRe.MatchString(c) }},
	{"gaming-wine", func(c string) bool {
		return wineRe.MatchString(c) &&
			!strings.Contains(c, "protonmail") &&
			!strings.Contains(c, "protonvpn")
	}},
}

// ErrorRecorder tallies detection failures for observability (the
// broker_detect_errors_total counter). Optional; a nil ErrorRecorder just
// skips the metric — the WARN log line below still fires either way.
type ErrorRecorder interface {
	IncDetectError()
}

// Detector reports contention from a process Lister.
type Detector struct {
	list Lister
	errs ErrorRecorder // optional; nil = metric recording disabled
}

// New returns a Detector backed by list.
func New(list Lister) *Detector { return &Detector{list: list} }

// SetErrorRecorder registers where Detect() reports listing failures. Safe to
// call before Detect() is first invoked; not safe to call concurrently with
// Detect() (matches the SetGPUManager/SetPlexChecker convention elsewhere in
// this repo — configure once at startup, then run).
func (d *Detector) SetErrorRecorder(r ErrorRecorder) { d.errs = r }

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
	for _, r := range rules {
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
