package sigclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestDialErrorsRedactCredentials(t *testing.T) {
	urls := []string{
		"ws://alice:superSecretUserPw@127.0.0.1:1/ws?token=qSecretTok&password=qSecretPw&key=qSecretKey#fragSecretPw",
		"ws://us%40er:p@ss31@127.0.0.1:1/ws%23pctSecretFrag",
	}
	secrets := []string{
		"superSecretUserPw", "qSecretTok", "qSecretPw", "qSecretKey",
		"fragSecretPw", "us%40er", "p@ss31", "ss31", "pctSecretFrag",
	}
	for _, u := range urls {
		_, err1 := Probe(context.Background(), u, "7K3QX9MD", "roomPw", Options{})
		_, err2 := Create(context.Background(), u, proto.Create{Name: "alice"}, Options{})
		_, err3 := Join(context.Background(), u, proto.Join{Room: "7K3QX9MD", Name: "alice"}, Options{})
		for _, err := range []error{err1, err2, err3} {
			if err == nil {
				t.Fatalf("expected dial error for %s", u)
			}
			msg := err.Error()
			for _, sec := range secrets {
				if strings.Contains(msg, sec) {
					t.Fatalf("dial error leaked secret %q: %s", sec, msg)
				}
			}
			if !strings.Contains(msg, "127.0.0.1:1/ws") {
				t.Fatalf("dial error lost host/path context: %s", msg)
			}
			if !strings.Contains(msg, "#[cut]: ") {
				t.Fatalf("dial error lost colon after redacted fragment: %s", msg)
			}
			var ue *url.Error
			if errors.As(err, &ue) {
				t.Fatalf("dial error exposed *url.Error via Unwrap: %+v", ue)
			}
			var opErr *net.OpError
			if !errors.As(err, &opErr) {
				t.Fatalf("expected sanitized *net.OpError via Unwrap, got %T (%v)", errors.Unwrap(err), err)
			}
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Probe(ctx, urls[0], "7K3QX9MD", "", Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected errors.Is(err, context.Canceled), got %v", err)
	}
	if strings.Contains(err.Error(), "superSecretUserPw") {
		t.Fatalf("canceled dial error leaked secret: %v", err)
	}
}

// Leave returns within its bound when the server reads the Leave but never
// answers the close frame nor closes the socket (the close
// handshake held up quitting for 5 s), and the Leave still arrives.
func TestLeaveReturnsWhenServerNeverCloses(t *testing.T) {
	got := make(chan proto.Leave, 1)
	hang := make(chan struct{})
	defer close(hang)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		if _, _, err := ws.Read(context.Background()); err != nil {
			return
		}
		b, _ := json.Marshal(proto.Welcome{T: proto.TWelcome, Room: "7K3QX9MD", PeerID: "alice", Resume: "tok"})
		ws.Write(context.Background(), websocket.MessageText, b)
		_, b, err = ws.Read(context.Background())
		if err != nil {
			return
		}
		var m proto.Leave
		json.Unmarshal(b, &m)
		got <- m
		<-hang // no more reads: the client's close frame is never answered
	}))
	defer ts.Close()
	a, err := Create(context.Background(), "ws"+strings.TrimPrefix(ts.URL, "http"), proto.Create{T: proto.TCreate, Name: "alice"}, Options{Renew: -1})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	a.LeaveFor(proto.LeaveUpdate)
	if d := time.Since(start); d > leaveCloseWait+500*time.Millisecond {
		t.Fatalf("Leave took %v, want about %v", d, leaveCloseWait)
	}
	select {
	case m := <-got:
		if m.T != proto.TLeave || m.Reason != proto.LeaveUpdate {
			t.Fatalf("server got %+v, want a leave for an update", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the Leave never reached the server")
	}
	for ev := range a.Events { // run ends: the socket really is closed
		if ev.Err == nil || ev.Err.Error() != "left" {
			t.Fatalf("event %+v after Leave", ev)
		}
	}
}

// Against the real server, which closes the socket once it has the Leave,
// Leave returns as soon as it does and the other member sees us leave.
func TestLeaveFastWhenServerCloses(t *testing.T) {
	url := testServer(t)
	a, err := Create(context.Background(), url, proto.Create{T: proto.TCreate, Password: "correct horse", Name: "alice"}, Options{Renew: -1})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Join(context.Background(), url, proto.Join{T: proto.TJoin, Room: a.Welcome.Room, Password: "correct horse", Name: "bob"}, Options{Renew: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Leave()
	go func() { // drain alice's events so run can end
		for range a.Events {
		}
	}()
	start := time.Now()
	a.Leave()
	if d := time.Since(start); d >= leaveCloseWait {
		t.Fatalf("Leave took %v; the server's close should end it sooner", d)
	}
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev := <-b.Events:
			if ev.Left == a.Welcome.PeerID {
				return
			}
		case <-timeout:
			t.Fatal("bob never saw alice leave")
		}
	}
}

// SlotLost tells a refused resume apart from other ends.
func TestSlotLost(t *testing.T) {
	lost := fmt.Errorf("signaling lost (EOF); resume failed: %w", fmt.Errorf("%w: %w", errSent, ServerError(proto.ErrResumeFail)))
	if !SlotLost(lost) {
		t.Fatal("resume_failed not seen")
	}
	for _, err := range []error{errors.New("left"), errors.New("kicked"), ServerError(proto.ErrNoRoom), nil} {
		if SlotLost(err) {
			t.Fatalf("SlotLost(%v)", err)
		}
	}
}
