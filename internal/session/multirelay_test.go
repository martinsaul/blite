package session

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"blite/internal/media"
	"blite/internal/proto"
	"blite/internal/server"
)

// Several relays end to end (builds from proto.MultiRelayBuild): one relay
// serves until the room outgrows it, then the second volunteer relays too
// and new members home on it. Sharers on different relays see each other
// over the trunk, at most two hops; when the first relay withdraws,
// everyone moves to the second without the streams stopping for good.
func TestMultiRelay(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	cm := &proto.ClientMeta{AppVersion: "0.1.0", Build: proto.MultiRelayBuild}
	cfg := func(n string) Config {
		return Config{Server: ws, Name: n, View: media.ViewCount, Client: cm, Log: testLog(t, n, quiet)}
	}
	ctx := context.Background()

	r1, err := Create(ctx, cfg("r1"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer r1.Close()
	if err := r1.Volunteer(true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "r1 relays", 5*time.Second, func() bool { return relayName(r1) == "r1" })
	all := map[string]*Session{"r1": r1}
	join := func(name string) *Session {
		t.Helper()
		s, err := Join(ctx, cfg(name), r1.sig.Welcome.Room, r1.pw)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Close)
		all[name] = s
		return s
	}
	r2 := join("r2")
	if err := r2.Volunteer(true); err != nil {
		t.Fatal(err)
	}
	alice, bob := join("alice"), join("bob")
	alice.SetSource(media.SourceTest)
	framesGrow(t, "bob sees alice via r1", bob, "alice")
	if st := alice.State(); len(st.Relay) != 1 || len(st.RelayWaiting) != 1 {
		t.Fatalf("one relay while it has room: %+v / %+v", st.Relay, st.RelayWaiting)
	}

	// Outgrow r1 (4 children by default): r2 relays too.
	join("carol")
	dave := join("dave")
	waitFor(t, "two relays", 5*time.Second, func() bool { return len(alice.State().Relay) == 2 })
	erin, frank := join("erin"), join("frank")
	waitFor(t, "erin homed on r2", 5*time.Second, func() bool { return erin.mesh.Home(erin.mesh.Me()) == r2.mesh.Me() })
	if h := bob.mesh.Home(bob.mesh.Me()); h != r1.mesh.Me() {
		t.Fatal("bob moved off r1")
	}
	erin.SetSource(media.SourceTest)

	framesGrow(t, "frank (r2) sees alice (r1) over the trunk", frank, "alice")
	framesGrow(t, "bob (r1) sees erin (r2) over the trunk", bob, "erin")
	framesGrow(t, "r2 sees alice", r2, "alice")
	framesGrow(t, "r1 sees erin", r1, "erin")
	framesGrow(t, "dave sees alice", dave, "alice")
	waitFor(t, "r2 reaches alice via r1 only", 15*time.Second, func() bool {
		return member(r2, "alice").Pair == "via r1" && member(frank, "alice").Pair == "via r2"
	})
	if alice.State().Safety != frank.State().Safety {
		t.Fatal("safety codes differ")
	}

	// r1 withdraws: everyone moves to r2.
	if err := r1.Volunteer(false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "r2 alone relays", 5*time.Second, func() bool {
		st := bob.State()
		return len(st.Relay) == 1 && st.Relay[0].Name == "r2"
	})
	waitFor(t, "bob moves to r2", 20*time.Second, func() bool { return member(bob, "erin").Pair == "via r2" })
	framesGrow(t, "after the move: bob sees erin", bob, "erin")
	framesGrow(t, "after the move: frank sees alice", frank, "alice")
	framesGrow(t, "after the move: r1 sees erin", r1, "erin")
}

// testLog logs to $BLITE_TEST_LOG/<name>.log if set, else nowhere.
func testLog(t *testing.T, name string, quiet *slog.Logger) *slog.Logger {
	dir := os.Getenv("BLITE_TEST_LOG")
	if dir == "" {
		return quiet
	}
	f, err := os.Create(filepath.Join(dir, name+".log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// A room mixing builds before and from proto.MultiRelayBuild stays on one
// relay, paused above proto.RelayMaxPeers, and connections between the two
// carry the legacy slot count: a newer relay serves an older viewer and
// the other way round.
func TestMixedBuildRelay(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	newer := &proto.ClientMeta{AppVersion: "0.1.0", Build: proto.MultiRelayBuild}
	older := &proto.ClientMeta{AppVersion: "0.1.0", Build: proto.MultiRelayBuild - 1}
	cfg := func(n string, cm *proto.ClientMeta) Config {
		return Config{Server: ws, Name: n, View: media.ViewCount, Client: cm, Log: testLog(t, n, quiet)}
	}
	ctx := context.Background()
	a, err := Create(ctx, cfg("alice", newer), "")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.SetSource(media.SourceTest)
	join := func(name string, cm *proto.ClientMeta) *Session {
		t.Helper()
		s, err := Join(ctx, cfg(name, cm), a.sig.Welcome.Room, a.pw)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Close)
		return s
	}
	old := join("old", older)
	old.SetSource(media.SourceTest)
	rel := join("rel", newer)
	v := join("v", newer)
	if err := rel.Volunteer(true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "rel relays for everyone", 5*time.Second, func() bool {
		return relayName(a) == "rel" && relayName(old) == "rel" && relayName(v) == "rel"
	})
	framesGrow(t, "old sees alice via the newer relay", old, "alice")
	framesGrow(t, "v sees old via the relay", v, "old")
	if n := rel.mesh.SlotCount(old.mesh.Me()); n != proto.RelayMaxPeers-1 {
		t.Fatalf("slots toward the older build: %d", n)
	}

	// The older member relays instead.
	if err := old.Volunteer(true); err != nil {
		t.Fatal(err)
	}
	if err := rel.Volunteer(false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "old relays", 5*time.Second, func() bool { return relayName(a) == "old" && relayName(v) == "old" })
	waitFor(t, "v reaches alice via old", 20*time.Second, func() bool { return member(v, "alice").Pair == "via old" })
	framesGrow(t, "v sees alice via the older relay", v, "alice")

	// Two more: above RelayMaxPeers with an older member, relay mode pauses.
	join("x", newer)
	join("y", newer)
	waitFor(t, "paused", 5*time.Second, func() bool { return a.State().RelayPaused && old.State().RelayPaused })
	framesGrow(t, "mesh while paused: v sees alice", v, "alice")
}

// More members than slots: a relay connection carries 8 origins, so
// sharers that weren't given one take the slot of a quiet member when they
// start; everyone sees them, and the members without a slot still show as
// connected.
func TestRelaySlotOverflow(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	cm := &proto.ClientMeta{AppVersion: "0.1.0", Build: proto.MultiRelayBuild}
	cfg := func(n string) Config {
		return Config{Server: ws, Name: n, View: media.ViewCount, Client: cm, Log: testLog(t, n, quiet)}
	}
	ctx := context.Background()
	rc := cfg("relay")
	rc.UploadKbps = 200_000 // declared: room for everyone, and paced
	r, err := Create(ctx, rc, "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Volunteer(true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "relay", 5*time.Second, func() bool { return relayName(r) == "relay" })
	var ss []*Session
	for i := range 10 {
		s, err := Join(ctx, cfg("m"+string(rune('a'+i))), r.sig.Welcome.Room, r.pw)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Close)
		ss = append(ss, s)
	}
	waitFor(t, "everyone connected", 20*time.Second, func() bool {
		for _, s := range ss {
			for _, m := range s.State().Members {
				if m.State != "connected" {
					return false
				}
			}
		}
		return true
	})
	if n := r.mesh.SlotCount(ss[0].mesh.Me()); n != 8 {
		t.Fatalf("slots: %d", n)
	}
	// Share from the two members whose IDs sort last: with 9 origins for
	// 8 slots, at least one of them had none.
	byID := slices.Clone(ss)
	slices.SortFunc(byID, func(a, b *Session) int { return strings.Compare(a.mesh.Me(), b.mesh.Me()) })
	s1, s2 := byID[8], byID[9]
	s1.SetSource(media.SourceTest)
	s2.SetSource(media.SourceTest)
	for _, v := range byID[:3] {
		framesGrow(t, "a viewer sees the first sharer", v, s1.cfg.Name)
		framesGrow(t, "a viewer sees the second sharer", v, s2.cfg.Name)
	}
	framesGrow(t, "one sharer sees the other", s1, s2.cfg.Name)
}
