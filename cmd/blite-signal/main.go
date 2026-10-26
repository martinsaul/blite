// Command blite-signal is Blite's signaling server: it introduces peers in a room
// and relays their SDP/ICE. It never carries media.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

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
	s := server.New(server.Config{
		TrustProxy:          env("BLITE_TRUST_PROXY", "false") == "true",
		ICE:                 ice,
		MinCompatibleBuild:  minCompat,
		MinRecommendedBuild: minRec,
		HostKey:             os.Getenv("BLITE_HOST_KEY"),
		ReportDir:           os.Getenv("BLITE_REPORT_DIR"),
		Log:                 log,
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go s.Run(ctx)

	hs := &http.Server{Addr: addr, Handler: s, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(sctx)
	}()
	log.Info("listening", "addr", addr)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
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
