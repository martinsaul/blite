// Command blite is the Blite client.
//
// Normally it runs as a tray app: the tray opens a local web UI in the default
// browser for creating and joining sessions, and every remote screen opens in
// its own native window. A share link passed as an argument (which is how the
// OS hands over blite:// links) is forwarded to the running instance.
//
//	blite                                   # tray app
//	blite 'blite://join/7K3QX9MD#secret'    # join via the running tray app (or start it)
//	blite -register                         # make this binary the blite:// handler
//
// Headless mode, for synthetic test peers and debugging:
//
//	blite -headless -create -source test -view count -for 30s
//	blite -headless -link 'https://blite.example.com/j/7K3QX9MD#secret'
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	molt "moltclient"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"blite/internal/app"
	"blite/internal/audio"
	"blite/internal/capture"
	"blite/internal/diag"
	"blite/internal/identity"
	"blite/internal/media"
	"blite/internal/proto"
	"blite/internal/register"
	"blite/internal/session"
	"blite/internal/tray"
	"blite/internal/ui"
	"blite/internal/update"
)

// Stamped by the build scripts (-X main.version=... etc.): a version name, a
// monotonic build number the server compares, and the build date. Local
// builds are "dev" with build 0. defaultChannel is the update channel used
// without -channel or BLITE_CHANNEL; defaultServer is the signaling server
// used without -server or BLITE_SERVER (BLITE_DEFAULT_SERVER at build time).
var (
	version        = "dev"
	build          = "0"
	buildDate      = ""
	defaultChannel = "stable"
	defaultServer  = "ws://localhost:8080/ws"
)

func clientMeta() *proto.ClientMeta {
	n, _ := strconv.Atoi(build)
	return &proto.ClientMeta{Platform: runtime.GOOS, AppVersion: version, Build: n, BuildDate: buildDate, OSVersion: osVersion()}
}

func main() {
	var (
		server    = flag.String("server", envOr("BLITE_SERVER", defaultServer), "signaling URL")
		headless  = flag.Bool("headless", false, "no tray or UI; run one session from flags")
		create    = flag.Bool("create", false, "headless: create a new room")
		join      = flag.String("join", "", "headless: room code to join")
		link      = flag.String("link", "", "share link (https://.../j/CODE#password or blite://join/CODE#password)")
		password  = flag.String("password", "", "room password (create: generated if empty)")
		name      = flag.String("name", hostname(), "display name")
		source    = flag.String("source", "", "video source: screen | test | none | a monitor:/window: source from -sources (default: screen with -headless; none in the app, where the user starts sharing)")
		sources   = flag.Bool("sources", false, "list shareable monitors and windows and exit")
		showVer   = flag.Bool("version", false, "print the version and exit")
		channel   = flag.String("channel", envOr("BLITE_CHANNEL", defaultChannel), "update channel: stable | beta")
		pinned    = flag.String("pin-channel", "", "update channel chosen by hand, kept across self-updates (set by the updater)")
		view      = flag.String("view", "window", "remote video: window | count | none")
		audioArg  = flag.String("audio", "none", "headless: shared audio: none | test | system | app:<name>")
		encoder   = flag.String("encoder", os.Getenv("BLITE_ENCODER"), "force an encoder factory (e.g. x264enc)")
		relay     = flag.Bool("relay", false, "force TURN relay (ICE transport policy relay)")
		dur       = flag.Duration("for", 0, "headless: leave after this long (0 = until Ctrl-C)")
		volunteer = flag.Bool("volunteer", false, "headless: volunteer as the room's relay (relay mode)")
		resArg    = flag.String("resolution", "720p", "what we share at: 720p | 1080p | 2160p (4K)")
		hostFile  = flag.String("host", "", "hosted relay: keep the rooms in this JSON file open and relay them (needs BLITE_HOST_KEY)")
		uiPort    = flag.Int("ui-port", 0, "local UI port (0 = random)")
		reg       = flag.Bool("register", false, "register this binary as the blite:// link handler and exit")
		bg        = flag.Bool("background", false, "start in the tray without opening the window (used at sign-in)")
		jsonLog   = flag.Bool("json", false, "JSON logs")
		upKbps    = flag.Int("relay-upload-kbps", 0, "relay: our upload capacity; transcoded renditions must fit in 80% of it (0 = no cap)")
	)
	flag.Parse()
	ch, pin := updateChannel(*channel, *pinned, flagSet("channel"), molt.UpdateResult() != "")

	var h slog.Handler = slog.NewTextHandler(stderr{}, nil)
	if *jsonLog {
		h = slog.NewJSONHandler(stderr{}, nil)
	}
	log := slog.New(h).With("version", clientMeta().Label())
	if addr := os.Getenv("BLITE_PPROF"); addr != "" { // e.g. 127.0.0.1:6060 (memory investigations)
		go func() { log.Warn("pprof", "err", http.ListenAndServe(addr, nil)) }()
	}

	if *reg {
		exe, err := os.Executable()
		if err == nil {
			err = register.Register(register.Launcher(exe))
		}
		if err != nil {
			fatal(log, "register", err)
		}
		fmt.Println("registered blite:// links to", exe)
		return
	}
	if *showVer {
		c := clientMeta()
		fmt.Printf("Blite %s, built %s, %s\n", c.Label(), c.BuildDate, c.OSVersion)
		return
	}
	if *sources {
		ts, err := capture.List()
		if err != nil {
			fatal(log, "list sources", err)
		}
		for _, t := range ts {
			fmt.Printf("%-28s %s: %s\n", t.Source, t.Kind, t.Name)
		}
		return
	}
	// The OS passes a blite:// (or pasted https) link as a bare argument.
	if *link == "" && flag.NArg() == 1 && isLink(flag.Arg(0)) {
		*link = flag.Arg(0)
	}

	cfg := session.Config{Server: *server, Name: *name, Client: clientMeta(), View: media.ViewKind(*view), Encoder: *encoder, Relay: *relay, UploadKbps: *upKbps, Log: log}
	cfg.Name = orDefault(identity.CleanName(cfg.Name), "Blite user") // '#' is reserved for the tag
	if *hostFile != "" {
		key, err := identity.LoadOrCreate() // a stable identity, so members don't see a new key
		if err != nil {
			fatal(log, "identity key", err)
		}
		cfg.Identity = key
		runHost(cfg, log, *hostFile)
		return
	}
	if *headless {
		runHeadless(cfg, log, *create, *join, *link, *password, media.SourceKind(orDefault(*source, "screen")), *audioArg, *dur, *volunteer, media.Resolution(*resArg))
		return
	}
	if relaunchGuarded(log) {
		return
	}
	runTray(cfg, log, *link, media.SourceKind(orDefault(*source, "none")), *uiPort, ch, pin, *bg)
}

