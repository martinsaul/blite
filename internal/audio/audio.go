// Package audio picks what a Blite user shares, OBS-style: one or more apps, or
// every app except Blite ("system"), and which output device plays the call.
//
// Linux (PipeWire): WirePlumber won't link a capture stream to another app's
// output stream, so Blite creates a virtual sink "blite-share", links the chosen
// apps' output ports into it IN ADDITION to their normal outputs (the user keeps
// hearing them), and captures that sink's monitor. Verified 2026-10-24 with two
// tone generators: only the linked app's tone reaches the capture.
package audio

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
)

// App is a program currently playing audio.
type App struct {
	Key  string `json:"key"`  // stable across stream restarts: name + pid
	Name string `json:"name"` // application.name
	PID  int    `json:"pid,omitempty"`
}

// Device is an output device that can play the call.
type Device struct {
	ID   string `json:"id"`   // node.name (Linux) / device id (Windows)
	Name string `json:"name"` // human description
}

// Selection is what to share: Mode "none", "system" (all apps except Blite),
// "apps" (only Apps, by App.Key), "window" (the process PID that owns the
// shared window, and its child processes) or "test" (beeps instead of
// captured audio; the session handles it, a Sharer doesn't).
type Selection struct {
	Mode string   `json:"mode"`
	Apps []string `json:"apps,omitempty"`
	PID  int      `json:"pid,omitempty"`
}

// CaptureSource is the GStreamer source description for the shared mix, used by
// media.StartAudioSource.
type CaptureSource string

// stream is an app's audio stream the selection matched; links is how many
// of its ports feed the shared mix (Linux; 1 on Windows).
type stream struct {
	name  string
	pid   int
	links int
}

// shareLog logs what the selection matches, for "they can't hear me": the
// matched streams whenever they change, and while an apps or window
// selection matches none, a warning with what is playing (on change, then
// at most once a minute).
type shareLog struct {
	log    *slog.Logger
	last   string
	warned time.Time
}

func (l *shareLog) note(sel Selection, matched []stream, playing []string, now time.Time) {
	parts := make([]string, len(matched))
	for i, s := range matched {
		parts[i] = fmt.Sprintf("%s pid=%d links=%d", s.name, s.pid, s.links)
	}
	sort.Strings(parts)
	key := sel.Mode + ": " + strings.Join(parts, ", ")
	changed := key != l.last
	l.last = key
	switch {
	case sel.Mode == "none":
	case len(matched) == 0 && (sel.Mode == "apps" || sel.Mode == "window"):
		if changed || now.Sub(l.warned) >= time.Minute {
			l.warned = now
			l.log.Warn("shared audio matches no app stream", "mode", sel.Mode, "apps", sel.Apps, "pid", sel.PID, "playing", playing)
		}
	case changed:
		l.log.Info("shared audio streams", "mode", sel.Mode, "streams", strings.Join(parts, ", "))
	}
}

func orDiscard(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.New(slog.DiscardHandler)
	}
	return l
}
