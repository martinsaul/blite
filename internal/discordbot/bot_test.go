package discordbot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeGateway is a Discord Gateway that sends hello, expects identify (or
// resume), then sends READY and the events queued on send.
type fakeGateway struct {
	t        *testing.T
	send     chan string
	identify chan map[string]any
	resumed  chan map[string]any
	conns    chan *websocket.Conn
}

func newFakeGateway(t *testing.T) (*fakeGateway, *httptest.Server) {
	g := &fakeGateway{t: t, send: make(chan string, 16), identify: make(chan map[string]any, 4), resumed: make(chan map[string]any, 4), conns: make(chan *websocket.Conn, 4)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx := r.Context()
		g.conns <- ws
		ws.Write(ctx, websocket.MessageText, []byte(`{"op":10,"d":{"heartbeat_interval":45000}}`))
		_, b, err := ws.Read(ctx)
		if err != nil {
			return
		}
		var p struct {
			Op int            `json:"op"`
			D  map[string]any `json:"d"`
		}
		json.Unmarshal(b, &p)
		switch p.Op {
		case opIdentify:
			g.identify <- p.D
			ws.Write(ctx, websocket.MessageText, []byte(`{"op":0,"s":1,"t":"READY","d":{"session_id":"S1","resume_gateway_url":"`+"ws://"+r.Host+`"}}`))
		case opResume:
			g.resumed <- p.D
		}
		go func() { // drain heartbeats
			for {
				if _, _, err := ws.Read(ctx); err != nil {
					return
				}
			}
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case m := <-g.send:
				if err := ws.Write(ctx, websocket.MessageText, []byte(m)); err != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(srv.Close)
	return g, srv
}

type recorder struct {
	mu     sync.Mutex
	active map[string]bool
	got    chan string
}

func (r *recorder) Active(u string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active[u]
}

func (r *recorder) OnVoice(u string, v *Voice) {
	if v == nil {
		r.got <- u + ":-"
		return
	}
	r.got <- u + ":" + v.Guild + "/" + v.Channel
}

func (r *recorder) next(t *testing.T) string {
	t.Helper()
	select {
	case s := <-r.got:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("no voice update")
		return ""
	}
}

func TestGatewayFollowsActiveUsersOnly(t *testing.T) {
	g, srv := newFakeGateway(t)
	rec := &recorder{active: map[string]bool{"alice": true}, got: make(chan string, 16)}
	b := &Bot{Token: "tok", GatewayURL: "ws://" + strings.TrimPrefix(srv.URL, "http://"), Active: rec.Active, OnVoice: rec.OnVoice}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	id := <-g.identify
	if id["token"] != "tok" || id["intents"] != float64(Intents) {
		t.Fatalf("identify %v", id)
	}
	// Bob is in voice too: dropped, never stored.
	g.send <- `{"op":0,"s":2,"t":"GUILD_CREATE","d":{"id":"g1","name":"Game Night","channels":[{"id":"c1","name":"gaming"},{"id":"c2","name":"chill"}],"voice_states":[{"user_id":"alice","channel_id":"c1"},{"user_id":"bob","channel_id":"c1"}]}}`
	if s := rec.next(t); s != "alice:Game Night/gaming" {
		t.Fatalf("got %q", s)
	}
	g.send <- `{"op":0,"s":3,"t":"VOICE_STATE_UPDATE","d":{"guild_id":"g1","user_id":"bob","channel_id":"c2"}}`
	g.send <- `{"op":0,"s":4,"t":"VOICE_STATE_UPDATE","d":{"guild_id":"g1","user_id":"alice","channel_id":"c2"}}`
	if s := rec.next(t); s != "alice:Game Night/chill" {
		t.Fatalf("got %q", s)
	}
	g.send <- `{"op":0,"s":5,"t":"VOICE_STATE_UPDATE","d":{"guild_id":"g1","user_id":"alice","channel_id":null}}`
	if s := rec.next(t); s != "alice:-" {
		t.Fatalf("got %q", s)
	}
	b.mu.Lock()
	n := len(b.voice) + len(b.gen)
	_, bob := b.gen["bob"]
	b.mu.Unlock()
	if bob || n != 1 { // alice's gen only
		t.Fatalf("kept state for others: voice+gen = %d, bob %v", n, bob)
	}

	// A reconnect resumes the session.
	(<-g.conns).Close(websocket.StatusGoingAway, "")
	select {
	case r := <-g.resumed:
		if r["session_id"] != "S1" {
			t.Fatalf("resume %v", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no resume")
	}
}

func TestGatewayFatalClose(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ws.Write(r.Context(), websocket.MessageText, []byte(`{"op":10,"d":{"heartbeat_interval":45000}}`))
		ws.Read(r.Context())
		ws.Close(4004, "Authentication failed.")
	}))
	defer srv.Close()
	b := &Bot{Token: "bad", GatewayURL: "ws://" + strings.TrimPrefix(srv.URL, "http://"), Active: func(string) bool { return false }}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := b.Run(ctx); err == nil || ctx.Err() != nil {
		t.Fatalf("Run = %v, want a fatal error at once", err)
	}
}

func TestActivateLooksUpOnce(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.Path)
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bot tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/guilds/g2/voice-states/alice" {
			w.Write([]byte(`{"channel_id":"c9","user_id":"alice"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer api.Close()
	rec := &recorder{active: map[string]bool{"alice": true}, got: make(chan string, 4)}
	b := &Bot{Token: "tok", APIURL: api.URL, Active: rec.Active, OnVoice: rec.OnVoice}
	b.init()
	b.guilds["g1"] = &guild{name: "One", channels: map[string]string{}}
	b.guilds["g2"] = &guild{name: "Two", channels: map[string]string{"c9": "lounge"}}
	b.Activate(context.Background(), "alice")
	if s := rec.next(t); s != "alice:Two/lounge" {
		t.Fatalf("got %q", s)
	}
	if v, ok := b.Voice("alice"); !ok || v.ChannelID != "c9" {
		t.Fatalf("Voice = %+v %v", v, ok)
	}
	b.Forget("alice")
	if _, ok := b.Voice("alice"); ok {
		t.Fatal("Forget kept the voice state")
	}
}
