package detect

import "testing"

func lister(cmds ...string) Lister {
	return func() ([]Process, error) {
		var ps []Process
		for _, c := range cmds {
			ps = append(ps, Process{Cmdline: c})
		}
		return ps, nil
	}
}

func TestDetect(t *testing.T) {
	cases := []struct {
		name       string
		cmds       []string
		wantReason string
		wantCont   bool
	}{
		{"idle", []string{"/usr/bin/firefox", "ollama serve"}, "", false},
		{"plex", []string{"/x/Plex Transcoder -i foo"}, "plex", true},
		{"steam", []string{"reaper SteamLaunch AppId=440 -- game"}, "gaming-steam", true},
		{"lutris", []string{"python3 lutris wine-ge runner foo"}, "gaming-lutris", true},
		{"heroic", []string{"heroic --no-sandbox game launch"}, "gaming-heroic", true},
		{"wine", []string{"C:/wine/Game.exe"}, "gaming-wine", true},
		{"protonmail-excluded", []string{"wine protonmail-bridge.exe"}, "", false},
		{"protonvpn-excluded", []string{"wine protonvpn.exe"}, "", false},
		{"plex-beats-steam", []string{"reaper SteamLaunch AppId=1", "Plex Transcoder x"}, "plex", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := New(lister(c.cmds...))
			reason, cont := d.Detect()
			if cont != c.wantCont || reason != c.wantReason {
				t.Fatalf("Detect() = (%q,%v), want (%q,%v)", reason, cont, c.wantReason, c.wantCont)
			}
		})
	}
}

func TestDetectListerErrorFailsOpen(t *testing.T) {
	d := New(func() ([]Process, error) { return nil, errReadProc })
	if reason, cont := d.Detect(); cont || reason != "" {
		t.Fatalf("on lister error want no contention, got (%q,%v)", reason, cont)
	}
}

// TestDetectListerErrorRecordsMetric pins the fix for the 2026-08-01 audit
// finding: the fail-open on a listing error (proven above) must not be
// silent. This does not change the policy (still fails open) — it only
// requires the failure to register on the ErrorRecorder.
func TestDetectListerErrorRecordsMetric(t *testing.T) {
	rec := &fakeErrorRecorder{}
	d := New(func() ([]Process, error) { return nil, errReadProc })
	d.SetErrorRecorder(rec)

	if reason, cont := d.Detect(); cont || reason != "" {
		t.Fatalf("on lister error want no contention, got (%q,%v)", reason, cont)
	}
	if rec.n != 1 {
		t.Fatalf("IncDetectError calls = %d, want 1", rec.n)
	}

	// A second failure must record again — this is a per-poll signal, not a
	// one-shot latch.
	d.Detect()
	if rec.n != 2 {
		t.Fatalf("IncDetectError calls after second failure = %d, want 2", rec.n)
	}
}

// TestDetectNoErrorRecorderDoesNotPanic proves the nil ErrorRecorder default
// (no SetErrorRecorder call, as in every other test in this file) is safe.
func TestDetectNoErrorRecorderDoesNotPanic(t *testing.T) {
	d := New(func() ([]Process, error) { return nil, errReadProc })
	d.Detect() // must not panic with errs == nil
}

// TestProcListerNonLinuxIsNotAnError pins that "wrong OS" and "real /proc
// read failure" are deliberately distinguishable: off Linux, ProcLister
// returns (nil, nil) — detection is disabled by design, not broken — so it
// must never reach Detect()'s error-logging/metric path.
func TestProcListerNonLinuxIsNotAnError(t *testing.T) {
	old := goos
	goos = "darwin"
	defer func() { goos = old }()

	procs, err := ProcLister()
	if err != nil {
		t.Fatalf("ProcLister on non-Linux: err = %v, want nil", err)
	}
	if procs != nil {
		t.Fatalf("ProcLister on non-Linux: procs = %v, want nil", procs)
	}
}

type fakeErrorRecorder struct{ n int }

func (f *fakeErrorRecorder) IncDetectError() { f.n++ }

var errReadProc = errTest("read /proc failed")

type errTest string

func (e errTest) Error() string { return string(e) }
