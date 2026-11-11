package session

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"blite/internal/audio"
	"blite/internal/media"
	"blite/internal/proto"
	"blite/internal/server"
)

// End-to-end encryption: in an encrypted room, members decode
// each other's video and audio in a mesh and through a relay, a late
// joiner gets the keys, and media keeps flowing after a member leaves
// (everyone switches to a new key).
func TestE2EEMeshRelayAndRekey(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	cm := &proto.ClientMeta{AppVersion: "0.1.0", Build: proto.E2EEBuild}
	cfg := func(n string) Config {
		return Config{Server: ws, Name: n, View: media.ViewCount, Client: cm, E2EE: true, Log: quiet}
	}
	ctx := context.Background()

	a, err := Create(ctx, cfg("alice"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if !a.State().Encrypted {
		t.Fatal("room not encrypted")
	}
	a.SetSource(media.SourceTest)
	a.SetAudio(audio.Selection{Mode: "test"})
	b, err := Join(ctx, cfg("bob"), a.sig.Welcome.Room, a.pw)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	b.SetSource(media.SourceTest)
	if !b.State().Encrypted {
		t.Fatal("joiner doesn't know the room is encrypted")
	}
	framesGrow(t, "mesh: bob decodes alice", b, "alice")
	framesGrow(t, "mesh: alice decodes bob", a, "bob")
	waitFor(t, "bob decodes alice's audio", 20*time.Second, func() bool { return member(b, "alice").Audio > 20 })

	// An older build may not join.
	old := cfg("old")
	old.Client = &proto.ClientMeta{AppVersion: "0.1.0", Build: proto.E2EEBuild - 1}
	if _, err := Join(ctx, old, a.sig.Welcome.Room, a.pw); err == nil || !strings.Contains(err.Error(), proto.ErrUpdateRequired) {
		t.Fatalf("old build joined an encrypted room: %v", err)
	}

	// Carol relays; alice and bob have no direct connection.
	c, err := Join(ctx, cfg("carol"), a.sig.Welcome.Room, a.pw)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	framesGrow(t, "mesh: carol decodes alice", c, "alice")
	if err := c.Volunteer(true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice-bob direct connection pruned", 20*time.Second, func() bool {
		return strings.HasPrefix(member(b, "alice").Pair, "via ") && strings.HasPrefix(member(a, "bob").Pair, "via ")
	})
	framesGrow(t, "relay: bob decodes alice via carol", b, "alice")
	framesGrow(t, "relay: alice decodes bob via carol", a, "bob")

	// A late joiner gets everyone's keys.
	d, err := Join(ctx, cfg("dave"), a.sig.Welcome.Room, a.pw)
	if err != nil {
		t.Fatal(err)
	}
	framesGrow(t, "relay: dave decodes alice", d, "alice")
	framesGrow(t, "relay: dave decodes bob", d, "bob")

	// Dave leaves: everyone rekeys, and media keeps flowing.
	d.Close()
	time.Sleep(2 * keySwitchDelayForTest)
	framesGrow(t, "after rekey: bob decodes alice", b, "alice")
	framesGrow(t, "after rekey: alice decodes bob", a, "bob")
	if a.State().Safety != b.State().Safety {
		t.Fatal("safety codes differ")
	}

	// Without alice's key, bob decodes nothing more from her: the media
	// really is encrypted.
	b.mesh.ForgetKeysOf(a.sig.Welcome.PeerID)
	time.Sleep(time.Second)
	before := member(b, "alice").Frames
	time.Sleep(3 * time.Second)
	if got := member(b, "alice").Frames; got > before+2 {
		t.Fatalf("bob decoded %d frames from alice without her key", got-before)
	}
	framesGrow(t, "alice still decodes bob", a, "bob")
}

const keySwitchDelayForTest = time.Second
