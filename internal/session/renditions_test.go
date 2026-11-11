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

func peerID(s *Session, name string) string { return member(s, name).ID }

// Renditions end to end. Mesh: alice shares at 1080p, bob's decoder "falls
// behind" and asks for less; alice restarts her source with a 720p
// rendition and sends bob that, the others keep HIGH. Then a relay: carol
// relays, and transcodes alice's 1080p to 720p for bob; a refusing provider
// leaves him where he was.
func TestRenditionsMeshAndRelay(t *testing.T) { testRenditions(t, false) }

// With end-to-end encryption the relay decrypts what it transcodes and
// encrypts its renditions with its own key.
func TestRenditionsEncrypted(t *testing.T) { testRenditions(t, true) }

func testRenditions(t *testing.T, e2ee bool) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	cm := &proto.ClientMeta{AppVersion: "0.1.0", Build: room.DefaultMinRelayBuild}
	if e2ee {
		cm.Build = proto.E2EEBuild
	}
	allow := true
	cfg := func(n string) Config {
		return Config{Server: ws, Name: n, View: media.ViewCount, Client: cm, Log: quiet, E2EE: e2ee,
			CanEncode: func() bool { return allow }}
	}
	ctx := context.Background()

	a, err := Create(ctx, cfg("alice"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.SetResolution(media.Res1080); err != nil {
		t.Fatal(err)
	}
	if err := a.SetSource(media.SourceTest); err != nil {
		t.Fatal(err)
	}
	b, err := Join(ctx, cfg("bob"), a.sig.Welcome.Room, a.pw)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	c, err := Join(ctx, cfg("carol"), a.sig.Welcome.Room, a.pw)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waitFor(t, "bob told he gets alice at 1080p", 20*time.Second, func() bool { return member(b, "alice").Recv == 1080 })
	framesGrow(t, "mesh: bob sees alice", b, "alice")

	// Mesh: alice provides.
	if !b.mesh.DecodeLag(peerID(b, "alice")) {
		t.Fatal("bob did not ask")
	}
	waitFor(t, "bob gets alice's 720p rendition", 15*time.Second, func() bool {
		return member(b, "alice").Recv == 720 && member(a, "bob").Layer == "720p"
	})
	framesGrow(t, "mesh: bob sees alice's rendition", b, "alice")
	if l := member(a, "carol").Layer; l != "high" {
		t.Fatalf("carol gets %q, want high", l)
	}
	framesGrow(t, "mesh: carol still sees alice", c, "alice")

	// Relay: carol transcodes for bob. Alice's own renditions are idle
	// now, but stay for 2 minutes: bob must get carol's.
	if err := c.Volunteer(true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "relay pruned the direct link", 20*time.Second, func() bool {
		return strings.HasPrefix(member(b, "alice").Pair, "via ") && relayName(a) == "carol"
	})
	waitFor(t, "bob told he gets alice at 1080p via carol", 20*time.Second, func() bool { return member(b, "alice").Recv == 1080 })
	framesGrow(t, "relay: bob sees alice", b, "alice")
	b.mesh.ForgetAsk(peerID(b, "alice"))
	if !b.mesh.DecodeLag(peerID(b, "alice")) {
		t.Fatal("bob did not ask the relay")
	}
	waitFor(t, "bob gets carol's 720p transcode of alice", 20*time.Second, func() bool { return member(b, "alice").Recv == 720 })
	framesGrow(t, "relay: bob sees the transcode", b, "alice")

	// A late viewer whose relay refuses: stays on what it gets.
	allow = false
	d, err := Join(ctx, cfg("dave"), a.sig.Welcome.Room, a.pw)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	waitFor(t, "dave gets alice at 1080p", 20*time.Second, func() bool { return member(d, "alice").Recv == 1080 })
	// carol already transcodes 720p of alice, so dave reuses it: no new
	// encoder needed.
	if !d.mesh.DecodeLag(peerID(d, "alice")) {
		t.Fatal("dave did not ask")
	}
	waitFor(t, "dave reuses the running transcode", 20*time.Second, func() bool { return member(d, "alice").Recv == 720 })
	framesGrow(t, "relay: dave sees the transcode", d, "alice")
}

// The "can't see" notice appears only when the provider sends nothing
// because of congestion, not for a viewer's own "off" or a member who
// isn't sharing.
func TestCantSeeNotice(t *testing.T) {
	s := newSession(Config{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, "pw")
	s.members["x"] = &Member{ID: "x", Name: "Xan"}
	for _, c := range []struct {
		height, top int
		congested   bool
		notice      bool
	}{
		{0, 1080, true, true},
		{360, 1080, true, false},
		{0, 1080, false, false},
		{0, 0, false, false},
	} {
		s.gotSending("x", c.height, c.top, c.congested)
		got := s.members["x"].Notice
		if (got != "") != c.notice {
			t.Errorf("%+v: notice %q", c, got)
		}
		if c.notice && got != "You can't see Xan's stream: its quality is too high for your connection or computer right now." {
			t.Errorf("notice text %q", got)
		}
	}
}

// A sharer whose encoder can't do the chosen size says why and steps down:
// a game share one size, a desktop share to a lower frame rate first. The
// test pattern at 4K with x264 runs far below 24 fps on any machine we test
// on; skip where it doesn't.
func TestEncoderTooSlowStepsDown(t *testing.T) {
	for _, tc := range []struct {
		mode media.ContentMode
		res  media.Resolution
		fps  int
		note string
	}{
		{media.ModeGame, media.Res1080, 60, "can't encode 4K fast enough"},
		{media.ModeDesktop, media.Res2160, 20, "can't encode 4K at 30 fps"},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
			ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
			defer ts.Close()
			ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
			a, err := Create(context.Background(), Config{Server: ws, Name: "alice", View: media.ViewCount, Encoder: "x264enc", Log: quiet}, "")
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			if err := a.SetResolution(media.Res2160); err != nil {
				t.Fatal(err)
			}
			if err := a.SetMode(tc.mode); err != nil {
				t.Fatal(err)
			}
			if err := a.SetSource(media.SourceTest); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(40 * time.Second) // warm-up + 2 windows + restart
			for time.Now().Before(deadline) && a.State().ResolutionNote == "" {
				time.Sleep(200 * time.Millisecond)
			}
			st := a.State()
			if st.ResolutionNote == "" {
				t.Skip("this machine encodes 4K fast enough")
			}
			if st.Resolution != string(tc.res) || st.HighFPS != tc.fps || !strings.Contains(st.ResolutionNote, tc.note) {
				t.Fatalf("resolution %q at %d fps, note %q", st.Resolution, st.HighFPS, st.ResolutionNote)
			}
			t.Log(st.ResolutionNote)
		})
	}
}
