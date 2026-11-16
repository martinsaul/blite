package sigclient

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"blite/internal/proto"
	"blite/internal/server"
)

func testServer(t *testing.T) string {
	t.Helper()
	s := server.New(server.Config{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
}

// Renewing moves alice to a new socket several times while bob streams
// signals at her: every signal arrives once and in order, bob never sees
// her leave, and alice never shows "reconnecting".
func TestRenewLosesNothing(t *testing.T) {
	url := testServer(t)
	var reconnecting atomic.Int32
	a, err := Create(context.Background(), url, proto.Create{T: proto.TCreate, Password: "correct horse", Name: "alice"}, Options{
		Renew: 200 * time.Millisecond,
		OnState: func(s string) {
			if s == "reconnecting" {
				reconnecting.Add(1)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Leave()
	b, err := Join(context.Background(), url, proto.Join{T: proto.TJoin, Room: a.Welcome.Room, Password: "correct horse", Name: "bob"}, Options{Renew: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Leave()

	first := a.conn()
	const n = 100
	go func() {
		for i := range n {
			for b.Signal(a.Welcome.PeerID, i) != nil {
				time.Sleep(time.Millisecond)
			}
			// Bursts of 8 leave messages queued on the old socket when a
			// renewal switches; ~40/s overall stays under the server's
			// per-connection limit, and ~2.5 s covers about ten renewals.
			if i%8 == 7 {
				time.Sleep(200 * time.Millisecond)
			}
		}
	}()
	want, renewals := 0, 0
	timeout := time.After(10 * time.Second)
	for want < n {
		select {
		case ev := <-a.Events:
			switch {
			case ev.Err != nil:
				t.Fatalf("alice's session ended: %v", ev.Err)
			case ev.Resumed != nil:
				renewals++
			case ev.Signal != nil:
				var got int
				json.Unmarshal(ev.Signal.Data, &got)
				if got != want {
					t.Fatalf("signal %d, want %d (lost or reordered across a renewal)", got, want)
				}
				want++
			}
		case <-timeout:
			t.Fatalf("got %d of %d signals, %d renewals", want, n, renewals)
		}
	}
	if renewals < 2 || a.conn() == first {
		t.Fatalf("%d renewals; want several", renewals)
	}
	if r := reconnecting.Load(); r != 0 {
		t.Fatalf("alice showed reconnecting %d times", r)
	}
	for {
		select {
		case ev := <-b.Events:
			if ev.Left != "" || ev.Err != nil {
				t.Fatalf("bob saw %+v", ev)
			}
		default:
			return
		}
	}
}

// A renewal that can't reach the server leaves the old socket working.
func TestRenewFailsKeepsOld(t *testing.T) {
	url := testServer(t)
	a, err := Create(context.Background(), url, proto.Create{T: proto.TCreate, Password: "correct horse", Name: "alice"}, Options{Renew: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Leave()
	first := a.conn()
	a.url = "ws://127.0.0.1:1/ws" // nothing listens there
	if a.renew() {
		t.Fatal("renewal to a dead server succeeded")
	}
	if a.conn() != first {
		t.Fatal("socket swapped after a failed renewal")
	}
	if err := a.Rename("alice2"); err != nil {
		t.Fatalf("old socket broken: %v", err)
	}
}

// fakeRenewServer seats the first connection, then answers each resume the
// way a real server may when a slot moves: the welcome goes out on the new
// socket while ten messages are still queued on the old one, which it then
// closes; after that it sends ten more on the new socket. Signals carry a
// running number.
func fakeRenewServer(t *testing.T) string {
	t.Helper()
	var mu sync.Mutex
	var cur *websocket.Conn
	n := 0
	sig := func(ws *websocket.Conn) {
		b, _ := json.Marshal(proto.Signal{T: proto.TSignal, From: "bob", Data: json.RawMessage(strconv.Itoa(n))})
		n++
		ws.Write(context.Background(), websocket.MessageText, b)
	}
	welcome := func(ws *websocket.Conn) {
		b, _ := json.Marshal(proto.Welcome{T: proto.TWelcome, Room: "7K3QX9MD", PeerID: "alice", Resume: "tok", Peers: []proto.Peer{{PeerID: "bob", Name: "bob"}}})
		ws.Write(context.Background(), websocket.MessageText, b)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		_, b, err := ws.Read(context.Background())
		if err != nil {
			return
		}
		var env proto.Envelope
		json.Unmarshal(b, &env)
		mu.Lock()
		old := cur
		cur = ws
		welcome(ws)
		if env.T == proto.TResume && old != nil {
			for range 10 {
				sig(old)
			}
			old.Close(websocket.StatusNormalClosure, "replaced")
		}
		for range 10 {
			sig(ws)
		}
		mu.Unlock()
		for { // until the client closes it
			if _, _, err := ws.Read(context.Background()); err != nil {
				return
			}
		}
	}))
	t.Cleanup(ts.Close)
	return "ws" + strings.TrimPrefix(ts.URL, "http")
}

// What was still queued on the old socket when the slot moved arrives
// before what comes on the new one, across several renewals.
func TestRenewReadsOldSocketToTheEnd(t *testing.T) {
	url := fakeRenewServer(t)
	a, err := Create(context.Background(), url, proto.Create{T: proto.TCreate, Password: "correct horse", Name: "alice"}, Options{Renew: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Leave()
	want, renewals := 0, 0
	timeout := time.After(10 * time.Second)
	for renewals < 4 {
		select {
		case ev := <-a.Events:
			switch {
			case ev.Err != nil:
				t.Fatalf("session ended: %v", ev.Err)
			case ev.Resumed != nil:
				renewals++
			case ev.Signal != nil:
				var got int
				json.Unmarshal(ev.Signal.Data, &got)
				if got != want {
					t.Fatalf("signal %d, want %d", got, want)
				}
				want++
			}
		case <-timeout:
			t.Fatalf("%d renewals, %d signals", renewals, want)
		}
	}
}
