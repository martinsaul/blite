package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"blite/internal/proto"
)

// A restarted server with a StateFile keeps its rooms: both members resume
// into their old slots and can still signal each other, and the file is
// private to the server's user.
func TestStateFileSurvivesRestart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state.json")
	cfg := Config{StateFile: file, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	start := func() (*Server, string) {
		s := New(cfg)
		if err := s.LoadState(); err != nil {
			t.Fatal(err)
		}
		ts := httptest.NewServer(s)
		t.Cleanup(ts.Close)
		return s, "ws" + strings.TrimPrefix(ts.URL, "http")
	}

	s1, url := start()
	a := dial(t, url)
	a.send(proto.Create{T: proto.TCreate, Password: pw, Name: "alice"})
	wa := a.expect(proto.TWelcome)
	b := dial(t, url)
	b.send(proto.Join{T: proto.TJoin, Room: wa["room"].(string), Password: pw, Name: "bob"})
	wb := b.expect(proto.TWelcome)
	a.expect(proto.TPeerJoined)
	if err := s1.SaveState(); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file: %v %v, want mode 0600", fi.Mode(), err)
	}

	_, url2 := start() // the "restarted" server
	resume := func(w map[string]any) *client {
		c := dial(t, url2)
		c.send(proto.Resume{T: proto.TResume, Room: w["room"].(string), PeerID: w["peerId"].(string), Token: w["resume"].(string)})
		got := c.expect(proto.TWelcome)
		if got["peerId"] != w["peerId"] || got["owner"] != wa["peerId"] {
			t.Fatalf("resume after restart: %v", got)
		}
		return c
	}
	b2 := resume(wb)
	a2 := resume(wa)
	b2.send(proto.Signal{T: proto.TSignal, To: wa["peerId"].(string), Data: []byte(`"hi"`)})
	if m := a2.expect(proto.TSignal); m["from"] != wb["peerId"] {
		t.Fatalf("signal after restart: %v", m)
	}
}

func TestStateFileMissingOrBad(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state.json")
	s := New(Config{StateFile: file, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err := s.LoadState(); err != nil {
		t.Fatalf("missing file: %v", err)
	}
	os.WriteFile(file, []byte("not json"), 0o600)
	if err := s.LoadState(); err == nil {
		t.Fatal("bad file loaded")
	}
}

// CloseWhenSent writes everything queued before it, then closes.
func TestCloseWhenSentDrains(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c := &conn{ws: ws, out: make(chan []byte, sendBuffer), closed: make(chan struct{})}
		for i := range 50 {
			c.Send(proto.Signal{T: proto.TSignal, Data: []byte(strconv.Itoa(i))})
		}
		c.CloseWhenSent()
		c.Send(proto.Signal{T: proto.TSignal, Data: []byte("99")}) // after: never sent
		c.writeLoop(context.Background())
		<-c.closed
		ws.Read(context.Background()) // until the close handshake is done
	}))
	defer ts.Close()
	cl := dial(t, "ws"+strings.TrimPrefix(ts.URL, "http"))
	for i := range 50 {
		if m := cl.expect(proto.TSignal); m["data"] != float64(i) {
			t.Fatalf("message %d: %v", i, m)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, b, err := cl.ws.Read(ctx); websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatalf("after the queue: %s %v, want a normal close", b, err)
	}
}
