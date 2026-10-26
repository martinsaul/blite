package session

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"blite/internal/media"
	"blite/internal/proto"
	"blite/internal/room"
	"blite/internal/server"
)

func relayName(s *Session) string {
	if q := s.State().Relay; len(q) > 0 {
		return q[0].Name
	}
	return ""
}

// framesGrow waits until viewer v has decoded more frames of name than now.
func framesGrow(t *testing.T, what string, v *Session, name string) {
	t.Helper()
	before := member(v, name).Frames
	waitFor(t, what, 20*time.Second, func() bool { return member(v, name).Frames > before+20 })
}

// Relay mode end to end: alice and bob share, carol volunteers as relay; dave
// joins, queues, and takes over when carol withdraws; then he withdraws too. Alice and bob keep seeing each other throughout: via carol while
// she relays (with no direct connection between them), directly afterwards.
func TestRelayModeHandsOffAndBack(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	cm := &proto.ClientMeta{AppVersion: "0.1.0", Build: room.DefaultMinRelayBuild}
	cfg := func(n string) Config {
		return Config{Server: ws, Name: n, View: media.ViewCount, Client: cm, Log: quiet}
	}
	ctx := context.Background()

	a, err := Create(ctx, cfg("alice"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.SetSource(media.SourceTest)
	b, err := Join(ctx, cfg("bob"), a.sig.Welcome.Room, a.pw)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	b.SetSource(media.SourceTest)
	c, err := Join(ctx, cfg("carol"), a.sig.Welcome.Room, a.pw)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	framesGrow(t, "mesh: bob sees alice", b, "alice")
	framesGrow(t, "mesh: carol sees bob", c, "bob")

	if err := c.Volunteer(true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "everyone sees carol as relay", 5*time.Second, func() bool {
		return relayName(a) == "carol" && relayName(b) == "carol" && relayName(c) == "carol"
	})
	waitFor(t, "alice-bob direct connection pruned", 15*time.Second, func() bool {
		return strings.HasPrefix(member(b, "alice").Pair, "via ") && strings.HasPrefix(member(a, "bob").Pair, "via ")
	})
	framesGrow(t, "relay: bob sees alice via carol", b, "alice")
	framesGrow(t, "relay: alice sees bob via carol", a, "bob")
	framesGrow(t, "relay: carol sees alice", c, "alice")

	// A late joiner connects to the relay only and sees both sharers.
	d, err := Join(ctx, cfg("dave"), a.sig.Welcome.Room, a.pw)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	framesGrow(t, "relay: dave sees alice", d, "alice")
	framesGrow(t, "relay: dave sees bob", d, "bob")
	if st := d.State(); !strings.HasPrefix(member(d, "alice").Pair, "via ") || len(st.Relay) != 1 {
		t.Fatalf("dave: alice pair %q, relay %v", member(d, "alice").Pair, st.Relay)
	}
	if a.State().Safety != d.State().Safety {
		t.Fatal("safety codes differ in relay mode")
	}

	// Dave queues behind carol; carol withdraws: dave takes over.
	if err := d.Volunteer(true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "dave queued", 5*time.Second, func() bool { return len(a.State().Relay) == 2 })
	if err := c.Volunteer(false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "dave relays", 5*time.Second, func() bool { return relayName(a) == "dave" && relayName(b) == "dave" })
	waitFor(t, "alice's connection to carol pruned", 15*time.Second, func() bool {
		return member(a, "bob").Pair == "via dave" && member(a, "carol").Pair == "via dave"
	})
	framesGrow(t, "handover: bob sees alice via dave", b, "alice")
	framesGrow(t, "handover: carol sees bob via dave", c, "bob")

	// Dave withdraws with nobody queued: back to full mesh.
	if err := d.Volunteer(false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "back to mesh", 5*time.Second, func() bool { return relayName(b) == "" && relayName(d) == "" })
	waitFor(t, "bob connects to alice directly", 15*time.Second, func() bool {
		p := member(b, "alice").Pair
		return p != "" && !strings.HasPrefix(p, "via ")
	})
	framesGrow(t, "mesh again: bob sees alice", b, "alice")
	framesGrow(t, "mesh again: dave sees bob", d, "bob")
	framesGrow(t, "mesh again: carol sees alice", c, "alice")
}
