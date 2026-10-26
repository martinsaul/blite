package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"blite/internal/proto"
)

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	s := New(Config{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return s, "ws" + strings.TrimPrefix(ts.URL, "http")
}

type client struct {
	t  *testing.T
	ws *websocket.Conn
}

func dial(t *testing.T, url string) *client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, url+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	return &client{t: t, ws: ws}
}

func (c *client) send(v any) {
	c.t.Helper()
	b, _ := json.Marshal(v)
	if err := c.ws.Write(context.Background(), websocket.MessageText, b); err != nil {
		c.t.Fatal(err)
	}
}

func (c *client) recv() map[string]any {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := c.ws.Read(ctx)
	if err != nil {
		c.t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

func (c *client) expect(typ string) map[string]any {
	c.t.Helper()
	m := c.recv()
	if m["t"] != typ {
		c.t.Fatalf("want %s, got %v", typ, m)
	}
	return m
}

const pw = "correct horse"

func TestCreateJoinSignalLeave(t *testing.T) {
	_, url := newTestServer(t)
	a := dial(t, url)
	a.send(proto.Create{T: proto.TCreate, Password: pw, Name: "alice", IDKey: "ka"})
	wa := a.expect(proto.TWelcome)
	room := wa["room"].(string)
	if len(wa["iceServers"].([]any)) == 0 {
		t.Fatal("no ICE servers")
	}

	b := dial(t, url)
	b.send(proto.Join{T: proto.TJoin, Room: strings.ToLower(room[:4] + "-" + room[4:]), Password: pw, Name: "bob", IDKey: "kb"})
	wb := b.expect(proto.TWelcome)
	peers := wb["peers"].([]any)
	if len(peers) != 1 || peers[0].(map[string]any)["peerId"] != wa["peerId"] {
		t.Fatalf("bob's peers: %v", peers)
	}
	if pj := a.expect(proto.TPeerJoined); pj["peerId"] != wb["peerId"] || pj["name"] != "bob" {
		t.Fatalf("peer-joined: %v", pj)
	}

	b.send(map[string]any{"t": "signal", "to": wa["peerId"], "data": map[string]any{"sdp": "offer"}})
	sig := a.expect(proto.TSignal)
	if sig["from"] != wb["peerId"] || sig["data"].(map[string]any)["sdp"] != "offer" {
		t.Fatalf("signal: %v", sig)
	}

	b.send(map[string]any{"t": "signal", "to": "nobody", "data": map[string]any{}})
	if e := b.expect(proto.TError); e["code"] != proto.ErrNoPeer {
		t.Fatalf("unknown peer: %v", e)
	}

	b.send(map[string]any{"t": "leave"})
	if pl := a.expect(proto.TPeerLeft); pl["peerId"] != wb["peerId"] {
		t.Fatalf("peer-left: %v", pl)
	}
}

func TestJoinRateLimit(t *testing.T) {
	s, url := newTestServer(t)
	a := dial(t, url)
	a.send(proto.Create{T: proto.TCreate, Password: pw, Name: "alice"})
	room := a.expect(proto.TWelcome)["room"].(string)

	for i := 0; i < 5; i++ {
		c := dial(t, url)
		c.send(proto.Join{T: proto.TJoin, Room: room, Password: "nope nope", Name: "x"})
		if e := c.expect(proto.TError); e["code"] != proto.ErrBadPassword {
			t.Fatalf("attempt %d: %v", i, e)
		}
	}
	c := dial(t, url)
	c.send(proto.Join{T: proto.TJoin, Room: room, Password: pw, Name: "x"})
	if e := c.expect(proto.TError); e["code"] != proto.ErrRateLimited {
		t.Fatalf("6th attempt, even with the right password: %v", e)
	}
	_ = s
}

func TestDisconnectResume(t *testing.T) {
	s, url := newTestServer(t)
	a := dial(t, url)
	a.send(proto.Create{T: proto.TCreate, Password: pw, Name: "alice"})
	wa := a.expect(proto.TWelcome)

	b := dial(t, url)
	b.send(proto.Join{T: proto.TJoin, Room: wa["room"].(string), Password: pw, Name: "bob"})
	b.expect(proto.TWelcome)
	a.expect(proto.TPeerJoined)

	a.ws.CloseNow() // drop without leave
	deadline := time.Now().Add(2 * time.Second)
	for s.Rooms().Count() != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	a2 := dial(t, url)
	a2.send(proto.Resume{T: proto.TResume, Room: wa["room"].(string), PeerID: wa["peerId"].(string), Token: wa["resume"].(string)})
	w2 := a2.expect(proto.TWelcome)
	if w2["peerId"] != wa["peerId"] || len(w2["peers"].([]any)) != 1 {
		t.Fatalf("resume welcome: %v", w2)
	}
}

func TestJoinPage(t *testing.T) {
	_, url := newTestServer(t)
	base := "http" + strings.TrimPrefix(url, "ws")

	res, err := http.Get(base + "/j/7k3q-x9md")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(body), `data-code="7K3QX9MD"`) {
		t.Fatalf("join page: %d %s", res.StatusCode, body)
	}
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("CSP: %q", csp)
	}

	res, _ = http.Get(base + "/j/%3Cscript%3E")
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatalf("bad code got %d", res.StatusCode)
	}
}

