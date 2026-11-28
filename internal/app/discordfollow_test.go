package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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

func stubBrowser(t *testing.T) *browserStub {
	t.Helper()
	s := &browserStub{ch: make(chan string, 8)}
	old := OpenBrowser
	OpenBrowser = func(u string) error {
		s.mu.Lock()
		s.urls = append(s.urls, u)
		s.mu.Unlock()
		s.ch <- u
		return nil
	}
	t.Cleanup(func() { OpenBrowser = old })
	return s
}

type browserStub struct {
	mu   sync.Mutex
	urls []string
	ch   chan string
}

func (s *browserStub) list() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.urls))
	copy(out, s.urls)
	return out
}

func TestValidDiscordLinkURL(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"https://discord.com/oauth2/authorize?client_id=42&response_type=code&scope=identify&state=abc", true},
		{"https://www.discord.com/oauth2/authorize?client_id=42", true},
		{"https://discord.com:443/oauth2/authorize?client_id=42", true},
		{"https://www.discord.com:443/oauth2/authorize", true},
		{"https://DISCORD.COM/oauth2/authorize?client_id=42", true},
		{"HTTPS://WWW.Discord.Com/oauth2/authorize?state=xyz", true},
		{"https://discord.com/oauth2/authorize/", true},

		{"http://discord.com/oauth2/authorize?client_id=42", false},
		{"file:///etc/passwd", false},
		{"file:///C:/Windows/System32/calc.exe", false},
		{`\\host\share`, false},
		{`\\host\share\payload.exe`, false},
		{"https://discord.com.evil.net/oauth2/authorize", false},
		{"https://evil.net/?x=discord.com", false},
		{"https://evil.net/oauth2/authorize?x=discord.com", false},
		{"javascript:alert(1)", false},
		{"https://user@discord.com/oauth2/authorize", false},
		{"https://user:pass@discord.com/oauth2/authorize", false},
		{"https://@discord.com/oauth2/authorize", false},
		{"https://discord.com:8443/oauth2/authorize", false},
		{"https://discord.com:/oauth2/authorize", false},
		{"https://discord.com/oauth2/token", false},
		{"https://discord.com/oauth2/authorize-evil", false},
		{"https://discord.com/oauth2/authorize/../channels", false},
		{"https://discord.com/oauth2/authorize%2f..%2fchannels", false},
		{`https://discord.com/oauth2/authorize\..\..\channels`, false},
		{"//discord.com/oauth2/authorize", false},
		{"/oauth2/authorize", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := validDiscordLinkURL(tc.url); got != tc.want {
			t.Errorf("validDiscordLinkURL(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}

func TestDiscordFollow(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	browser := stubBrowser(t)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	key := identity.Ephemeral()

	var probes, sockets atomic.Int32
	unlinked := make(chan bool, 1)
	closed := make(chan struct{}, 1)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/discord/follow" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Upgrade") == "" {
			probes.Add(1)
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		sockets.Add(1)
		defer ws.CloseNow()
		defer func() {
			select {
			case closed <- struct{}{}:
			default:
			}
		}()
		ctx := r.Context()
		send := func(m proto.DiscordMsg) {
			b, _ := json.Marshal(m)
			ws.Write(ctx, websocket.MessageText, b)
		}
		recv := func() (proto.DiscordMsg, error) {
			var m proto.DiscordMsg
			_, b, err := ws.Read(ctx)
			if err == nil {
				err = json.Unmarshal(b, &m)
			}
			return m, err
		}
		send(proto.DiscordMsg{T: proto.TDiscordHello, Challenge: base64.StdEncoding.EncodeToString([]byte("challenge"))})
		auth, err := recv()
		if err != nil {
			return
		}
		sig, _ := base64.StdEncoding.DecodeString(auth.Sig)
		if auth.Key != key.Public() || identity.Verify(auth.Key, proto.DiscordFollowPurpose, []byte("challenge"), sig) != nil {
			ws.Close(websocket.StatusPolicyViolation, "bad signature")
			return
		}
		// Pre-linked install: first state already says Linked.
		send(proto.DiscordMsg{T: proto.TDiscordState, Linked: true, User: "Alice",
			Voice: &proto.DiscordVoice{GuildID: "g", Guild: "Game Night", ChannelID: "c1", Channel: "gaming"}})
		// Unsolicited LinkURL without a click must be ignored.
		send(proto.DiscordMsg{T: proto.TDiscordState, LinkURL: "https://discord.com/oauth2/authorize?client_id=42&state=unsolicited"})
		if m, err := recv(); err == nil && m.T == proto.TDiscordUnlink {
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

	// Not linked at start: plain HTTP probe offers Connect Discord without opening a socket.
	d := waitDiscord(t, a, func(d Discord) bool { return d.Status == "login" })
	if d.Via != "bot" {
		t.Fatalf("probed state %+v", d)
	}
	time.Sleep(50 * time.Millisecond)
	if got := sockets.Load(); got != 0 {
		t.Fatalf("sockets opened before DiscordLogin = %d, want 0", got)
	}
	if probes.Load() == 0 {
		t.Fatal("expected HTTP probe at start")
	}

	// Clicking Connect Discord opens the socket; already-linked install sets flag without opening browser.
	a.DiscordLogin()
	d = waitDiscord(t, a, func(d Discord) bool { return d.Status == "ready" })
	if d.Via != "bot" || d.User != "Alice" || d.ChannelID != "c1" || d.Channel != "gaming" || d.Guild != "Game Night" {
		t.Fatalf("followed state %+v", d)
	}
	if !discordLinked() {
		t.Fatal("discordLinked flag not set when server reported Linked")
	}
	if got := browser.list(); len(got) != 0 {
		t.Fatalf("browser opened unexpectedly: %v", got)
	}

	a.DiscordUnlink()
	select {
	case <-unlinked:
	case <-time.After(5 * time.Second):
		t.Fatal("unlink not sent")
	}
	waitDiscord(t, a, func(d Discord) bool { return d.Status == "login" && d.ChannelID == "" })
	if discordLinked() {
		t.Fatal("discordLinked flag not cleared on unlink")
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("socket not closed after unlink")
	}
	time.Sleep(50 * time.Millisecond)
	if got := sockets.Load(); got != 1 {
		t.Fatalf("sockets opened = %d, want 1 (must not reconnect after unlink)", got)
	}
}

func TestDiscordFollowLinkURLConsentAndPersistence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	browser := stubBrowser(t)

	var logBuf syncBuffer
	log := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	key := identity.Ephemeral()

	const goodURL = "https://discord.com/oauth2/authorize?client_id=42&response_type=code&scope=identify&state=ok1"

	var (
		sockets     atomic.Int32
		linkedOnSrv atomic.Bool
	)
	linkDone := make(chan chan proto.DiscordMsg, 2)
	srvClosed := make(chan struct{}, 4)

	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/discord/follow" {
			http.NotFound(w, r)
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		n := sockets.Add(1)
		defer ws.CloseNow()
		defer func() { srvClosed <- struct{}{} }()
		ctx := r.Context()
		send := func(m proto.DiscordMsg) {
			b, _ := json.Marshal(m)
			ws.Write(ctx, websocket.MessageText, b)
		}
		recv := func() (proto.DiscordMsg, error) {
			var m proto.DiscordMsg
			_, b, err := ws.Read(ctx)
			if err == nil {
				err = json.Unmarshal(b, &m)
			}
			return m, err
		}
		send(proto.DiscordMsg{T: proto.TDiscordHello, Challenge: base64.StdEncoding.EncodeToString([]byte("challenge"))})
		if _, err := recv(); err != nil {
			return
		}

		if n == 1 {
			// Unsolicited LinkURL before anything else (no click request sent yet).
			send(proto.DiscordMsg{T: proto.TDiscordState, LinkURL: "https://discord.com/oauth2/authorize?state=before-link"})
			send(proto.DiscordMsg{T: proto.TDiscordState, Linked: false})

			// Click 1 -> server sends a bad-host LinkURL, then a second good LinkURL without a new click.
			m, err := recv()
			if err != nil || m.T != proto.TDiscordLink {
				return
			}
			send(proto.DiscordMsg{T: proto.TDiscordState, LinkURL: "https://evil.net/oauth2/authorize?secret=never-log-me"})
			send(proto.DiscordMsg{T: proto.TDiscordState, LinkURL: "https://discord.com/oauth2/authorize?state=second-without-click"})

			// Click 2 -> server sends a good LinkURL, then a duplicate good LinkURL on the same click.
			m, err = recv()
			if err != nil || m.T != proto.TDiscordLink {
				return
			}
			send(proto.DiscordMsg{T: proto.TDiscordState, LinkURL: goodURL})
			send(proto.DiscordMsg{T: proto.TDiscordState, LinkURL: "https://discord.com/oauth2/authorize?state=dup-same-click"})

			// Wait for the test to trigger OAuth completion.
			push := make(chan proto.DiscordMsg, 2)
			linkDone <- push
			for msg := range push {
				send(msg)
			}
			recv()
			return
		}

		// Second App instance starts with discordLinked() == true on disk.
		send(proto.DiscordMsg{T: proto.TDiscordState, Linked: linkedOnSrv.Load(), User: "Bob"})
		// Then server reports not linked: client must clear flag and close socket.
		push := make(chan proto.DiscordMsg, 2)
		linkDone <- push
		for msg := range push {
			send(msg)
		}
		recv()
	}))
	defer fake.Close()

	wsURL := "ws" + strings.TrimPrefix(fake.URL, "http") + "/ws"
	a := New(session.Config{Server: wsURL, Name: "bob", Identity: key, View: media.ViewCount, Log: log}, "none")
	ctx1, cancel1 := context.WithCancel(context.Background())
	go a.RunDiscord(ctx1)

	waitDiscord(t, a, func(d Discord) bool { return d.Status == "login" })
	if sockets.Load() != 0 {
		t.Fatalf("socket opened at start when unlinked: %d", sockets.Load())
	}

	// Click 1: receives bad-host URL + second URL without click; neither opens.
	a.DiscordLogin()
	waitDiscord(t, a, func(d Discord) bool { return d.Status == "linking" })
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logBuf.String(), "evil.net") {
		if time.Now().After(deadline) {
			t.Fatalf("expected warning for evil.net, got logs:\n%s", logBuf.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if strings.Contains(logBuf.String(), "never-log-me") {
		t.Fatalf("warning log leaked URL query contents:\n%s", logBuf.String())
	}
	if got := browser.list(); len(got) != 0 {
		t.Fatalf("bad or unrequested LinkURL opened: %v", got)
	}

	// Click 2: receives good URL (opened once) + duplicate URL (ignored).
	a.DiscordLogin()
	select {
	case got := <-browser.ch:
		if got != goodURL {
			t.Fatalf("opened %q, want %q", got, goodURL)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("good LinkURL was not opened")
	}
	var push1 chan proto.DiscordMsg
	select {
	case push1 = <-linkDone:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not reach post-link wait")
	}
	if got := browser.list(); len(got) != 1 {
		t.Fatalf("expected exactly 1 browser open, got %v", got)
	}

	// Browser OAuth completes: server reports Linked -> flag is set.
	linkedOnSrv.Store(true)
	push1 <- proto.DiscordMsg{T: proto.TDiscordState, Linked: true, User: "Bob"}
	waitDiscord(t, a, func(d Discord) bool { return d.Status == "ready" && d.User == "Bob" })
	if !discordLinked() {
		t.Fatal("discordLinked flag should be true after Linked")
	}
	cancel1()
	close(push1)
	a.Shutdown()
	<-srvClosed

	// Next launch with the persisted flag: opens the follow socket at start without DiscordLogin.
	b := New(session.Config{Server: wsURL, Name: "bob", Identity: key, View: media.ViewCount, Log: log}, "none")
	defer b.Shutdown()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go b.RunDiscord(ctx2)

	waitDiscord(t, b, func(d Discord) bool { return d.Status == "ready" && d.User == "Bob" })
	var push2 chan proto.DiscordMsg
	select {
	case push2 = <-linkDone:
	case <-time.After(5 * time.Second):
		t.Fatal("second instance did not connect at start")
	}

	// Server reports not linked -> flag cleared and socket closed.
	push2 <- proto.DiscordMsg{T: proto.TDiscordState, Linked: false}
	close(push2)
	waitDiscord(t, b, func(d Discord) bool { return d.Status == "login" })
	if discordLinked() {
		t.Fatal("discordLinked flag not cleared when server reported not linked")
	}
	select {
	case <-srvClosed:
	case <-time.After(5 * time.Second):
		t.Fatal("socket did not close after server reported not linked")
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestDiscordFollowNoBot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
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
