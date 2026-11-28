package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"blite/internal/identity"
	"blite/internal/proto"
)

// followClient is an app's follow socket.
type followClient struct {
	t  *testing.T
	ws *websocket.Conn
}

func dialFollow(t *testing.T, base string, k identity.Key) *followClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/v1/discord/follow", nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &followClient{t: t, ws: ws}
	t.Cleanup(func() { ws.CloseNow() })
	hello := c.next()
	ch, _ := base64.StdEncoding.DecodeString(hello.Challenge)
	if hello.T != proto.TDiscordHello || len(ch) != 32 {
		t.Fatalf("hello %+v", hello)
	}
	sig := k.Sign(proto.DiscordFollowPurpose, ch)
	c.send(proto.DiscordMsg{T: proto.TDiscordAuth, Key: k.Public(), Sig: base64.StdEncoding.EncodeToString(sig)})
	return c
}

func (c *followClient) send(m proto.DiscordMsg) {
	b, _ := json.Marshal(m)
	if err := c.ws.Write(context.Background(), websocket.MessageText, b); err != nil {
		c.t.Fatal(err)
	}
}

func (c *followClient) next() proto.DiscordMsg {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := c.ws.Read(ctx)
	if err != nil {
		c.t.Fatal(err)
	}
	var m proto.DiscordMsg
	json.Unmarshal(b, &m)
	return m
}

// until reads states until one matches (the bot's first lookup may send
// the same state again).
func (c *followClient) until(ok func(proto.DiscordMsg) bool) proto.DiscordMsg {
	c.t.Helper()
	for range 5 {
		if m := c.next(); ok(m) {
			return m
		}
	}
	c.t.Fatal("no matching state")
	return proto.DiscordMsg{}
}

