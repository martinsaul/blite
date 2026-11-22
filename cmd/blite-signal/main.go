// Command blite-signal is Blite's signaling server: it introduces peers in a room
// and relays their SDP/ICE. It never carries media.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"blite/internal/canary"
	"blite/internal/server"
	"blite/internal/turn"
)

func main() {
	// `blite-signal -healthcheck` probes a running instance; used by the Docker
	// HEALTHCHECK because the distroless image has no shell or curl.
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		c := http.Client{Timeout: 3 * time.Second}
		res, err := c.Get("http://127.0.0.1" + env("BLITE_ADDR", ":8080") + "/healthz")
		if err != nil || res.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		os.Exit(0)
	}
	// Blue/green deploy hooks.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-drain", "-undrain", "-adopt", "-status":
			os.Exit(adminCmd(os.Args[1][1:]))
		case "-canary":
			os.Exit(canaryCmd(os.Args[2:]))
		}
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	addr := env("BLITE_ADDR", ":8080")
	ice, err := iceFromEnv()
	if err != nil {
		log.Error("bad ICE config", "err", err)
		os.Exit(2)
	}
	// Client build gates (see server.Config): BLITE_MIN_COMPATIBLE_BUILD only
	// for breaking protocol changes, BLITE_MIN_RECOMMENDED_BUILD otherwise.
	minCompat, err1 := strconv.Atoi(env("BLITE_MIN_COMPATIBLE_BUILD", "0"))
	minRec, err2 := strconv.Atoi(env("BLITE_MIN_RECOMMENDED_BUILD", "0"))
	if err1 != nil || err2 != nil {
		log.Error("BLITE_MIN_COMPATIBLE_BUILD / BLITE_MIN_RECOMMENDED_BUILD must be build numbers")
		os.Exit(1)
	}
	// Rooms are uncapped unless the operator sets BLITE_MAX_PEERS.
	maxPeers, err := strconv.Atoi(env("BLITE_MAX_PEERS", "0"))
	if err != nil || maxPeers < 0 {
		log.Error("BLITE_MAX_PEERS must be a number (0 = no cap)")
		os.Exit(1)
	}
	s := server.New(server.Config{
		TrustProxy:          env("BLITE_TRUST_PROXY", "false") == "true",
		ICE:                 ice,
		MinCompatibleBuild:  minCompat,
		MinRecommendedBuild: minRec,
		MaxPeers:            maxPeers,
		HostKey:             os.Getenv("BLITE_HOST_KEY"),
		SiteAliases:         siteAliases(os.Getenv("BLITE_SITE_ALIASES")),
		ReportDir:           os.Getenv("BLITE_REPORT_DIR"),
		DiscordClientID:     os.Getenv("BLITE_DISCORD_CLIENT_ID"),
		DiscordSecret:       os.Getenv("BLITE_DISCORD_SECRET"),
		DiscordPublicKey:    os.Getenv("BLITE_DISCORD_PUBLIC_KEY"),
		DiscordBotToken:     os.Getenv("BLITE_DISCORD_BOT_TOKEN"),
		DiscordLinksFile:    os.Getenv("BLITE_DISCORD_LINKS_FILE"),
		DiscordLinkRedirect: os.Getenv("BLITE_DISCORD_LINK_REDIRECT"),
		UpdateURL:           os.Getenv("BLITE_UPDATE_URL"),
		UpdateChannel:       env("BLITE_DOWNLOAD_CHANNEL", "beta"),
		// BLITE_STATE_FILE keeps rooms across restarts: members resume
		// instead of rejoining. It holds password and token hashes; keep
		// it on a private volume.
		StateFile: os.Getenv("BLITE_STATE_FILE"),
		// Blue/green: BLITE_COLOR=green plus the file
		// .ci-deploy-defer-green next to the state file at startup =
		// deferred: the state file is neither loaded nor written until
		// adopt (SIGHUP or -adopt).
		DeferState: server.ShouldDefer(os.Getenv("BLITE_STATE_FILE"), os.Getenv("BLITE_COLOR")),
		// BLITE_STANDBY=1: the warm standby behind the Worker.
		// It takes sessions through the Worker only on a failover, or
		// once BLITE_ACTIVE_FILE (default "active" next to the state
		// file) exists.
		Standby:    env("BLITE_STANDBY", "") == "1",
		ActiveFile: os.Getenv("BLITE_ACTIVE_FILE"),
		Log:        log,
	})
	if standby := env("BLITE_STANDBY", ""); standby != "" && standby != "0" && standby != "1" {
		log.Error("BLITE_STANDBY must be 1 or unset")
		os.Exit(1)
	} else if standby == "1" {
		log.Info("standby mode: sessions through the Worker only on a failover or when promoted")
	}
	if s.Deferred() {
		log.Info("deferred: state file neither loaded nor written until adopt (SIGHUP or -adopt)",
			"color", os.Getenv("BLITE_COLOR"), "deferFile", server.DeferFile(os.Getenv("BLITE_STATE_FILE"), os.Getenv("BLITE_COLOR")))
	} else if err := s.LoadState(); err != nil {
		log.Error("load state; starting empty", "err", err)
	}
	// SIGHUP reloads the state file (rooms this server has served since
	// are kept). While deferred it does nothing: only -adopt ends that, so
	// a stray HUP (statesync) can't make a second writer before the old
	// colour's final save.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			if s.Deferred() {
				log.Warn("SIGHUP ignored while deferred; adopt with -adopt")
				continue
			}
			if err := s.LoadState(); err != nil {
				log.Error("reload state", "err", err)
			}
		}
	}()
	// The deploy hooks' admin listener: loopback only, never through
	// the proxy (it reaches the container's own address, not 127.0.0.1).
	if aa := adminAddr(); aa != "off" {
		go func() {
			ahs := &http.Server{Addr: aa, Handler: s.AdminHandler(), ReadHeaderTimeout: 10 * time.Second}
			if err := ahs.ListenAndServe(); err != nil {
				log.Error("admin listener: deploy hooks (-drain, -adopt) unavailable", "addr", aa, "err", err)
			}
		}()
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go s.Run(ctx)

	hs := &http.Server{Addr: addr, Handler: s, ReadHeaderTimeout: 10 * time.Second}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		// Save before the connections close, so everyone restores as
		// connected and gets a full resume window from the restart.
		if s.Drained() || s.Deferred() {
			log.Info("shutdown: state file not saved (drained or deferred: it isn't ours)")
		} else if err := s.SaveState(); err != nil {
			log.Error("save state", "err", err)
		}
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(sctx)
	}()
	log.Info("listening", "addr", addr)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
	<-stopped
}