// relaunchGuarded starts the tray app through the Windows crash guard
// when it was started directly, so the guard always runs with
// Blite. It reports whether this process should exit.
func relaunchGuarded(log *slog.Logger) bool {
	if runtime.GOOS != "windows" || os.Getenv("BLITE_GUARD") != "" {
		return false
	}
	if why := os.Getenv("BLITE_UNGUARDED"); why != "" { // the guard started us, unguarded
		unguarded = why
		return false
	}
	// The updater (molt-swap) launched us to check a new build: it watches
	// this process, and a relaunch through the guard would look like an exit
	// before reporting healthy, so the update would roll back. Once healthy
	// we hand over (startGuarded).
	if os.Getenv(molt.EnvHealthFile) != "" {
		unguarded = "started by molt-swap to check an update"
		return false
	}
	exe, err := os.Executable()
	if err != nil {
		unguarded = err.Error()
		return false
	}
	g := register.Launcher(exe)
	if g == exe {
		unguarded = "no blite-guard.exe in " + filepath.Dir(filepath.Dir(exe))
		return false
	}
	if err := exec.Command(g, os.Args[1:]...).Start(); err != nil {
		unguarded = "starting " + g + ": " + err.Error()
		return false
	}
	return true
}

// unguarded is why this Windows run has no crash guard ("" = it has one, or
// isn't Windows); logged once the log file is open.
var unguarded string

// stderr writes to whatever os.Stderr is now (logToFiles may replace it).
type stderr struct{}

func (stderr) Write(b []byte) (int, error) { return os.Stderr.Write(b) }

// guardedAfterUpdate (Windows) is whether this run, which molt-swap started
// to health-check a new build and so runs without the guard, should hand
// over to a guarded instance once it is healthy. Without that, a crash in
// the run after an update left no logs and no dump.
func guardedAfterUpdate(link string) bool {
	return runtime.GOOS == "windows" && os.Getenv("BLITE_GUARD") == "" &&
		os.Getenv(molt.EnvHealthFile) != "" && link == ""
}

