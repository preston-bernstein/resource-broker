// Package detect identifies GPU contention from high-priority local processes
// (gaming, Plex transcoding) by inspecting process command lines. The match
// patterns are ported verbatim from the Bash V3 resource-manager so behaviour
// does not regress.
package detect

import (
	"os"
	"path/filepath"
	"regexp"
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

// Detector reports contention from a process Lister.
type Detector struct {
	list Lister
}

// New returns a Detector backed by list.
func New(list Lister) *Detector { return &Detector{list: list} }

// Detect returns a contention reason and true if any high-priority process is
// running. On a listing error it returns ("", false) — fail open, never block
// inference because we couldn't read /proc.
func (d *Detector) Detect() (string, bool) {
	procs, err := d.list()
	if err != nil {
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

// ProcLister reads /proc and returns running processes. Linux-only; on other
// platforms it returns no processes (detection effectively disabled).
func ProcLister() ([]Process, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, nil // not Linux / no procfs: behave as "no contention"
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
