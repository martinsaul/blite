package audio

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// The streams are logged when they change; a selection matching none warns
// on change and then at most once a minute.
func TestShareLog(t *testing.T) {
	var buf bytes.Buffer
	l := shareLog{log: slog.New(slog.NewTextHandler(&buf, nil))}
	t0 := time.Now()
	ff := []stream{{name: "Firefox", pid: 42, links: 2}}
	apps := Selection{Mode: "apps", Apps: []string{"Firefox#41"}}
	playing := []string{"Firefox#42"}
	for i, c := range []struct {
		sel     Selection
		matched []stream
		at      time.Duration
		want    string // "" = nothing logged
	}{
		{Selection{Mode: "none"}, nil, 0, ""},
		{Selection{Mode: "system"}, ff, 0, `level=INFO msg="shared audio streams" mode=system streams="Firefox pid=42 links=2"`},
		{Selection{Mode: "system"}, ff, 2 * time.Second, ""},
		{Selection{Mode: "system"}, []stream{{name: "Firefox", pid: 42, links: 1}}, 4 * time.Second, `streams="Firefox pid=42 links=1"`},
		{apps, nil, 6 * time.Second, `level=WARN msg="shared audio matches no app stream" mode=apps apps=[Firefox#41] pid=0 playing=[Firefox#42]`},
		{apps, nil, 30 * time.Second, ""},
		{apps, nil, 66 * time.Second, "level=WARN"},
		{apps, nil, 68 * time.Second, ""},
		{Selection{Mode: "window", PID: 42}, ff, 70 * time.Second, `mode=window streams="Firefox pid=42 links=2"`},
		{Selection{Mode: "window", PID: 42}, nil, 72 * time.Second, "level=WARN"},
		{Selection{Mode: "system"}, nil, 74 * time.Second, `level=INFO msg="shared audio streams" mode=system streams=""`},
		{Selection{Mode: "none"}, nil, 76 * time.Second, ""},
	} {
		buf.Reset()
		l.note(c.sel, c.matched, playing, t0.Add(c.at))
		got := buf.String()
		if c.want == "" && got != "" || !strings.Contains(got, c.want) || strings.Count(got, "\n") > 1 {
			t.Errorf("%d: logged %q, want %q", i, got, c.want)
		}
	}
}

func TestIsVoiceApp(t *testing.T) {
	for _, n := range []string{"Discord.exe", "discord", "WEBRTC VoiceEngine", "ts3client_win64.exe", "Zoom.exe", "ms-teams.exe"} {
		if !isVoiceApp(n) {
			t.Errorf("%q: want a voice chat app", n)
		}
	}
	for _, n := range []string{"RSDragonwilds-Win64-Shipping.exe", "firefox", "chrome.exe", "Spotify.exe", ""} {
		if isVoiceApp(n) {
			t.Errorf("%q: not a voice chat app", n)
		}
	}
	if !isVoiceApp("", "Discord") {
		t.Error("any of the names counts")
	}
}
