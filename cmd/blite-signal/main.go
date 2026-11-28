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
	"net/netip"
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
	cfg, code, msg, err := configFromEnv(os.Getenv)
	if err != nil {
		log.Error(msg, "err", err)
		os.Exit(code)
	}
	if code != 0 {
		log.Error(msg)
		os.Exit(code)
	}
	cfg.Log = log
	s := server.New(cfg)
	if cfg.Standby {
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
	return envFrom(os.Getenv, k, def)
}

func envFrom(getenv func(string) string, k, def string) string {
	if v := getenv(k); v != "" {
		return v
	}
	return def
}

func configFromEnv(getenv func(string) string) (cfg server.Config, exitCode int, errMsg string, err error) {
	ice, err := iceFrom(getenv)
	if err != nil {
		return cfg, 2, "bad ICE config", err
	}
	// Client build gates (see server.Config): BLITE_MIN_COMPATIBLE_BUILD only
	// for breaking protocol changes, BLITE_MIN_RECOMMENDED_BUILD otherwise.
	minCompat, err1 := strconv.Atoi(envFrom(getenv, "BLITE_MIN_COMPATIBLE_BUILD", "0"))
	minRec, err2 := strconv.Atoi(envFrom(getenv, "BLITE_MIN_RECOMMENDED_BUILD", "0"))
	if err1 != nil || err2 != nil {
		return cfg, 1, "BLITE_MIN_COMPATIBLE_BUILD / BLITE_MIN_RECOMMENDED_BUILD must be build numbers", nil
	}
	maxPeers, err := strconv.Atoi(envFrom(getenv, "BLITE_MAX_PEERS", "50"))
	if err != nil || maxPeers < 0 {
		return cfg, 1, "BLITE_MAX_PEERS must be a number (0 = no cap)", nil
	}
	maxRooms, err := strconv.Atoi(envFrom(getenv, "BLITE_MAX_ROOMS", "10000"))
	if err != nil || maxRooms < 0 {
		return cfg, 1, "BLITE_MAX_ROOMS must be a number (0 = no cap)", nil
	}
	maxConns, err := strconv.Atoi(envFrom(getenv, "BLITE_MAX_CONNS", "20000"))
	if err != nil || maxConns < 0 {
		return cfg, 1, "BLITE_MAX_CONNS must be a number (0 = no cap)", nil
	}
	maxConnsPerIP, err := strconv.Atoi(envFrom(getenv, "BLITE_MAX_CONNS_PER_IP", "0"))
	if err != nil || maxConnsPerIP < 0 {
		return cfg, 1, "BLITE_MAX_CONNS_PER_IP must be a number (0 = no cap)", nil
	}
	reportMaxBytes, err := strconv.ParseInt(envFrom(getenv, "BLITE_REPORT_MAX_BYTES", strconv.FormatInt(server.DefaultReportMaxBytes, 10)), 10, 64)
	if err != nil || reportMaxBytes <= 0 {
		return cfg, 1, "BLITE_REPORT_MAX_BYTES must be a positive number of bytes", nil
	}
	proxies, err := parsePrefixes(getenv("BLITE_TRUSTED_PROXIES"))
	if err != nil {
		return cfg, 1, "BLITE_TRUSTED_PROXIES must be comma-separated addresses or networks", err
	}
	var trustProxy bool
	switch envFrom(getenv, "BLITE_TRUST_PROXY", "false") {
	case "true", "1":
		trustProxy = true
	case "false", "0":
	default:
		return cfg, 1, "BLITE_TRUST_PROXY must be true or false", nil
	}
	if trustProxy && len(proxies) == 0 {
		return cfg, 1, "BLITE_TRUST_PROXY requires BLITE_TRUSTED_PROXIES", nil
	}
	standby := envFrom(getenv, "BLITE_STANDBY", "")
	if standby != "" && standby != "0" && standby != "1" {
		return cfg, 1, "BLITE_STANDBY must be 1 or unset", nil
	}
	stateFile := getenv("BLITE_STATE_FILE")
	return server.Config{
		TrustProxy:          trustProxy,
		TrustedProxies:      proxies,
		MaxRooms:            maxRooms,
		MaxConns:            maxConns,
		MaxConnsPerIP:       maxConnsPerIP,
		ICE:                 ice,
		MinCompatibleBuild:  minCompat,
		MinRecommendedBuild: minRec,
		MaxPeers:            maxPeers,
		HostKey:             getenv("BLITE_HOST_KEY"),
		SiteAliases:         siteAliases(getenv("BLITE_SITE_ALIASES")),
		ReportDir:           getenv("BLITE_REPORT_DIR"),
		ReportMaxBytes:      reportMaxBytes,
		DiscordClientID:     getenv("BLITE_DISCORD_CLIENT_ID"),
		DiscordSecret:       getenv("BLITE_DISCORD_SECRET"),
		DiscordPublicKey:    getenv("BLITE_DISCORD_PUBLIC_KEY"),
		DiscordBotToken:     getenv("BLITE_DISCORD_BOT_TOKEN"),
		DiscordLinksFile:    getenv("BLITE_DISCORD_LINKS_FILE"),
		DiscordLinkRedirect: getenv("BLITE_DISCORD_LINK_REDIRECT"),
		UpdateURL:           getenv("BLITE_UPDATE_URL"),
		UpdateChannel:       envFrom(getenv, "BLITE_DOWNLOAD_CHANNEL", "beta"),
		AndroidManifest:     androidManifest(envFrom(getenv, "BLITE_ANDROID_MANIFEST", "https://bellion.app/download/android.json")),
		StateFile:           stateFile,
		DeferState:          server.ShouldDefer(stateFile, getenv("BLITE_COLOR")),
		Standby:             standby == "1",
		FailoverSecret:      getenv("BLITE_FAILOVER_SECRET"),
		ActiveFile:          getenv("BLITE_ACTIVE_FILE"),
	}, 0, "", nil
}

// iceFrom picks the ICE provider:
//   - BLITE_TURN_SECRET + BLITE_TURN_URLS (comma-separated): coturn REST credentials
//   - BLITE_ICE_JSON: a fixed JSON array of ICE servers
//   - otherwise: public STUN only
func iceFrom(getenv func(string) string) (turn.Provider, error) {
	if secret := getenv("BLITE_TURN_SECRET"); secret != "" {
		urls := strings.Split(envFrom(getenv, "BLITE_TURN_URLS", ""), ",")
		if urls[0] == "" {
			return nil, errors.New("BLITE_TURN_SECRET set without BLITE_TURN_URLS")
		}
		return turn.Coturn{URLs: urls, Secret: []byte(secret), TTL: 6 * time.Hour, Extra: turn.DefaultSTUN}, nil
	}
	if js := getenv("BLITE_ICE_JSON"); js != "" {
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

func androidManifest(v string) string {
	if v == "off" {
		return ""
	}
	return v
}

// parsePrefixes reads a comma-separated list of addresses (one host each) and
// CIDR networks.
func parsePrefixes(v string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.Split(v, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if p, err := netip.ParsePrefix(f); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(f)
		if err != nil {
			return nil, fmt.Errorf("%q: not an address or network", f)
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}
