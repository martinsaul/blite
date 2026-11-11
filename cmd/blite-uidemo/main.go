// Command blite-uidemo shows the native window with made-up people, for
// screenshots and UI review without a call.
package main

import (
	"context"
	"os"
	"slices"

	"blite/internal/app"
	"blite/internal/audio"
	"blite/internal/nui"
	"blite/internal/session"
	"blite/internal/update"
)

type demo struct {
	inCall bool
	src    string
	dc     string // Discord state: "" | login | ready | room
	ch     chan struct{}
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
	st := app.UIState{Version: "0.1.0 (84) linux", Name: "Alice", Tag: "4417", Autostart: &auto,
		Update: &update.Info{Channel: "beta", CanApply: true}, Layout: session.Layout{Mode: "grid"},
		Reach: &app.Reach{OK: true, Text: "Others can reach you directly."}}
	switch d.dc {
	case "login":
		st.Discord = &app.Discord{Status: "login"}
	case "ready":
		st.Discord = &app.Discord{Status: "ready", ChannelID: "111", Channel: "gaming", Guild: "Game Night"}
	case "room":
		st.Discord = &app.Discord{Status: "ready", ChannelID: "111", Channel: "gaming", Guild: "Game Night", Room: "K7PQ-2M4X", Open: true, People: 3}
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
	st.Session = &session.State{Room: "K7PQ-2M4X", Link: "https://share.example/j/K7PQ2M4X#pw", Source: d.src,
		Encoder: "nvh264enc", Resolution: "1080p", Mode: "desktop", Encrypted: true, Safety: "4821 0937 5512",
		IsOwner: true, RelayMaxPeers: 8, MeshComfortPeers: 4, Layout: st.Layout, Audio: audio.Selection{Mode: "system"},
		Members: []session.Member{
			{ID: "a", Name: "Bob", Tag: "2926", Client: "0.1.0 (83) windows", State: "connected", Pair: "host/prflx", Layer: "high",
				RTTms: 152, TxKbps: 3050, RxKbps: 12, Quality: "auto", Volume: 1, Recv: 1080},
			{ID: "b", Name: "Carol", Tag: "0815", Client: "0.1.0 (83) linux", State: "connected", Pair: "srflx/srflx", Layer: "720p",
				Limit: "bandwidth", RTTms: 48, Loss: 0.021, TxKbps: 1400, RxKbps: 2900, Quality: "low", Volume: 0.6, Muted: true, Recv: 720},
			{ID: "c", Name: "Dave", Tag: "7301", Client: "0.1.0 (82) windows", State: "unreachable", Quality: "auto", Volume: 1,
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
func (d *demo) JoinLink(context.Context, string) error               { d.inCall = true; return nil }
func (d *demo) Leave()                                               { d.inCall = false }
func (d *demo) SetSource(s string) error                             { d.src = s; d.changed(); return nil }
func (d *demo) Sources() any                                         { return nil }
func (d *demo) AudioOptions() any                                    { return nil }
func (d *demo) SetAudio(string, []string) error                      { return nil }
func (d *demo) SetOutput(string) error                               { return nil }
func (d *demo) SetPeer(string, string, float64, bool) error          { return nil }
func (d *demo) SetResolution(string) error                           { return nil }
func (d *demo) SetMode(string) error                                 { return nil }
func (d *demo) SetLayout(string, int, int, string) error             { return nil }
func (d *demo) SendLogsNow(context.Context) error                    { return nil }
func (d *demo) OpenLogFolder() error                                 { return nil }
func (d *demo) Diagnostics() string                                  { return "demo" }
func (d *demo) Feedback(context.Context, string, string, bool) error { return nil }
func (d *demo) ShowWindow(string) error                              { return nil }
func (d *demo) Volunteer(bool) error                                 { return nil }
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
	arg := ""
	if len(os.Args) > 1 {
		arg = os.Args[1]
	}
	d := &demo{inCall: arg == "call", dc: arg, src: "screen", ch: make(chan struct{}, 1)}
	if arg == "servers" { // a server of the user's, picked: the server picker shows, Discord is off
		d.dc = "ready"
		d.servers = []string{"wss://share.example.com/ws"}
		d.pick = d.servers[0]
	}
	nui.Run(d, nui.Options{})
}
