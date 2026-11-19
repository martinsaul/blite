package server

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"blite/internal/proto"
)

func TestDiscordToken(t *testing.T) {
	var got url.Values
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		got = r.PostForm
		w.Write([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":604800}`))
	}))
	defer fake.Close()
	old := discordTokenURL
	discordTokenURL = fake.URL
	defer func() { discordTokenURL = old }()

	s := New(Config{DiscordClientID: "42", DiscordSecret: "shh"})
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("POST", "/v1/discord/token", strings.NewReader(`{"code":"c0de"}`)))
	body, _ := io.ReadAll(w.Body)
	if w.Code != 200 || !strings.Contains(string(body), `"access_token":"at"`) {
		t.Fatalf("%d %s", w.Code, body)
	}
	if got.Get("client_secret") != "shh" || got.Get("code") != "c0de" || got.Get("grant_type") != "authorization_code" || got.Get("redirect_uri") != DiscordRedirect {
		t.Fatalf("form %v", got)
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("POST", "/v1/discord/token", strings.NewReader(`{}`)))
	if w.Code != 400 {
		t.Fatalf("empty body: %d", w.Code)
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/v1/channel/123456789", nil))
	if w.Code != 404 {
		t.Fatalf("no room: %d", w.Code)
	}
}

func TestDiscordChannelSharers(t *testing.T) {
	s, url := newTestServer(t)
	base := "http" + strings.TrimPrefix(url, "ws")
	const chID = "123456789"

	getChannel := func() (int, proto.ChannelRoom, []byte) {
		t.Helper()
		res, err := http.Get(base + "/v1/channel/" + chID)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(res.Body)
		res.Body.Close()
		var cr proto.ChannelRoom
		if res.StatusCode == http.StatusOK {
			if err := json.Unmarshal(raw, &cr); err != nil {
				t.Fatalf("decode %s: %v", raw, err)
			}
		}
		return res.StatusCode, cr, raw
	}
	waitRoom := func(cond func(proto.ChannelRoom, bool) bool) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if cond(s.Rooms().Channel(chID)) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		cr, ok := s.Rooms().Channel(chID)
		t.Fatalf("timed out waiting for channel state; last=%+v ok=%v", cr, ok)
	}

	a := dial(t, url)
	a.send(proto.Create{T: proto.TCreate, Password: "", Name: "alice", Channel: chID})
	code := a.expect(proto.TWelcome)["room"].(string)

	st, cr, _ := getChannel()
	if st != 200 || cr.Room != code || !cr.Open || cr.People != 1 || len(cr.Sharers) != 0 || !slices.Equal(cr.Members, []string{"alice"}) {
		t.Fatalf("initial channel: %d %+v", st, cr)
	}

	b := dial(t, url)
	b.send(proto.Join{T: proto.TJoin, Room: code, Name: "bob"})
	b.expect(proto.TWelcome)
	a.expect(proto.TPeerJoined)

	b.send(proto.Sharing{T: proto.TSharing, On: true})
	waitRoom(func(c proto.ChannelRoom, ok bool) bool { return ok && slices.Equal(c.Sharers, []string{"bob"}) })

	st, cr, raw := getChannel()
	if st != 200 || cr.People != 2 || !slices.Equal(cr.Sharers, []string{"bob"}) || !slices.Equal(cr.Members, []string{"alice", "bob"}) {
		t.Fatalf("bob sharing: %d %+v", st, cr)
	}

	// Older clients decode only {room, open, people} and ignore new fields.
	var legacy struct {
		Room   string `json:"room"`
		Open   bool   `json:"open"`
		People int    `json:"people"`
	}
	if err := json.Unmarshal(raw, &legacy); err != nil || legacy.Room != code || !legacy.Open || legacy.People != 2 {
		t.Fatalf("legacy decode: %+v %v", legacy, err)
	}

	a.send(proto.Sharing{T: proto.TSharing, On: true})
	b.send(proto.Rename{T: proto.TRename, Name: "robert"})
	a.expect(proto.TPeerRenamed)
	waitRoom(func(c proto.ChannelRoom, ok bool) bool {
		return ok && slices.Equal(c.Sharers, []string{"alice", "robert"})
	})
	_, cr, _ = getChannel()
	if !slices.Equal(cr.Sharers, []string{"alice", "robert"}) || !slices.Equal(cr.Members, []string{"alice", "robert"}) {
		t.Fatalf("both sharing after rename: %+v", cr)
	}

	a.send(proto.Sharing{T: proto.TSharing, On: false})
	b.send(proto.Leave{T: proto.TLeave})
	a.expect(proto.TPeerLeft)
	waitRoom(func(c proto.ChannelRoom, ok bool) bool { return ok && c.People == 1 && len(c.Sharers) == 0 })
	_, cr, _ = getChannel()
	if cr.People != 1 || len(cr.Sharers) != 0 || !slices.Equal(cr.Members, []string{"alice"}) {
		t.Fatalf("after bob left: %+v", cr)
	}

	a.send(proto.Leave{T: proto.TLeave})
	waitRoom(func(_ proto.ChannelRoom, ok bool) bool { return !ok })
	if st, _, _ := getChannel(); st != 404 {
		t.Fatalf("after everyone left: %d", st)
	}
}

func TestDiscordInteraction(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}

	// Disabled when DiscordPublicKey is unset.
	off := New(Config{})
	w := httptest.NewRecorder()
	off.ServeHTTP(w, httptest.NewRequest("POST", "/v1/discord/interactions", strings.NewReader(`{"type":1}`)))
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when disabled, got %d", w.Code)
	}

	s := New(Config{DiscordPublicKey: hex.EncodeToString(pub)})
	signedReq := func(ts, body string, sigHex string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/discord/interactions", strings.NewReader(body))
		if sigHex == "" {
			sig := ed25519.Sign(priv, []byte(ts+body))
			sigHex = hex.EncodeToString(sig)
		}
		req.Header.Set("X-Signature-Timestamp", ts)
		req.Header.Set("X-Signature-Ed25519", sigHex)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec
	}

	// Bad signature -> 401.
	if rec := signedReq("1700000000", `{"type":1}`, strings.Repeat("00", ed25519.SignatureSize)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad sig: expected 401, got %d", rec.Code)
	}

	// PING -> PONG.
	rec := signedReq("1700000000", `{"type":1}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("ping: %d %s", rec.Code, rec.Body.String())
	}
	var pong struct {
		Type int `json:"type"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pong); err != nil || pong.Type != 1 {
		t.Fatalf("ping body: %s", rec.Body.String())
	}

	// /status slash command -> ephemeral status message.
	rec = signedReq("1700000001", `{"type":2,"data":{"name":"status"}}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Type int `json:"type"`
		Data struct {
			Content string `json:"content"`
			Flags   int    `json:"flags"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("status json: %v", err)
	}
	if resp.Type != 4 || resp.Data.Flags != 64 || !strings.Contains(resp.Data.Content, "Bellion Share signaling is online") {
		t.Fatalf("unexpected status response: %+v", resp)
	}
}