// adminAddr is the deploy hooks' listener (BLITE_ADMIN_ADDR, "off" = none).
func adminAddr() string { return env("BLITE_ADMIN_ADDR", "127.0.0.1:8081") }

// adminCmd runs a deploy hook (drain, undrain, adopt, status) on the local
// server's admin listener. It prints the server's answer (stdout on
// success, stderr otherwise) and returns the exit code.
func adminCmd(op string) int {
	aa := adminAddr()
	if aa == "off" {
		fmt.Fprintln(os.Stderr, op+": BLITE_ADMIN_ADDR=off: no admin listener")
		return 2
	}
	if strings.HasPrefix(aa, ":") {
		aa = "127.0.0.1" + aa
	}
	c := http.Client{Timeout: 30 * time.Second} // drain fsyncs the state file
	url := "http://" + aa + "/v1/admin/" + op
	var res *http.Response
	var err error
	if op == "status" {
		res, err = c.Get(url)
	} else {
		res, err = c.Post(url, "text/plain", nil)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, op+":", err)
		return 1
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	if res.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "%s: %s: %s", op, res.Status, body)
		return 1
	}
	os.Stdout.Write(body)
	return 0
}

// canaryCmd runs `-canary <ws(s) URL> [-connect host:port]`.
func canaryCmd(args []string) int {
	fs := flag.NewFlagSet("canary", flag.ContinueOnError)
	connect := fs.String("connect", "", "dial this host:port instead of the URL's host (Host and TLS name stay the URL's)")
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(os.Stderr, "usage: blite-signal -canary wss://<host>/ws [-connect host:port]")
		return 2
	}
	url := args[0]
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: blite-signal -canary wss://<host>/ws [-connect host:port]")
		return 2
	}
	build, _ := strconv.Atoi(env("BLITE_MIN_COMPATIBLE_BUILD", "0"))
	start := time.Now()
	if err := canary.Run(context.Background(), url, canary.Options{Connect: *connect, Build: build}); err != nil {
		fmt.Fprintln(os.Stderr, "canary failed:", err)
		return 1
	}
	fmt.Printf("canary ok: create, join, signal, resume, signal (%s)\n", time.Since(start).Round(time.Millisecond))
	return 0
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// iceFromEnv picks the ICE provider:
//   - BLITE_TURN_SECRET + BLITE_TURN_URLS (comma-separated): coturn REST credentials
//   - BLITE_ICE_JSON: a fixed JSON array of ICE servers
//   - otherwise: public STUN only
func iceFromEnv() (turn.Provider, error) {
	if secret := os.Getenv("BLITE_TURN_SECRET"); secret != "" {
		urls := strings.Split(env("BLITE_TURN_URLS", ""), ",")
		if urls[0] == "" {
			return nil, errors.New("BLITE_TURN_SECRET set without BLITE_TURN_URLS")
		}
		return turn.Coturn{URLs: urls, Secret: []byte(secret), TTL: 6 * time.Hour, Extra: turn.DefaultSTUN}, nil
	}
	if js := os.Getenv("BLITE_ICE_JSON"); js != "" {
		var s turn.Static
		if err := json.Unmarshal([]byte(js), &s); err != nil {
			return nil, err
		}
		return s, nil
	}
	return turn.DefaultSTUN, nil
}

// siteAliases splits a comma-separated host list.
func siteAliases(v string) []string {
	var out []string
	for _, h := range strings.Split(v, ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			out = append(out, h)
		}
	}
	return out
}
