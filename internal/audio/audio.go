// Package audio picks what a Blite user shares, OBS-style: one or more apps, or
// every app except Blite ("system"), and which output device plays the call.
//
// Linux (PipeWire): WirePlumber won't link a capture stream to another app's
// output stream, so Blite creates a virtual sink "blite-share", links the chosen
// apps' output ports into it IN ADDITION to their normal outputs (the user keeps
// hearing them), and captures that sink's monitor. Verified 2026-10-24 with two
// tone generators: only the linked app's tone reaches the capture.
package audio

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

// Selection is what to share: Mode "none", "system" (all apps except Blite) or
// "apps" (only Apps, by App.Key).
type Selection struct {
	Mode string   `json:"mode"`
	Apps []string `json:"apps,omitempty"`
}

// CaptureSource is the GStreamer source description for the shared mix, used by
// media.StartAudioSource.
type CaptureSource string