func TestDiscordLinkAndFollow(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			r.ParseForm()
			if r.PostForm.Get("code") != "c0de" || r.PostForm.Get("redirect_uri") != "https://example.test/v1/discord/linked" {
				http.Error(w, "bad", 400)
				return
			}
			w.Write([]byte(`{"access_token":"at"}`))
		case "/users/@me":
			if r.Header.Get("Authorization") != "Bearer at" {
				http.Error(w, "no", 401)
				return
			}
			w.Write([]byte(`{"id":"555","username":"alice","global_name":"Alice"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()
	oldTok, oldAPI := discordTokenURL, discordAPI
	discordTokenURL, discordAPI = fake.URL+"/token", fake.URL
	defer func() { discordTokenURL, discordAPI = oldTok, oldAPI }()

	links := filepath.Join(t.TempDir(), "links.json")
	s := New(Config{DiscordClientID: "42", DiscordSecret: "shh", DiscordBotToken: "bot",
		DiscordLinksFile: links, DiscordLinkRedirect: "https://example.test/v1/discord/linked"})
	s.discord.bot.APIURL = fake.URL // no guilds: nothing to look up
	srv := httptest.NewServer(s)
	defer srv.Close()

	k := identity.Ephemeral()
	c := dialFollow(t, srv.URL, k)
	if st := c.next(); st.T != proto.TDiscordState || st.Linked {
		t.Fatalf("unlinked state %+v", st)
	}
	if s.discord.active("555") {
		t.Fatal("active before linking")
	}

	c.send(proto.DiscordMsg{T: proto.TDiscordLink})
	st := c.next()
	u, err := url.Parse(st.LinkURL)
	if err != nil || u.Host != "discord.com" || u.Query().Get("scope") != "identify" || u.Query().Get("client_id") != "42" {
		t.Fatalf("link url %q", st.LinkURL)
	}
	state := u.Query().Get("state")

	// Discord sends the browser back.
	res, err := http.Get(srv.URL + "/v1/discord/linked?code=c0de&state=" + url.QueryEscape(state))
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("callback: %v %v", res.Status, err)
	}
	if st := c.next(); !st.Linked || st.User != "Alice" {
		t.Fatalf("linked state %+v", st)
	}
	if !s.discord.active("555") {
		t.Fatal("not active while the app is connected")
	}
	if b, _ := os.ReadFile(links); !strings.Contains(string(b), `"555"`) {
		t.Fatalf("links file %s", b)
	}
	// The same state can't link twice.
	if res, _ := http.Get(srv.URL + "/v1/discord/linked?code=c0de&state=" + url.QueryEscape(state)); res.StatusCode != 400 {
		t.Fatalf("state reused: %s", res.Status)
	}

	// The app goes away: no longer followed.
	c.ws.Close(websocket.StatusNormalClosure, "")
	deadline := time.Now().Add(5 * time.Second)
	for s.discord.active("555") {
		if time.Now().After(deadline) {
			t.Fatal("still active after the app left")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Next start: linked from the file (a new server, as after a deploy).
	s2 := New(Config{DiscordClientID: "42", DiscordSecret: "shh", DiscordBotToken: "bot", DiscordLinksFile: links})
	srv2 := httptest.NewServer(s2)
	defer srv2.Close()
	c2 := dialFollow(t, srv2.URL, k)
	if st := c2.next(); !st.Linked {
		t.Fatalf("link not kept: %+v", st)
	}
	c2.send(proto.DiscordMsg{T: proto.TDiscordUnlink})
	c2.until(func(m proto.DiscordMsg) bool { return !m.Linked })
	if s2.discord.active("555") {
		t.Fatal("active after unlink")
	}
}

func TestDiscordFollowBadSignature(t *testing.T) {
	s := New(Config{DiscordClientID: "42", DiscordSecret: "shh", DiscordBotToken: "bot"})
	srv := httptest.NewServer(s)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/discord/follow", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	ws.Read(ctx) // hello
	k := identity.Ephemeral()
	b, _ := json.Marshal(proto.DiscordMsg{T: proto.TDiscordAuth, Key: k.Public(), Sig: base64.StdEncoding.EncodeToString(k.Sign(proto.DiscordFollowPurpose, []byte("not the challenge")))})
	ws.Write(ctx, websocket.MessageText, b)
	if _, _, err := ws.Read(ctx); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("read after a bad signature: %v", err)
	}
}

func TestDiscordFollowOffWithoutBot(t *testing.T) {
	s := New(Config{DiscordClientID: "42", DiscordSecret: "shh"})
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/v1/discord/follow", nil))
	if w.Code != 404 {
		t.Fatalf("follow without a bot token: %d", w.Code)
	}
}

func TestDiscordFollowProbeWithBot(t *testing.T) {
	s := New(Config{DiscordClientID: "42", DiscordSecret: "shh", DiscordBotToken: "bot"})
	ts := httptest.NewServer(s)
	defer ts.Close()
	res, err := http.Get(ts.URL + "/v1/discord/follow")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode == http.StatusNotFound || res.StatusCode >= 500 {
		t.Fatalf("plain GET probe with bot = %d, want non-404 client error (e.g. 426)", res.StatusCode)
	}
}

// TestBotLink: /bot, the link users send their server's admin, opens the
// docs section with the Add to Discord button.
func TestBotLink(t *testing.T) {
	ts := httptest.NewServer(New(Config{}))
	defer ts.Close()
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := c.Get(ts.URL + "/bot")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/docs/discord#add-the-bot" {
		t.Fatalf("got %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	res, err = http.Get(ts.URL + "/docs/discord")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(b), `id="add-the-bot"`) || !strings.Contains(string(b), "scope=bot") {
		t.Fatal("the docs page lacks the add-the-bot section")
	}
}

func TestDiscordLinkPendingCapAndReplace(t *testing.T) {
	s := New(Config{DiscordClientID: "42", DiscordSecret: "shh", DiscordBotToken: "bot", Log: quietLog()})
	s.discord.maxPending = 2
	srv := httptest.NewServer(s)
	defer srv.Close()

	k1, k2, k3 := identity.Ephemeral(), identity.Ephemeral(), identity.Ephemeral()
	c1 := dialFollow(t, srv.URL, k1)
	c1.next() // initial state

	// Two link requests from the same key keep only the newest state.
	c1.send(proto.DiscordMsg{T: proto.TDiscordLink})
	u1, _ := url.Parse(c1.next().LinkURL)
	st1 := u1.Query().Get("state")

	c1.send(proto.DiscordMsg{T: proto.TDiscordLink})
	u2, _ := url.Parse(c1.next().LinkURL)
	st2 := u2.Query().Get("state")
	if st1 == "" || st2 == "" || st1 == st2 {
		t.Fatalf("states %q %q", st1, st2)
	}
	s.discord.mu.Lock()
	nPending := len(s.discord.pending)
	s.discord.mu.Unlock()
	if nPending != 1 {
		t.Fatalf("pending count for one key = %d, want 1", nPending)
	}
	if res, _ := http.Get(srv.URL + "/v1/discord/linked?code=c0de&state=" + url.QueryEscape(st1)); res.StatusCode != 400 {
		t.Fatalf("replaced state still accepted: %d", res.StatusCode)
	}

	// Fill the map (maxPending = 2): k2 fits, k3 is refused until an entry expires.
	c2 := dialFollow(t, srv.URL, k2)
	c2.next()
	c2.send(proto.DiscordMsg{T: proto.TDiscordLink})
	if m := c2.next(); m.LinkURL == "" {
		t.Fatalf("k2 refused below cap: %+v", m)
	}

	c3 := dialFollow(t, srv.URL, k3)
	c3.next()
	c3.send(proto.DiscordMsg{T: proto.TDiscordLink})
	if m := c3.next(); m.LinkURL != "" || m.Error != proto.ErrRateLimited {
		t.Fatalf("k3 over pending cap: %+v", m)
	}

	// Expire k1's pending entry: k3 now gets a link URL.
	s.discord.mu.Lock()
	p := s.discord.pending[st2]
	p.expires = time.Now().Add(-time.Second)
	s.discord.pending[st2] = p
	s.discord.mu.Unlock()

	c3.send(proto.DiscordMsg{T: proto.TDiscordLink})
	if m := c3.next(); m.LinkURL == "" {
		t.Fatalf("k3 after expiry: %+v", m)
	}
}

func TestDiscordFollowPerConnRateLimitAndUnlinkNoRewrite(t *testing.T) {
	links := filepath.Join(t.TempDir(), "links.json")
	s := New(Config{DiscordClientID: "42", DiscordSecret: "shh", DiscordBotToken: "bot", DiscordLinksFile: links, Log: quietLog()})
	srv := httptest.NewServer(s)
	defer srv.Close()

	k := identity.Ephemeral()
	c := dialFollow(t, srv.URL, k)
	c.next() // initial state

	// Unlinking a key that isn't linked must not create or rewrite links.json.
	c.send(proto.DiscordMsg{T: proto.TDiscordUnlink})
	c.next()
	if _, err := os.Stat(links); !os.IsNotExist(err) {
		t.Fatalf("unlink of unlinked key wrote %s: %v", links, err)
	}

	// Seed a link for another key, backdate mtime, and verify unlinking k leaves the file untouched.
	other := identity.Ephemeral()
	if _, err := s.discord.links.set(other.Public(), &discordLink{User: "999", Name: "Bob", At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-10 * time.Second).Truncate(time.Second)
	if err := os.Chtimes(links, past, past); err != nil {
		t.Fatal(err)
	}
	s.discord.links.mu.Lock()
	s.discord.links.mtime = past
	s.discord.links.mu.Unlock()

	c.send(proto.DiscordMsg{T: proto.TDiscordUnlink})
	c.next()
	if fi, err := os.Stat(links); err != nil || !fi.ModTime().Equal(past) {
		t.Fatalf("unlinked key rewrote links file: mtime %v, want %v", fi.ModTime(), past)
	}

	// Per-connection limit: 5 link and 5 unlink messages/min; extras are ignored.
	var fifthState string
	for i := range 5 {
		c.send(proto.DiscordMsg{T: proto.TDiscordLink})
		m := c.next()
		if m.LinkURL == "" {
			t.Fatalf("link %d empty", i)
		}
		u, _ := url.Parse(m.LinkURL)
		fifthState = u.Query().Get("state")
	}
	// 6th and 7th link messages are ignored (fifthState stays active, nothing sent).
	c.send(proto.DiscordMsg{T: proto.TDiscordLink})
	c.send(proto.DiscordMsg{T: proto.TDiscordLink})
	// We already sent 2 unlinks above; 3 more reach 5, then 6th is ignored.
	for range 3 {
		c.send(proto.DiscordMsg{T: proto.TDiscordUnlink})
		if m := c.next(); m.LinkURL != "" {
			t.Fatalf("expected unlink state, got extra link URL %q", m.LinkURL)
		}
	}
	c.send(proto.DiscordMsg{T: proto.TDiscordUnlink}) // 6th unlink: ignored
	rctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, b, err := c.ws.Read(rctx); err == nil {
		t.Fatalf("6th unlink was not ignored: %s", b)
	}

	s.discord.mu.Lock()
	_, kept := s.discord.pending[fifthState]
	s.discord.mu.Unlock()
	if !kept {
		t.Fatal("ignored 6th link replaced the 5th pending state")
	}
}
