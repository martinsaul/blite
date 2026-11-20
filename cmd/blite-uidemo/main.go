// Command blite-uidemo shows the native window with made-up people, for
// screenshots and UI review without a call.
//
// Scenes:
//
//	home          Home screen (Share card, Join card, Recent rooms)
//	discord       Home screen with Discord channel Join + Share banner (alias: ready)
//	discord-watch Home screen with Discord sharer + avatars + Watch (alias: room)
//	login         Home screen with Discord Connect prompt
//	servers       Home screen with a custom signaling server picked
//	sharesheet    Share options sheet open
//	advanced      Share options sheet with Advanced... expanded
//	call          In-call control window (people list, eyes, relay footer)
//	panel         In-call control window with a person's side panel open
//	gear          Control window with Gear menu open (and update dot)
//	help          Control window with ? menu open
//	picker        "Share a monitor or window" with made-up displays and windows
//	video         The main video window: two streams and our preview in its corner
//	video-tile    The main video window with our preview expanded to a tile
package main

import (
	"context"
	"flag"
	"image"
	"image/color"
	"math"
	"slices"
	"time"

	"blite/internal/app"
	"blite/internal/audio"
	"blite/internal/capture"
	"blite/internal/feeds"
	"blite/internal/nui"
	"blite/internal/session"
	"blite/internal/update"
)

type demo struct {
	inCall        bool
	src           string
	dc            string // Discord state: "" | login | ready | room
	fps           int
	uploadKbps    int
	updateWaiting bool
	autoWatch     bool
	playSounds    bool
	portMap       bool
	selfWatch     bool
	bobWatch      bool
	carolWatch    bool
	relay         bool
	presets       []app.QualityPreset
	ch            chan struct{}
	// servers: the user's own signaling servers; def and pick: the default
	// and the lobby's pick ("" = the built-in one).
	servers   []string
	def, pick string
}

const demoServer = "wss://bellion.example.com/ws"

func (d *demo) changed() {
	select {
	case d.ch <- struct{}{}:
	default:
	}
}