// startGuarded starts Blite again through the guard, in the tray, carrying
// the update outcome this run already read for the UI. molt-swap counts
// "reported healthy, then exited" as a good update.
func startGuarded(log *slog.Logger, result string) {
	exe, err := os.Executable()
	if err != nil {
		log.Warn("hand over to the crash guard", "err", err)
		return
	}
	g := register.Launcher(exe)
	if g == exe {
		return
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, molt.EnvHealthFile+"=") && !strings.HasPrefix(kv, molt.EnvUpdateResult+"=") {
			env = append(env, kv)
		}
	}
	if result != "" {
		env = append(env, molt.EnvUpdateResult+"="+result)
	}
	cmd := exec.Command(g, append(os.Args[1:], "-background")...)
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		log.Warn("hand over to the crash guard", "err", err)
		return
	}
	log.Info("updated build is healthy: continuing under the crash guard", "pid", cmd.Process.Pid)
}

func flagSet(name string) (set bool) {
	flag.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return
}

func isLink(s string) bool {
	return strings.HasPrefix(s, "blite:") || strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")
}

func runTray(cfg session.Config, log *slog.Logger, link string, src media.SourceKind, port int, channel string, pinChannel bool, background bool) {
	// Second launch: hand over to the running instance.
	if in, err := app.ForwardLink(link); err == nil {
		if link == "" {
			app.OpenBrowser(fmt.Sprintf("http://127.0.0.1:%d/", in.Port))
		}
		return
	} else if err != app.ErrNoInstance {
		fatal(log, "join", err)
	}
	logToFiles(log)
	if unguarded != "" {
		log.Warn("running without the crash guard", "why", unguarded)
	}
	defer log.Info(cleanExit)

	// The name the user chose in the app wins over the hostname default.
	if n := identity.LoadName(); n != "" && !flagSet("name") {
		cfg.Name = n
	}
	cfg.Name = orDefault(identity.CleanName(cfg.Name), "Blite user")
	key, err := identity.LoadOrCreate()
	if err != nil {
		fatal(log, "identity key", err)
	}
	cfg.Identity, cfg.Known = key, identity.LoadKnown()
	go diag.WatchModules(context.Background(), log)
	a := app.New(cfg, src)
	a.Updates = update.New(cfg.Client.AppVersion, int64(cfg.Client.Build), channel, pinChannel, log)
	if cfg.Client.Build > 0 {
		a.Exe = installDefaults(log)
	}
	a.Exit = tray.Quit
	rep := &reporter{server: cfg.Server, log: log, name: a.DisplayName,
		base: proto.Report{Platform: runtime.GOOS, Version: cfg.Client.Label(), Build: cfg.Client.Build, OSVersion: cfg.Client.OSVersion}}
	a.Logs = func() any { return logState() }
	a.SendLogs = rep.send
	a.OpenLogs = func() error {
		d := logState().Dir
		if d == "" {
			return errors.New("no log folder")
		}
		os.MkdirAll(d, 0o700)
		return app.OpenBrowser(d)
	}
	go rep.previous()
	srv, err := ui.Start(a, port, log)
	if err != nil {
		fatal(log, "local UI", err)
	}
	pid := os.Getpid()
	if err := app.WriteInstance(app.Instance{Port: srv.Port, Token: srv.Token, PID: pid}); err != nil {
		log.Warn("instance file", "err", err)
	}
	defer app.RemoveInstance(pid)
	log.Info("local UI", "url", fmt.Sprintf("http://127.0.0.1:%d/", srv.Port))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		tray.Quit()
	}()

	open := func() {
		if err := app.OpenBrowser(srv.URL()); err != nil {
			log.Error("open browser", "err", err, "url", srv.URL())
		}
	}
	handOff := a.Updates != nil && guardedAfterUpdate(link)
	tray.Run(tray.Actions{Open: open, Leave: a.Leave, Quit: func() {}}, func() {
		if a.Updates != nil {
			// The UI is listening and the tray is up: this is "healthy" for
			// molt-swap after an update. Then check now and every 6 hours.
			a.Updates.Started()
			if handOff {
				go tray.Quit()
				return
			}
			go func() {
				for {
					a.CheckUpdate(context.Background())
					time.Sleep(6 * time.Hour)
				}
			}()
		}
		if link != "" {
			go func() {
				if err := a.JoinLink(context.Background(), link); err != nil {
					log.Error("join", "err", err)
				}
				open()
			}()
			return
		}
		if !background {
			open()
		}
	})
	a.Shutdown()
	srv.Close()
	if handOff {
		app.RemoveInstance(pid) // before the new instance looks for one
		startGuarded(log, a.Updates.Info().Result)
	}
}

