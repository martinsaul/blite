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
	"blite/internal/server"
	"blite/internal/sigclient"
)

// Ownership end to end: the creator owns the room, hands it over, an owner
// whose connection drops loses it at once (and not back on resume), and
// the owner bans someone, who is told why and can't rejoin.
func TestOwnerTransferHandoffBan(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	cfg := func(n string) Config { return Config{Server: ws, Name: n, View: "none", Log: quiet} }
	ctx := context.Background()

	a, err := Create(ctx, cfg("alice"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
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
	waitFor(t, "everyone sees alice as owner", 10*time.Second, func() bool {
		return a.State().IsOwner && member(b, "alice").Owner && member(c, "alice").Owner &&
			member(a, "carol").State == "connected" && member(a, "bob").State == "connected"
	})
	if err := b.Kick(peerID(b, "carol"), false); err == nil {
		t.Fatal("bob kicked without owning the room")
	}

	if err := a.Transfer(peerID(a, "carol")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "carol owns", 5*time.Second, func() bool { return c.State().IsOwner && member(a, "carol").Owner })

	// Carol's signaling drops: alice (joined first) owns at once; carol
	// resumes as a member.
	c.sig.Drop()
	waitFor(t, "alice owns after carol dropped", 5*time.Second, func() bool { return a.State().IsOwner })
	waitFor(t, "carol back, not owner", 15*time.Second, func() bool {
		st := c.State()
		return !st.Reconnecting && !st.IsOwner && member(c, "alice").Owner
	})

	// Alice bans bob.
	if err := a.Kick(peerID(a, "bob"), true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-b.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("bob's session didn't end")
	}
	if r := b.EndReason(); !strings.Contains(r, "alice removed you") || !strings.Contains(r, "banned") {
		t.Fatalf("bob told %q", r)
	}
	waitFor(t, "bob gone for carol", 5*time.Second, func() bool { return member(c, "bob").ID == "" })
	bobKey := b.cfg.Identity
	again := cfg("bob")
	again.Identity = bobKey
	if _, err := Join(ctx, again, a.sig.Welcome.Room, a.pw); !sigclient.IsServerError(err, proto.ErrBanned) {
		t.Fatalf("banned bob rejoined: %v", err)
	}
	_ = media.ViewCount
}

// Display names: a rename reaches the others; everyone sees the same tag
// for a member, derived from its identity key.
func TestRenameAndTag(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	cfg := func(n string) Config { return Config{Server: ws, Name: n, View: "none", Log: quiet} }
	ctx := context.Background()
	a, err := Create(ctx, cfg("alice"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Join(ctx, cfg("bob"), a.sig.Welcome.Room, a.pw)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	waitFor(t, "connected", 10*time.Second, func() bool { return member(a, "bob").State == "connected" })
	if got, want := member(a, "bob").Tag, b.State().Tag; got == "" || got != want {
		t.Fatalf("alice sees bob's tag %q, bob's own %q", got, want)
	}
	if err := b.Rename("Robert#9999"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice sees the new name", 5*time.Second, func() bool { return member(a, "Robert9999").ID != "" })
	if b.State().Name != "Robert9999" {
		t.Fatalf("bob's own name %q", b.State().Name)
	}
}