func (d *demo) State() any {
	auto := true
	fps := d.fps
	if fps == 0 {
		fps = 60
	}
	slow := session.SlowResolution
	if fps < 45 {
		slow = session.SlowFPS
	}
	up := &update.Info{Channel: "beta"}
	if d.updateWaiting {
		up.CanApply = true
		up.Available = "0.1.0 (85)"
	}
	st := app.UIState{Version: "0.1.0 (84) linux", Name: "Alice", Tag: "4417", Autostart: &auto,
		AutoWatch: d.autoWatch, PlaySounds: d.playSounds,
		Share: app.ShareConfig{
			Source:        orDemoSrc(d.src, d.inCall),
			Resolution:    "1080p",
			FPS:           fps,
			EffectiveSlow: slow,
			MinHeight:     360,
			SmallerCopy:   true,
			UploadKbps:    d.uploadKbps,
			Audio:         audio.Selection{Mode: "system"},
			E2EE:          true,
			PortMap:       d.portMap,
			Presets:       d.presets,
		},
		Update: up,
		Reach:  &app.Reach{OK: true, Text: "Others can reach you directly."}}
	switch d.dc {
	case "login":
		st.Discord = &app.Discord{Status: "login"}
	case "ready":
		st.Discord = &app.Discord{Status: "ready", ChannelID: "111", Channel: "gaming", Guild: "Game Night"}
	case "room":
		st.Discord = &app.Discord{Status: "ready", ChannelID: "111", Channel: "gaming", Guild: "Game Night", Room: "K7PQ-2M4X", Open: true, People: 3, Sharers: []string{"Bob"}, Members: []string{"Bob", "Carol", "Dave"}}
	}
	st.Recent = []app.RecentRoom{
		{Code: "K7PQ2M4X", Display: "K7PQ-2M4X", Server: demoServer, Host: "bellion.example.com", BuiltIn: true, Status: "ready"},
		{Code: "MAMAC0", Display: "MAMAC0", Server: "wss://share.example.com/ws", Host: "share.example.com", HasPassword: true, Status: "checking"},
	}
	srv := &app.Servers{List: []app.Server{{URL: demoServer, Host: "bellion.example.com", BuiltIn: true}}, Default: demoServer, Selected: demoServer}
	for _, c := range d.servers {
		srv.List = append(srv.List, app.Server{URL: c, Host: session.ServerHost(c)})
	}
	if d.def != "" {
		srv.Default, srv.Selected = d.def, d.def
	}
	if d.pick != "" {
		srv.Selected = d.pick
	}
	st.Servers = srv
	if !d.inCall {
		return st
	}
	selfEye := feeds.EyeGrey
	if d.src != "" && d.src != "none" {
		if d.selfWatch {
			selfEye = feeds.EyeOpen
		} else {
			selfEye = feeds.EyeClosed
		}
	}
	bobEye := feeds.EyeClosed
	if d.bobWatch {
		bobEye = feeds.EyeOpen
	}
	carolEye := feeds.EyeClosed
	if d.carolWatch {
		carolEye = feeds.EyeOpen
	}
	var relays []session.RelayEntry
	if d.relay {
		relays = []session.RelayEntry{{ID: "self", Name: "Alice", Me: true, Children: 2}}
	}
	st.Session = &session.State{Room: "K7PQ-2M4X", Link: "https://share.example/j/K7PQ2M4X#pw", Source: d.src,
		Encoder: "nvh264enc", Resolution: "1080p", Mode: "game", Encrypted: true, Safety: "4821 0937 5512",
		IsOwner: true, RelayMaxPeers: 8, MeshComfortPeers: 4, Audio: audio.Selection{Mode: "system"},
		ShareOptions:    session.ShareOptions{FPS: fps, MinHeight: 360, SmallerCopy: true, UploadKbps: 10000},
		UploadLimitKbps: 10000, TxKbps: 4450,
		SelfEye: selfEye, AutoWatch: d.autoWatch, Relay: relays,
		Members: []session.Member{
			{ID: "a", Name: "Bob", Tag: "2926", Client: "0.1.0 (83) windows", State: "connected", Pair: "host/prflx", Layer: "high",
				RTTms: 42, TxKbps: 3050, RxKbps: 4200, FPSIn: 60, Quality: "auto", Volume: 1, Recv: 1080, Sharing: true, Watching: d.bobWatch, Eye: bobEye},
			{ID: "b", Name: "Carol", Tag: "0815", Client: "0.1.0 (83) linux", State: "connected", Pair: "srflx/srflx", Layer: "720p",
				Limit: "bandwidth", RTTms: 185, Loss: 0.062, TxKbps: 1400, RxKbps: 2900, FPSIn: 30, Quality: "low", Volume: 0.6, Muted: true, Recv: 720, Sharing: true, Watching: d.carolWatch, Eye: carolEye},
			{ID: "c", Name: "Dave", Tag: "7301", Client: "0.1.0 (82) windows", State: "unreachable", Quality: "auto", Volume: 1, Eye: feeds.EyeGrey,
				Hint: "No route: their network may block UDP."},
		},
	}
	return st
}

func (d *demo) CheckUpdate(context.Context) error   { return nil }
func (d *demo) InstallUpdate(context.Context) error { return nil }
func (d *demo) DismissUpdateResult()                {}
func (d *demo) SetAutostart(bool) error             { return nil }
func (d *demo) Create(context.Context, string, bool) error {
	d.inCall = true
	d.src = ""
	d.changed()
	return nil
}
func (d *demo) JoinLink(context.Context, string) error { d.inCall = true; d.changed(); return nil }
func (d *demo) ProbeRecentRooms()                      {}
func (d *demo) JoinRecentRoom(context.Context, string, string) error {
	d.inCall = true
	d.changed()
	return nil
}
func (d *demo) RemoveRecentRoom(string, string) error       { return nil }
func (d *demo) Leave()                                      { d.inCall = false; d.changed() }
func (d *demo) SetSource(s string) error                    { d.src = s; d.changed(); return nil }
func (d *demo) Sources() any                                { return nil }
func (d *demo) AudioOptions() any                           { return nil }
func (d *demo) SetAudio(string, []string) error             { return nil }
func (d *demo) SetOutput(string) error                      { return nil }
func (d *demo) SetPeer(string, string, float64, bool) error { return nil }
func orDemoSrc(s string, inCall bool) string {
	if s != "" {
		return s
	}
	if inCall {
		return "none"
	}
	return "primary"
}