func TestClientBuildGates(t *testing.T) {
	s := New(Config{MinCompatibleBuild: 10, MinRecommendedBuild: 20, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	url := "ws" + strings.TrimPrefix(ts.URL, "http")
	meta := func(b int) *proto.ClientMeta {
		return &proto.ClientMeta{Platform: "linux", AppVersion: "0.1.0", Build: b}
	}

	for _, m := range []*proto.ClientMeta{nil, meta(9)} {
		c := dial(t, url)
		c.send(proto.Create{T: proto.TCreate, Password: pw, Name: "old", Client: m})
		if e := c.expect(proto.TError); e["code"] != proto.ErrUpdateRequired {
			t.Fatalf("build %v: %v", m, e)
		}
	}

	a := dial(t, url)
	a.send(proto.Create{T: proto.TCreate, Password: pw, Name: "alice", Client: meta(15)})
	wa := a.expect(proto.TWelcome)
	if wa["updateRecommended"] != true {
		t.Fatalf("build 15 below recommended 20 not nudged: %v", wa)
	}
	b := dial(t, url)
	b.send(proto.Join{T: proto.TJoin, Room: wa["room"].(string), Password: pw, Name: "bob", Client: meta(20)})
	wb := b.expect(proto.TWelcome)
	if wb["updateRecommended"] != nil {
		t.Fatalf("current build nudged: %v", wb)
	}
	if p := wb["peers"].([]any)[0].(map[string]any); p["client"] != "0.1.0 (15) linux" {
		t.Fatalf("peer client label: %v", p)
	}
	if pj := a.expect(proto.TPeerJoined); pj["client"] != "0.1.0 (20) linux" {
		t.Fatalf("peer-joined client label: %v", pj)
	}
}

func TestFixedRoomNeedsHostKey(t *testing.T) {
	s := New(Config{HostKey: "host-secret", Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	url := "ws" + strings.TrimPrefix(ts.URL, "http")
	const code = "7K3QX9MD"

	for _, key := range []string{"", "wrong"} {
		c := dial(t, url)
		c.send(proto.Create{T: proto.TCreate, Password: pw, Name: "relay", Room: code, HostKey: key})
		if e := c.expect(proto.TError); e["code"] != proto.ErrBadRequest {
			t.Fatalf("host key %q: %v", key, e)
		}
	}
	a := dial(t, url)
	a.send(proto.Create{T: proto.TCreate, Password: pw, Name: "relay", Room: "7k3q-x9md", HostKey: "host-secret"})
	if w := a.expect(proto.TWelcome); w["room"] != code {
		t.Fatalf("fixed room: %v", w)
	}
	// A second create of the same room joins it (relay restarted while the
	// room lived on); a wrong password is still refused.
	b := dial(t, url)
	b.send(proto.Create{T: proto.TCreate, Password: pw, Name: "relay2", Room: code, HostKey: "host-secret"})
	if w := b.expect(proto.TWelcome); w["room"] != code || len(w["peers"].([]any)) != 1 {
		t.Fatalf("re-create joined: %v", w)
	}
	x := dial(t, url)
	x.send(proto.Create{T: proto.TCreate, Password: "other password", Name: "x", Room: code, HostKey: "host-secret"})
	if e := x.expect(proto.TError); e["code"] != proto.ErrBadPassword {
		t.Fatalf("wrong password: %v", e)
	}
}

func TestReportStored(t *testing.T) {
	dir := t.TempDir()
	ts := httptest.NewServer(New(Config{ReportDir: dir, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}))
	defer ts.Close()
	post := func(body string) int {
		res, err := http.Post(ts.URL+"/v1/report", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if c := post(`{"platform":"windows","build":43,"kind":"crash","log":"fatal error: x"}`); c != http.StatusAccepted {
		t.Fatalf("status %d", c)
	}
	if c := post(`{"platform":"windows"}`); c != http.StatusBadRequest {
		t.Fatalf("empty log: status %d", c)
	}
	m, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(m) != 1 || !strings.Contains(m[0], "-windows-43-crash-") {
		t.Fatalf("stored %v", m)
	}
}
