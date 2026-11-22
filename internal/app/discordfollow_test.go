package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"blite/internal/identity"
	"blite/internal/media"
	"blite/internal/proto"
	"blite/internal/server"
	"blite/internal/session"
)

func waitDiscord(t *testing.T, a *App, ok func(Discord) bool) Discord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		a.mu.Lock()
		d := a.discord
		a.mu.Unlock()
		if ok(d) {
			return d
		}
		if time.Now().After(deadline) {
			t.Fatalf("discord state %+v", d)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDiscordFollow(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	key := identity.Ephemeral()
	unlinked := make(chan bool, 1)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/discord/follow" {
			http.NotFound(w, r)
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		ctx := r.Context()
		send := func(m proto.DiscordMsg) {
			b, _ := json.Marshal(m)
			ws.Write(ctx, websocket.MessageText, b)
		}
		recv := func() proto.DiscordMsg {
			var m proto.DiscordMsg
			_, b, _ := ws.Read(ctx)
			json.Unmarshal(b, &m)
			return m
		}
		send(proto.DiscordMsg{T: proto.TDiscordHello, Challenge: base64.StdEncoding.EncodeToString([]byte("challenge"))})
		auth := recv()
		sig, _ := base64.StdEncoding.DecodeString(auth.Sig)
		if auth.Key != key.Public() || identity.Verify(auth.Key, proto.DiscordFollowPurpose, []byte("challenge"), sig) != nil {
			ws.Close(websocket.StatusPolicyViolation, "bad signature")
			return
		}
		send(proto.DiscordMsg{T: proto.TDiscordState, Linked: true, User: "Alice",
			Voice: &proto.DiscordVoice{GuildID: "g", Guild: "Game Night", ChannelID: "c1", Channel: "gaming"}})
		if m := recv(); m.T == proto.TDiscordUnlink {
			unlinked <- true
			send(proto.DiscordMsg{T: proto.TDiscordState})
		}
		recv()
	}))
	defer fake.Close()

	a := New(session.Config{Server: "ws" + strings.TrimPrefix(fake.URL, "http") + "/ws", Name: "alice", Identity: key, View: media.ViewCount, Log: quiet}, "none")
	defer a.Shutdown()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.RunDiscord(ctx)

	d := waitDiscord(t, a, func(d Discord) bool { return d.Status == "ready" })
	if d.Via != "bot" || d.User != "Alice" || d.ChannelID != "c1" || d.Channel != "gaming" || d.Guild != "Game Night" {
		t.Fatalf("followed state %+v", d)
	}
	a.DiscordUnlink()
	select {
	case <-unlinked:
	case <-time.After(5 * time.Second):
		t.Fatal("unlink not sent")
	}
	waitDiscord(t, a, func(d Discord) bool { return d.Status == "login" && d.ChannelID == "" })
}

func TestDiscordFollowNoBot(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	a := New(session.Config{Server: "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws", Name: "alice", View: media.ViewCount, Log: quiet}, "none")
	defer a.Shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.runFollow(ctx); !errors.Is(err, errNoFollow) {
		t.Fatalf("runFollow without a bot = %v", err)
	}
}