func (d *demo) SetResolution(string) error { return nil }
func (d *demo) SetMode(string) error       { return nil }
func (d *demo) SetShareOptions(fps int, _ string, _ int, _ bool, upKbps int) error {
	d.fps = fps
	d.uploadKbps = upKbps
	d.changed()
	return nil
}
func (d *demo) SetRoomSecurity(bool, string, bool) error { return nil }
func (d *demo) AddQualityPreset(res string, fps int) error {
	d.presets = append(d.presets, app.QualityPreset{Resolution: res, FPS: fps})
	d.changed()
	return nil
}
func (d *demo) RemoveQualityPreset(res string, fps int) error {
	d.presets = slices.DeleteFunc(d.presets, func(p app.QualityPreset) bool { return p == app.QualityPreset{Resolution: res, FPS: fps} })
	d.changed()
	return nil
}
func (d *demo) ToggleWatch(id string) error {
	switch id {
	case feeds.Self:
		d.selfWatch = !d.selfWatch
	case "a":
		d.bobWatch = !d.bobWatch
	case "b":
		d.carolWatch = !d.carolWatch
	}
	d.changed()
	return nil
}
func (d *demo) PopOut(string) error                                  { return nil }
func (d *demo) Pin(string) error                                     { return nil }
func (d *demo) SetAutoWatch(on bool) error                           { d.autoWatch = on; d.changed(); return nil }
func (d *demo) SetPlaySounds(on bool) error                          { d.playSounds = on; d.changed(); return nil }
func (d *demo) SetPortMap(on bool) error                             { d.portMap = on; d.changed(); return nil }
func (d *demo) DismissLeavePrompt()                                  {}
func (d *demo) SendLogsNow(context.Context) error                    { return nil }
func (d *demo) OpenLogFolder() error                                 { return nil }
func (d *demo) Diagnostics() string                                  { return "demo" }
func (d *demo) Feedback(context.Context, string, string, bool) error { return nil }
func (d *demo) Volunteer(on bool) error                              { d.relay = on; d.changed(); return nil }
func (d *demo) SetName(string) error                                 { return nil }
func (d *demo) Transfer(string) error                                { return nil }
func (d *demo) Kick(string, bool) error                              { return nil }
func (d *demo) ShowUI()                                              {}
func (d *demo) Subscribe() (<-chan struct{}, func())                 { return d.ch, func() {} }

func (d *demo) DiscordLogin() {}
func (d *demo) GoLiveInChannel(context.Context, string, bool) error {
	d.inCall = true
	d.changed()
	return nil
}
func (d *demo) EnterChannel(context.Context, string, bool) error {
	d.inCall = true
	d.src = "none"
	d.changed()
	return nil
}
func (d *demo) JoinChannel(context.Context, string) error { d.inCall = true; d.changed(); return nil }

func (d *demo) AddServer(s string) error {
	n, err := session.NormalizeServer(s)
	if err != nil {
		return err
	}
	d.servers = append(d.servers, n)
	d.changed()
	return nil
}

func (d *demo) RemoveServer(url string) error {
	d.servers = slices.DeleteFunc(d.servers, func(c string) bool { return c == url })
	if d.def == url {
		d.def = ""
	}
	d.changed()
	return nil
}