func runHeadless(cfg session.Config, log *slog.Logger, create bool, join, link, password string, src media.SourceKind, audioArg string, dur time.Duration, volunteer bool, res media.Resolution) {
	if link != "" {
		code, pw, err := session.ParseLink(link)
		if err != nil {
			fatal(log, "bad link", err)
		}
		join, password = code, pw
	}
	if !create && join == "" {
		fmt.Fprintln(os.Stderr, "headless needs -create, -join CODE or -link URL")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if dur > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, dur)
		defer cancel()
	}
	var (
		s   *session.Session
		err error
	)
	if create {
		s, err = session.Create(ctx, cfg, password)
	} else {
		s, err = session.Join(ctx, cfg, join, password)
	}
	if err != nil {
		fatal(log, "signaling", err)
	}
	defer s.Close()
	st := s.State()
	fmt.Printf("room %s  link %s\n", st.Room, st.Link)
	if src != "none" {
		if err := s.SetResolution(res); err != nil {
			fatal(log, "resolution", err)
		}
		if err := s.SetSource(src); err != nil {
			fatal(log, "video source", err)
		}
		log.Info("sharing", "source", src, "encoder", s.State().Encoder)
	}
	if audioArg != "none" {
		sel := audio.Selection{Mode: audioArg}
		if name, ok := strings.CutPrefix(audioArg, "app:"); ok {
			sel = audio.Selection{Mode: "apps"}
			apps, _ := audio.ListApps()
			for _, a := range apps {
				if a.Name == name {
					sel.Apps = append(sel.Apps, a.Key)
				}
			}
		}
		if err := s.SetAudio(sel); err != nil {
			fatal(log, "audio", err)
		}
		log.Info("sharing audio", "mode", sel.Mode, "apps", sel.Apps)
	}

	if volunteer {
		if err := s.Volunteer(true); err != nil {
			fatal(log, "volunteer", err)
		}
		log.Info("volunteered as relay")
	}

	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	smp := newSampler()
	for {
		select {
		case <-ctx.Done():
			report(s)
			return
		case <-s.Done():
			log.Info("session ended")
			return
		case <-tick.C:
			smp.log(log, s.State())
		}
	}
}

func summary(st session.State) string {
	var parts []string
	for _, m := range st.Members {
		parts = append(parts, fmt.Sprintf("%s:%s frames=%d audio=%d pair=%s send=%s", m.Name, m.State, m.Frames, m.Audio, m.Pair, m.Layer))
	}
	return strings.Join(parts, "; ")
}

func report(s *session.Session) {
	fmt.Printf("final %s\n", summary(s.State()))
}

// installDefaults runs in release builds. Starting at sign-in is opt-in (the
// user turns it on in the window); when it is on, each start refreshes the
// path in case the install moved. On Windows it also makes this install the
// blite:// link handler. It returns the binary path ("" if unknown).
func installDefaults(log *slog.Logger) string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	exe, _ = filepath.EvalSymlinks(exe)
	if register.AutostartEnabled() {
		if err := register.SetAutostart(true, exe); err != nil {
			log.Warn("autostart", "err", err)
		}
	}
	// The .deb's desktop file already handles blite:// on Linux.
	if runtime.GOOS == "windows" {
		if err := register.Register(register.Launcher(exe)); err != nil {
			log.Warn("link handler", "err", err)
		}
	}
	return exe
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// updateChannel picks the update channel and whether to keep it across
// self-updates. Only a channel chosen by hand (-channel on the command line,
// or -pin-channel from a previous self-update) is kept. A build relaunched by
// molt-swap ignores -channel: builds up to 50 always passed their own stamped
// default there, which kept a wrongly stamped channel alive forever.
func updateChannel(channel, pinned string, channelSet, relaunched bool) (string, bool) {
	switch {
	case pinned != "":
		return pinned, true
	case relaunched:
		return envOr("BLITE_CHANNEL", defaultChannel), false
	}
	return channel, channelSet
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func hostname() string {
	h, _ := os.Hostname()
	if h == "" {
		return "blite"
	}
	return h
}

func fatal(log *slog.Logger, msg string, err error) {
	log.Error(msg, "err", err)
	os.Exit(1)
}
