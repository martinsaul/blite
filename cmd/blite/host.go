package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"blite/internal/media"
	"blite/internal/room"
	"blite/internal/session"
)

// hostRoom is one entry of the -host rooms file.
type hostRoom struct {
	Code     string `json:"code"`     // fixed room code, e.g. "7K3Q-X9MD"
	Password string `json:"password"` // room password (at least 4 characters)
	Name     string `json:"name"`     // our display name in the room; default "Blite relay"
	// Owners are identity keys (shown in each member's Blite window) of
	// the members who own the room while any of them is in it; otherwise
	// the relay owns it.
	Owners []string `json:"owners,omitempty"`
}

// runHost is the hosted relay: it keeps every room in the
// rooms file open, around the clock, and volunteers as each room's relay.
// The rooms use fixed codes (server host key, BLITE_HOST_KEY), so their
// share links survive restarts of either side.
func runHost(cfg session.Config, log *slog.Logger, file string) {
	b, err := os.ReadFile(file)
	if err != nil {
		fatal(log, "rooms file", err)
	}
	var rooms []hostRoom
	if err := json.Unmarshal(b, &rooms); err != nil {
		fatal(log, "rooms file", err)
	}
	cfg.HostKey = os.Getenv("BLITE_HOST_KEY")
	if cfg.HostKey == "" {
		fatal(log, "host", fmt.Errorf("BLITE_HOST_KEY is not set"))
	}
	cfg.View, cfg.NoPlayback = media.ViewKind("none"), true
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var wg sync.WaitGroup
	for _, r := range rooms {
		code := room.NormalizeCode(r.Code)
		if code == "" || len(r.Password) < room.MinPasswordLen {
			fatal(log, "rooms file", fmt.Errorf("room %q: bad code or password shorter than %d", r.Code, room.MinPasswordLen))
		}
		c := cfg
		c.HostRoom, c.Name, c.HostOwners = code, r.Name, r.Owners
		if c.Name == "" {
			c.Name = "Blite relay"
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			hostLoop(ctx, c, r.Password, log.With("room", room.Display(code)))
		}()
	}
	log.Info("hosting", "rooms", len(rooms))
	if len(rooms) == 0 {
		<-ctx.Done() // nothing configured yet: idle instead of restarting in a loop
	}
	wg.Wait()
}

// hostLoop keeps one room: (re)enter it, volunteer, and start over whenever
// the session ends, with backoff.
func hostLoop(ctx context.Context, cfg session.Config, password string, log *slog.Logger) {
	wait := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		if err := hostOnce(ctx, cfg, password, log); err != nil {
			log.Warn("session ended", "err", err)
		}
		if time.Since(start) > time.Minute {
			wait = time.Second
		}
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
		wait = min(2*wait, time.Minute)
	}
}

func hostOnce(ctx context.Context, cfg session.Config, password string, log *slog.Logger) error {
	s, err := session.Create(ctx, cfg, password)
	if err != nil {
		return err
	}
	defer s.Close()
	log.Info("room open", "link", strings.SplitN(s.State().Link, "#", 2)[0])
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	volunteer := func() {
		if err := s.Volunteer(true); err != nil {
			log.Warn("volunteer", "err", err)
		}
	}
	volunteer()
	last := ""
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.Done():
			return fmt.Errorf("signaling lost")
		case <-tick.C:
			st := s.State()
			queued := false
			for _, e := range st.Relay {
				queued = queued || e.Me
			}
			// Refused while a member's build is too old: try again later.
			if !queued {
				volunteer()
			}
			sum := fmt.Sprintf("members=%d relay=%v", len(st.Members), queued)
			if sum != last {
				log.Info("room", "members", len(st.Members), "queued", queued, "refused", st.RelayRefused)
				last = sum
			}
		}
	}
}