func (d *demo) SetDefaultServer(url string) error {
	d.def, d.pick = url, ""
	if url == demoServer {
		d.def = ""
	}
	d.changed()
	return nil
}

func (d *demo) SelectServer(url string) error { d.pick = url; d.changed(); return nil }

func main() {
	quitAfter := flag.Duration("quit-after", 0, "quit automatically after duration")
	flag.Parse()
	arg := ""
	if flag.NArg() > 0 {
		arg = flag.Arg(0)
	}
	d := &demo{
		src: "primary", fps: 60,
		autoWatch: true, playSounds: true, portMap: true, selfWatch: true, bobWatch: true, carolWatch: false,
		presets: []app.QualityPreset{{Resolution: "1080p", FPS: 45}},
		ch:      make(chan struct{}, 1),
	}
	var scene string
	switch arg {
	case "", "home":
	case "discord", "ready":
		d.dc = "ready"
	case "discord-watch", "room":
		d.dc = "room"
	case "login":
		d.dc = "login"
	case "servers":
		d.dc = "ready"
		d.servers = []string{"wss://share.example.com/ws"}
		d.pick = d.servers[0]
	case "sharesheet":
		scene = "sharesheet"
	case "advanced":
		d.fps = 45
		d.uploadKbps = 10000
		scene = "advanced"
	case "call":
		d.inCall = true
	case "panel":
		d.inCall = true
		scene = "panel"
	case "gear":
		d.inCall = true
		d.updateWaiting = true
		scene = "gear"
	case "help":
		d.inCall = true
		scene = "help"
	case "picker":
		d.inCall = true
		scene = "picker"
	case "video", "video-tile":
		videoScene(arg == "video-tile", *quitAfter)
		return
	}
	opts := nui.Options{Scene: scene, Targets: demoTargets, TargetIcons: demoIcons}
	if *quitAfter > 0 {
		opts.OnReady = func() {
			time.Sleep(*quitAfter)
			nui.Quit()
		}
	}
	nui.Run(d, opts)
}

// demoTargets are made-up displays and windows for the source picker.
func demoTargets() ([]capture.Target, error) {
	return []capture.Target{
		{Kind: "monitor", Name: "DP-1 (5120x2880), primary", Source: "monitor:0,0,5120,2880", Primary: true},
		{Kind: "monitor", Name: "DP-2 (5120x2880)", Source: "monitor:5120,0,5120,2880"},
		{Kind: "window", Name: "#general | Game Night - Discord", Source: "window:0x1", App: "discord"},
		{Kind: "window", Name: "Bellion Share - Mozilla Firefox", Source: "window:0x2", App: "firefox"},
		{Kind: "window", Name: "~/projects - Terminal", Source: "window:0x3", App: "Gnome-terminal"},
		{Kind: "window", Name: "notes.txt - Text Editor", Source: "window:0x4", App: "xed"},
		{Kind: "window", Name: "Untitled Window", Source: "window:0x5", App: "legacy"},
	}, nil
}

// demoIcons draws a round app icon for most demo windows; one keeps the
// generic icon, as an app without one does.
func demoIcons(ts []capture.Target) map[string]image.Image {
	cols := map[string]color.NRGBA{
		"window:0x1": {0x58, 0x65, 0xF2, 0xFF},
		"window:0x2": {0xFF, 0x71, 0x39, 0xFF},
		"window:0x3": {0x30, 0x34, 0x3C, 0xFF},
		"window:0x4": {0x3C, 0xCB, 0x7F, 0xFF},
	}
	out := map[string]image.Image{}
	for _, t := range ts {
		c, ok := cols[t.Source]
		if !ok {
			continue
		}
		const s = 48
		img := image.NewNRGBA(image.Rect(0, 0, s, s))
		for y := range s {
			for x := range s {
				if d := math.Hypot(float64(x)-s/2+0.5, float64(y)-s/2+0.5); d < s/2 {
					img.SetNRGBA(x, y, c)
				}
			}
		}
		out[t.Source] = img
	}
	return out
}
