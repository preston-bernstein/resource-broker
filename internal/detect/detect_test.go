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

var errReadProc = errTest("read /proc failed")

type errTest string

func (e errTest) Error() string { return string(e) }
