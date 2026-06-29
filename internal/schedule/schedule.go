// Package schedule encodes the known weekly compute calendar for this host.
//
// The broker uses this to coordinate background GPU workloads (Tdarr) around
// predictable high-load windows so they don't fight over the GPU blind.
//
// Edit the windows slice to update the schedule; no other code changes needed.
package schedule

import (
	"time"
)

// Window is a named recurring time slot when a known compute workload runs.
type Window struct {
	Name        string
	Weekday     *time.Weekday // nil = every day
	StartHour   int
	StartMinute int
	DurationH   float64 // hours; may be fractional
	Description string
}

// windows is the authoritative host compute schedule.
//
// internal-scraper-service: cron "0 2 * * 5" — runs npm scan against estatesales.net,
// heavy Ollama vision inference through the broker. Observed to run ~02:00–06:00
// on Fridays (the "4am thing" is just the scraper still going).
//
// safe-batch: a soft label for the lights-out window on any night; used by
// external tooling to know when background GPU work is least likely to conflict.
var windows = []Window{
	{
		Name:        "internal-scraper-service",
		Weekday:     weekdayPtr(time.Friday),
		StartHour:   2,
		StartMinute: 0,
		DurationH:   5, // 02:00–07:00 with margin
		Description: "estatesales.net Ollama vision scan (GPU via broker, heavy batch)",
	},
	{
		Name:        "safe-batch",
		Weekday:     nil, // every day
		StartHour:   2,
		StartMinute: 0,
		DurationH:   7, // 02:00–09:00
		Description: "preferred window for background GPU batch work (low inference demand)",
	},
}

func weekdayPtr(d time.Weekday) *time.Weekday { return &d }

// Active returns all windows whose scheduled slot contains t (evaluated in
// the local timezone).
func Active(t time.Time) []Window {
	t = t.Local()
	var out []Window
	for _, w := range windows {
		if contains(w, t) {
			out = append(out, w)
		}
	}
	return out
}

// InWindow reports whether the named window is active at time t.
func InWindow(name string, t time.Time) bool {
	for _, w := range Active(t) {
		if w.Name == name {
			return true
		}
	}
	return false
}

// SafeForBackgroundGPU reports whether background GPU transcoding (Tdarr) can
// run at time t without conflicting with a scheduled high-priority consumer.
// It returns false during the internal-scraper-service window, when the GPU is needed
// for heavy Ollama inference.
func SafeForBackgroundGPU(t time.Time) bool {
	return !InWindow("internal-scraper-service", t)
}

// Snapshot is the serialisable view exposed in /status.
type Snapshot struct {
	ActiveWindows []WindowInfo `json:"active_windows"`
	SafeForTdarr  bool         `json:"safe_for_tdarr"`
}

// WindowInfo is the JSON-safe form of a Window.
type WindowInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// TakeSnapshot returns the current schedule state for /status.
func TakeSnapshot(t time.Time) Snapshot {
	active := Active(t)
	infos := make([]WindowInfo, len(active))
	for i, w := range active {
		infos[i] = WindowInfo{Name: w.Name, Description: w.Description}
	}
	return Snapshot{
		ActiveWindows: infos,
		SafeForTdarr:  SafeForBackgroundGPU(t),
	}
}

// contains reports whether w's recurring slot contains t (t already in local time).
func contains(w Window, t time.Time) bool {
	if w.Weekday != nil && t.Weekday() != *w.Weekday {
		return false
	}
	startSec := w.StartHour*3600 + w.StartMinute*60
	endSec := startSec + int(w.DurationH*3600)
	tod := t.Hour()*3600 + t.Minute()*60 + t.Second()
	return tod >= startSec && tod < endSec
}
