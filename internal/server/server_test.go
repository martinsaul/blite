package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"blite/internal/proto"
	"blite/internal/sigclient"
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

func TestProbe(t *testing.T) {
	s, url := newTestServer(t)
	a := dial(t, url)
	a.send(proto.Create{T: proto.TCreate, Password: pw, Name: "alice"})
	code := a.expect(proto.TWelcome)["room"].(string)

	openCreator := dial(t, url)
	openCreator.send(proto.Create{T: proto.TCreate, Password: "", Name: "bob"})
	openCode := openCreator.expect(proto.TWelcome)["room"].(string)

	// Correct password (and display formatting) answers exists: true without
	// creating rooms or seating a peer.
	p := dial(t, url)
	p.send(proto.Probe{T: proto.TProbe, Room: strings.ToLower(code[:4] + "-" + code[4:]), Password: pw})
	if r := p.expect(proto.TProbeResult); r["exists"] != true {
		t.Fatalf("right password: %v", r)
	}
	p.send(proto.Probe{T: proto.TProbe, Room: openCode, Password: ""})
	if r := p.expect(proto.TProbeResult); r["exists"] != true {
		t.Fatalf("open room: %v", r)
	}

	// Wrong password and missing room both answer exists: false (no oracle).
	p.send(proto.Probe{T: proto.TProbe, Room: code, Password: "wrong password"})
	rWrong := p.expect(proto.TProbeResult)
	p.send(proto.Probe{T: proto.TProbe, Room: "ZZZZZZZZ", Password: pw})
	rMissing := p.expect(proto.TProbeResult)
	p.send(proto.Probe{T: proto.TProbe, Room: "not a code!", Password: pw})
	rBadCode := p.expect(proto.TProbeResult)
	if rWrong["exists"] != false || rMissing["exists"] != false || rBadCode["exists"] != false {
		t.Fatalf("wrong=%v missing=%v badCode=%v", rWrong, rMissing, rBadCode)
	}
	if s.Rooms().Count() != 2 {
		t.Fatalf("rooms count = %d, want 2", s.Rooms().Count())
	}

	// Alice did not receive peer-joined from probes: a real join is the first message she sees.
	j := dial(t, url)
	j.send(proto.Join{T: proto.TJoin, Room: code, Password: pw, Name: "carol"})
	j.expect(proto.TWelcome)
	if pj := a.expect(proto.TPeerJoined); pj["name"] != "carol" {
		t.Fatalf("alice got %v before carol", pj)
	}
}

func TestProbeRateLimit(t *testing.T) {
	// Per-IP: 5 failed probes (wrong password + missing room) block the 6th
	// probe and a join from the same IP, even with the right password.
	_, url := newTestServer(t)
	a := dial(t, url)
	a.send(proto.Create{T: proto.TCreate, Password: pw, Name: "alice"})
	code := a.expect(proto.TWelcome)["room"].(string)

	for i := 0; i < 3; i++ {
		c := dial(t, url)
		c.send(proto.Probe{T: proto.TProbe, Room: code, Password: "nope nope"})
		if r := c.expect(proto.TProbeResult); r["exists"] != false {
			t.Fatalf("wrong pw probe %d: %v", i, r)
		}
	}
	for i := 0; i < 2; i++ {
		c := dial(t, url)
		c.send(proto.Probe{T: proto.TProbe, Room: "ZZZZZZZZ", Password: pw})
		if r := c.expect(proto.TProbeResult); r["exists"] != false {
			t.Fatalf("missing room probe %d: %v", i, r)
		}
	}
	c := dial(t, url)
	c.send(proto.Probe{T: proto.TProbe, Room: code, Password: pw})
	if e := c.expect(proto.TError); e["code"] != proto.ErrRateLimited {
		t.Fatalf("6th probe from blocked IP: %v", e)
	}
	j := dial(t, url)
	j.send(proto.Join{T: proto.TJoin, Room: code, Password: pw, Name: "bob"})
	if e := j.expect(proto.TError); e["code"] != proto.ErrRateLimited {
		t.Fatalf("join from blocked IP: %v", e)
	}

	// Per-room: 20 failed probes from distinct IPs block the 21st probe and
	// join on that room, while another room remains reachable.
	s2 := New(Config{TrustProxy: true, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ts2 := httptest.NewServer(s2)
	t.Cleanup(ts2.Close)
	url2 := "ws" + strings.TrimPrefix(ts2.URL, "http")
	dialIP := func(ip string) *client {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		h := http.Header{"X-Forwarded-For": {ip}}
		ws, _, err := websocket.Dial(ctx, url2+"/ws", &websocket.DialOptions{HTTPHeader: h})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ws.CloseNow() })
		return &client{t: t, ws: ws}
	}

	ca := dialIP("10.0.0.1")
	ca.send(proto.Create{T: proto.TCreate, Password: pw, Name: "alice"})
	roomA := ca.expect(proto.TWelcome)["room"].(string)
	cb := dialIP("10.0.0.2")
	cb.send(proto.Create{T: proto.TCreate, Password: pw, Name: "bob"})
	roomB := cb.expect(proto.TWelcome)["room"].(string)

	for i := 1; i <= 20; i++ {
		p := dialIP(fmt.Sprintf("198.51.100.%d", i))
		p.send(proto.Probe{T: proto.TProbe, Room: roomA, Password: "wrong password"})
		if r := p.expect(proto.TProbeResult); r["exists"] != false {
			t.Fatalf("room probe %d: %v", i, r)
		}
	}
	p21 := dialIP("198.51.100.21")
	p21.send(proto.Probe{T: proto.TProbe, Room: roomA, Password: pw})
	if e := p21.expect(proto.TError); e["code"] != proto.ErrRateLimited {
		t.Fatalf("21st probe on roomA: %v", e)
	}
	j22 := dialIP("198.51.100.22")
	j22.send(proto.Join{T: proto.TJoin, Room: roomA, Password: pw, Name: "carol"})
	if e := j22.expect(proto.TError); e["code"] != proto.ErrRateLimited {
		t.Fatalf("join on blocked roomA: %v", e)
	}
	pOther := dialIP("198.51.100.22")
	pOther.send(proto.Probe{T: proto.TProbe, Room: roomB, Password: pw})
	if r := pOther.expect(proto.TProbeResult); r["exists"] != true {
		t.Fatalf("probe on unblocked roomB: %v", r)
	}
}

func TestSigclientProbe(t *testing.T) {
	_, url := newTestServer(t)
	wsURL := url + "/ws"
	ctx := context.Background()

	a, err := sigclient.Create(ctx, wsURL, proto.Create{Password: pw, Name: "alice"}, sigclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Leave()

	if st, err := sigclient.Probe(ctx, wsURL, a.Welcome.Room, pw, sigclient.Options{}); err != nil || st != sigclient.ProbeExists {
		t.Fatalf("right password: %v %v", st, err)
	}
	if st, err := sigclient.Probe(ctx, wsURL, a.Welcome.Room, "wrong", sigclient.Options{}); err != nil || st != sigclient.ProbeGone {
		t.Fatalf("wrong password: %v %v", st, err)
	}
	if st, err := sigclient.Probe(ctx, wsURL, "ZZZZZZZZ", pw, sigclient.Options{}); err != nil || st != sigclient.ProbeGone {
		t.Fatalf("missing room: %v %v", st, err)
	}

	// Older servers reply bad_request to unknown first messages.
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		if _, _, err := ws.Read(r.Context()); err != nil {
			return
		}
		b, _ := json.Marshal(proto.Error{T: proto.TError, Code: proto.ErrBadRequest})
		ws.Write(r.Context(), websocket.MessageText, b)
	}))
	defer legacy.Close()
	legacyWS := "ws" + strings.TrimPrefix(legacy.URL, "http")
	if st, err := sigclient.Probe(ctx, legacyWS, a.Welcome.Room, pw, sigclient.Options{}); err != nil || st != sigclient.ProbeUnsupported {
		t.Fatalf("legacy server: %v %v", st, err)
	}
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

	res, err := http.Get(base + "/j/7k3q-x9md?e2ee=1")
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

	jsRes, err := http.Get(base + "/static/join.js")
	if err != nil {
		t.Fatal(err)
	}
	jsBody, _ := io.ReadAll(jsRes.Body)
	jsRes.Body.Close()
	if jsRes.StatusCode != 200 || !strings.Contains(string(jsBody), `&e2ee=1`) {
		t.Fatalf("join.js missing e2ee passthrough: %d %s", jsRes.StatusCode, jsBody)
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

	// Send logs with a zip (base64 "PK\x03\x04zz"): stored beside the
	// JSON, which names it and no longer carries it. Not a zip: refused.
	if c := post(`{"platform":"windows","build":76,"kind":"user","log":"x","zip":"UEsDBHp6"}`); c != http.StatusAccepted {
		t.Fatalf("zip report: status %d", c)
	}
	if c := post(`{"platform":"windows","build":76,"kind":"user","log":"x","zip":"aGVsbG8="}`); c != http.StatusBadRequest {
		t.Fatalf("non-zip: status %d", c)
	}
	z, _ := filepath.Glob(filepath.Join(dir, "*-windows-76-user-*.zip"))
	if len(z) != 1 {
		t.Fatalf("zips %v", z)
	}
	if b, _ := os.ReadFile(z[0]); string(b) != "PK\x03\x04zz" {
		t.Fatalf("zip content %q", b)
	}
	j, _ := os.ReadFile(strings.TrimSuffix(z[0], ".zip") + ".json")
	if !strings.Contains(string(j), `"zipFile": "`+filepath.Base(z[0])) || strings.Contains(string(j), `"zip":`) {
		t.Fatalf("report json %s", j)
	}
}

func TestSiteSEO(t *testing.T) {
	s := New(Config{SiteAliases: []string{"www.bellion.app", "old.example.com"}})
	get := func(host, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.Host = host
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	if w := get("old.example.com", "/terms?x=1"); w.Code != 301 || w.Header().Get("Location") != "https://bellion.app/terms?x=1" {
		t.Fatalf("old host page: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := get("bellion.app", "/terms"); w.Code != 200 || !strings.Contains(w.Body.String(), `rel="canonical"`) {
		t.Fatalf("terms: %d", w.Code)
	}
	if w := get("old.example.com", "/healthz"); w.Code != 200 {
		t.Fatalf("healthz on old host: %d", w.Code)
	}
	if w := get("bellion.app", "/robots.txt"); !strings.Contains(w.Body.String(), "Disallow: /j/") {
		t.Fatal("robots.txt")
	}
	for _, p := range []string{"/sitemap.xml", "/favicon.ico", "/apple-touch-icon.png", "/static/og.jpg", "/static/icon-512.png"} {
		if w := get("bellion.app", p); w.Code != 200 || w.Body.Len() == 0 {
			t.Fatalf("%s: %d", p, w.Code)
		}
	}
	for _, n := range siteShots {
		w := get("bellion.app", "/static/shots/"+n)
		if w.Code != 200 || w.Body.Len() == 0 || w.Header().Get("Content-Type") != "image/webp" || !strings.Contains(w.Header().Get("Cache-Control"), "max-age") {
			t.Fatalf("shot %s: %d %q", n, w.Code, w.Header().Get("Content-Type"))
		}
	}
	sitemap := get("bellion.app", "/sitemap.xml").Body.String()
	pages := []string{"/terms", "/privacy"}
	for _, p := range docPages {
		pages = append(pages, p.Path)
	}
	if len(pages) < 9 {
		t.Fatalf("docs pages: %v", pages)
	}
	for _, p := range pages {
		w := get("bellion.app", p)
		body := w.Body.String()
		if w.Code != 200 || !strings.Contains(body, `<link rel="canonical" href="https://bellion.app`+p+`">`) ||
			!strings.Contains(body, `<meta name="description"`) || !strings.Contains(body, `href="/docs"`) ||
			!strings.Contains(body, "https://github.com/martinsaul/blite") {
			t.Fatalf("%s: %d, missing canonical, description or nav", p, w.Code)
		}
		if !strings.Contains(sitemap, "<loc>https://bellion.app"+p+"</loc>") {
			t.Fatalf("%s not in sitemap", p)
		}
		if w := get("www.bellion.app", p); w.Code != 301 || w.Header().Get("Location") != "https://bellion.app"+p {
			t.Fatalf("%s on old host: %d %q", p, w.Code, w.Header().Get("Location"))
		}
		// Every image a page shows is served.
		for _, m := range regexp.MustCompile(`src="(/static/[^"]+)"`).FindAllStringSubmatch(body, -1) {
			if w := get("bellion.app", m[1]); w.Code != 200 {
				t.Fatalf("%s: image %s: %d", p, m[1], w.Code)
			}
		}
	}
	// Every page links the shared stylesheet, versioned so it can be cached long.
	css := get("bellion.app", siteCSSURL)
	if css.Code != 200 || !strings.HasPrefix(css.Header().Get("Content-Type"), "text/css") ||
		!strings.Contains(css.Header().Get("Cache-Control"), "immutable") || css.Body.Len() == 0 {
		t.Fatalf("site.css: %d %q %q", css.Code, css.Header().Get("Content-Type"), css.Header().Get("Cache-Control"))
	}
	for _, p := range pages {
		if !strings.Contains(get("bellion.app", p).Body.String(), `href="`+siteCSSURL+`"`) {
			t.Fatalf("%s doesn't link %s", p, siteCSSURL)
		}
	}
	// Docs pages get section ids and an on-page table of contents.
	relays := get("bellion.app", "/docs/relays").Body.String()
	if !strings.Contains(relays, `<h2 id="relay-mode">Relay mode</h2>`) || !strings.Contains(relays, `<a href="#relay-mode">Relay mode</a>`) ||
		!strings.Contains(relays, `href="/docs/relays" aria-current="page"`) {
		t.Fatal("docs: missing section ids, table of contents or current page")
	}
	if !strings.Contains(get("bellion.app", "/docs").Body.String(), vibecoded) {
		t.Fatal("docs overview: missing the vibecoded statement")
	}
	// The relay guide covers all three ways, and the overview and
	// troubleshooting point to it.
	for _, id := range []string{"volunteer", "dedicated", "hosted", "upload", "troubleshooting"} {
		if !strings.Contains(relays, `<h2 id="`+id+`">`) || !strings.Contains(relays, `<a href="#`+id+`">`) {
			t.Fatalf("relay guide: missing section %q", id)
		}
	}
	for _, p := range []string{"/docs", "/docs/troubleshooting", "/docs/self-hosting"} {
		if !strings.Contains(get("bellion.app", p).Body.String(), `href="/docs/relays`) {
			t.Fatalf("%s doesn't link the relay guide", p)
		}
	}
	if w := get("bellion.app", "/docs/"); w.Code != 301 || w.Header().Get("Location") != "/docs" {
		t.Fatalf("/docs/: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := get("bellion.app", "/docs/nope"); w.Code != 404 {
		t.Fatalf("/docs/nope: %d", w.Code)
	}
}

// vibecoded is the start of the statement the home page and the docs
// overview carry about how Bellion Share is built.
const vibecoded = "Bellion Share is a vibecoded application: it was built with AI coding agents."

// TestDownloadPageSite checks the download page's nav, screenshots and
// source link.
func TestDownloadPageSite(t *testing.T) {
	up := httptest.NewServer(http.NotFoundHandler())
	defer up.Close()
	s := New(Config{SiteAliases: []string{"www.bellion.app"}, UpdateURL: up.URL, UpdateChannel: "stable"})
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "bellion.app"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `href="/docs"`) || !strings.Contains(body, "https://github.com/martinsaul/blite") {
		t.Fatalf("download page: %d", w.Code)
	}
	if !strings.Contains(body, vibecoded) || !strings.Contains(body, `href="`+siteCSSURL+`"`) {
		t.Fatal("download page: missing the vibecoded statement or stylesheet")
	}
	// Bigger groups find the relay guide from the home page.
	if !strings.Contains(body, "Set up a relay") || !strings.Contains(body, `href="/docs/relays"`) {
		t.Fatal("download page: missing the relay callout")
	}
	for _, m := range regexp.MustCompile(`src="(/static/[^"]+)"`).FindAllStringSubmatch(body, -1) {
		r := httptest.NewRequest("GET", m[1], nil)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("image %s: %d", m[1], w.Code)
		}
	}
}

// With TrustedProxies, only a listed peer's headers count, and in
// X-Forwarded-For the client is the last hop that isn't a trusted proxy.
func TestTrustedProxies(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(Config{TrustProxy: true, Log: quiet, TrustedProxies: []netip.Prefix{
		netip.MustParsePrefix("172.18.0.0/16"), netip.MustParsePrefix("10.9.9.9/32"),
	}})
	cases := []struct {
		remote string
		hdr    map[string]string
		want   string
	}{
		{"203.0.113.5:4000", map[string]string{"X-Forwarded-For": "1.2.3.4"}, "203.0.113.5"},
		{"203.0.113.5:4000", map[string]string{"CF-Connecting-IP": "1.2.3.4"}, "203.0.113.5"},
		{"172.18.0.3:4000", map[string]string{"CF-Connecting-IP": "198.51.100.7"}, "198.51.100.7"},
		{"172.18.0.3:4000", map[string]string{"X-Forwarded-For": "1.2.3.4, 198.51.100.7, 10.9.9.9"}, "198.51.100.7"},
		{"172.18.0.3:4000", map[string]string{"X-Forwarded-For": "10.9.9.9"}, "10.9.9.9"},
		{"172.18.0.3:4000", nil, "172.18.0.3"},
		{"[::ffff:172.18.0.3]:4000", map[string]string{"X-Forwarded-For": "198.51.100.8"}, "198.51.100.8"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/ws", nil)
		r.RemoteAddr = c.remote
		for k, v := range c.hdr {
			r.Header.Set(k, v)
		}
		if got := s.clientIP(r); got != c.want {
			t.Errorf("%s %v: client %s, want %s", c.remote, c.hdr, got, c.want)
		}
	}
	// Without TrustProxy, headers never count.
	plain := New(Config{Log: quiet})
	r := httptest.NewRequest("GET", "/ws", nil)
	r.RemoteAddr = "172.18.0.3:4000"
	r.Header.Set("CF-Connecting-IP", "1.2.3.4")
	if got := plain.clientIP(r); got != "172.18.0.3" {
		t.Errorf("no TrustProxy: %s", got)
	}
}

func TestUnseatedWSTimeout(t *testing.T) {
	s := New(Config{Log: quietLog()})
	s.firstMsgTimeout = 80 * time.Millisecond
	url := serve(t, s)

	// Idle socket: closed shortly after firstMsgTimeout.
	idle := dial(t, url)
	rctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, _, err := idle.ws.Read(rctx); err == nil || rctx.Err() != nil {
		t.Fatalf("idle unseated socket stayed open: %v", err)
	}

	// Bad message doesn't reset or clear the unseated deadline.
	bad := dial(t, url)
	bad.send(map[string]any{"t": "bogus"})
	if e := bad.expect(proto.TError); e["code"] != proto.ErrBadRequest {
		t.Fatalf("bad first msg: %v", e)
	}
	rctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if _, _, err := bad.ws.Read(rctx2); err == nil || rctx2.Err() != nil {
		t.Fatalf("unseated socket after error stayed open: %v", err)
	}

	// Seated socket stays alive well past firstMsgTimeout.
	seated := dial(t, url)
	seated.send(proto.Create{T: proto.TCreate, Password: pw, Name: "alice"})
	seated.expect(proto.TWelcome)
	time.Sleep(180 * time.Millisecond)
	seated.send(proto.Signal{T: proto.TSignal, To: "nobody", Data: []byte(`{}`)})
	if e := seated.expect(proto.TError); e["code"] != proto.ErrNoPeer {
		t.Fatalf("seated socket after timeout window: %v", e)
	}
}

func TestWSConnLimits(t *testing.T) {
	s := New(Config{TrustProxy: true, MaxConnsPerIP: 2, MaxConns: 3, Log: quietLog()})
	url := serve(t, s)
	dialFrom := func(ip string) (*websocket.Conn, int) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ws, resp, err := websocket.Dial(ctx, url+"/ws", &websocket.DialOptions{
			HTTPHeader: http.Header{"X-Forwarded-For": {ip}},
		})
		if err != nil {
			if resp == nil {
				t.Fatal(err)
			}
			return nil, resp.StatusCode
		}
		t.Cleanup(func() { ws.CloseNow() })
		return ws, 0
	}

	ws1, st := dialFrom("198.51.100.1")
	if st != 0 {
		t.Fatalf("conn 1: %d", st)
	}
	if _, st := dialFrom("198.51.100.1"); st != 0 {
		t.Fatalf("conn 2: %d", st)
	}
	if _, st := dialFrom("198.51.100.1"); st != http.StatusTooManyRequests {
		t.Fatalf("3rd conn from same IP: status %d, want 429", st)
	}
	if _, st := dialFrom("198.51.100.2"); st != 0 {
		t.Fatalf("conn 3 (other IP): %d", st)
	}
	if _, st := dialFrom("198.51.100.3"); st != http.StatusServiceUnavailable {
		t.Fatalf("4th conn over total cap: status %d, want 503", st)
	}

	ws1.CloseNow()
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.connMu.Lock()
		n := s.conns
		s.connMu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("conns did not drop after close: %d", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, st := dialFrom("198.51.100.1"); st != 0 {
		t.Fatalf("after closing one conn: status %d", st)
	}
}

func TestPasswordCheckRateLimit(t *testing.T) {
	s := New(Config{TrustProxy: true, Log: quietLog()})
	url := serve(t, s)
	dialFrom := func(ip string) *client {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ws, _, err := websocket.Dial(ctx, url+"/ws", &websocket.DialOptions{
			HTTPHeader: http.Header{"X-Forwarded-For": {ip}},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ws.CloseNow() })
		return &client{t: t, ws: ws}
	}

	creator := dialFrom("10.0.0.1")
	creator.send(proto.Create{T: proto.TCreate, Password: pw, Name: "alice"})
	code := creator.expect(proto.TWelcome)["room"].(string)

	// Correct-password joins never touch failIP, yet pwIP caps them at 30/min.
	for i := range 30 {
		c := dialFrom("10.0.0.2")
		c.send(proto.Join{T: proto.TJoin, Room: code, Password: pw, Name: "bob"})
		c.expect(proto.TWelcome)
		creator.expect(proto.TPeerJoined)
		c.send(proto.Leave{T: proto.TLeave})
		creator.expect(proto.TPeerLeft)
		c.ws.CloseNow()
		_ = i
	}
	c31 := dialFrom("10.0.0.2")
	c31.send(proto.Join{T: proto.TJoin, Room: code, Password: pw, Name: "bob"})
	if e := c31.expect(proto.TError); e["code"] != proto.ErrRateLimited {
		t.Fatalf("31st password check: %v", e)
	}
	p31 := dialFrom("10.0.0.2")
	p31.send(proto.Probe{T: proto.TProbe, Room: code, Password: pw})
	if e := p31.expect(proto.TError); e["code"] != proto.ErrRateLimited {
		t.Fatalf("probe from pwIP-blocked IP: %v", e)
	}
	other := dialFrom("10.0.0.3")
	other.send(proto.Join{T: proto.TJoin, Room: code, Password: pw, Name: "carol"})
	other.expect(proto.TWelcome)
}

func TestReportLimitsBudgetAndEscaping(t *testing.T) {
	dir := t.TempDir()
	var logBuf bytes.Buffer
	var logMu sync.Mutex
	logger := slog.New(slog.NewJSONHandler(writerFunc(func(p []byte) (int, error) {
		logMu.Lock()
		defer logMu.Unlock()
		return logBuf.Write(p)
	}), nil))
	s := New(Config{TrustProxy: true, ReportDir: dir, ReportMaxBytes: 900, Log: logger})
	ts := httptest.NewServer(s)
	defer ts.Close()

	postIP := func(ip string, rep map[string]any) int {
		b, _ := json.Marshal(rep)
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/report", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", ip)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	baseRep := func() map[string]any {
		return map[string]any{"platform": "linux", "build": 1, "kind": "crash", "log": "ok"}
	}

	// Field byte caps: 256 for short fields, 8 KiB for Note.
	for _, f := range []string{"name", "kind", "version", "osVersion", "platform", "file"} {
		r := baseRep()
		r[f] = strings.Repeat("a", 257)
		if got := postIP("10.1.0.1", r); got != http.StatusBadRequest {
			t.Fatalf("%s 257 bytes: status %d, want 400", f, got)
		}
	}
	rBadNote := baseRep()
	rBadNote["note"] = strings.Repeat("n", (8<<10)+1)
	if got := postIP("10.1.0.1", rBadNote); got != http.StatusBadRequest {
		t.Fatalf("note > 8 KiB: status %d, want 400", got)
	}

	// SetEscapeHTML(false) and 64-byte log truncation (valid UTF-8 across multi-byte rune).
	longName := strings.Repeat("a", 63) + "é" + strings.Repeat("b", 40) // 'é' starts at byte 63 (2 bytes -> cut at 63)
	longKind := strings.Repeat("k", 100)
	sEsc := New(Config{TrustProxy: true, ReportDir: t.TempDir(), Log: logger})
	tsEsc := httptest.NewServer(sEsc)
	defer tsEsc.Close()
	rOK := map[string]any{
		"platform": "linux", "build": 2, "kind": longKind, "name": longName,
		"version": strings.Repeat("v", 256), "osVersion": strings.Repeat("o", 256),
		"file": strings.Repeat("f", 256), "note": strings.Repeat("n", 8<<10),
		"log": "panic: a < b && c > d",
	}
	bOK, _ := json.Marshal(rOK)
	res, err := http.Post(tsEsc.URL+"/v1/report", "application/json", bytes.NewReader(bOK))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("valid max-field report: status %d, want 202", res.StatusCode)
	}
	escFiles, _ := filepath.Glob(filepath.Join(sEsc.cfg.ReportDir, "*.json"))
	if len(escFiles) != 1 {
		t.Fatalf("esc files: %v", escFiles)
	}
	raw, _ := os.ReadFile(escFiles[0])
	if !bytes.Contains(raw, []byte("panic: a < b && c > d")) || bytes.Contains(raw, []byte(`\u003c`)) || bytes.Contains(raw, []byte(`\u0026`)) {
		t.Fatalf("HTML escaped in stored report: %s", raw)
	}
	logMu.Lock()
	var logEntry map[string]any
	json.Unmarshal(logBuf.Bytes(), &logEntry)
	logMu.Unlock()
	gotName, _ := logEntry["name"].(string)
	gotKind, _ := logEntry["kind"].(string)
	if len(gotName) > 64 || !utf8.ValidString(gotName) || gotName != strings.Repeat("a", 63) {
		t.Fatalf("logged name = %q (len %d), want 63 'a's", gotName, len(gotName))
	}
	if len(gotKind) != 64 || gotKind != strings.Repeat("k", 64) {
		t.Fatalf("logged kind = %q (len %d), want 64 'k's", gotKind, len(gotKind))
	}

	// Disk budget (s has ReportMaxBytes: 900): oversized single report -> 503;
	// sequential and concurrent reports evict oldest first and stay within budget.
	if got := postIP("10.1.0.2", map[string]any{"platform": "linux", "build": 1, "kind": "crash", "log": strings.Repeat("x", 1000)}); got != http.StatusServiceUnavailable {
		t.Fatalf("report larger than ReportMaxBytes: status %d, want 503", got)
	}

	// Each small report is ~160 bytes; 900 bytes fits ~5, so posting 3 sequentially with spaced mtimes
	// under a tighter 450-byte budget keeps the newest 2 and deletes the oldest.
	dirSeq := t.TempDir()
	sSeq := New(Config{TrustProxy: true, ReportDir: dirSeq, ReportMaxBytes: 420, Log: quietLog()})
	tsSeq := httptest.NewServer(sSeq)
	defer tsSeq.Close()
	postSeq := func(tag string, mtime time.Time) {
		b, _ := json.Marshal(map[string]any{"platform": "linux", "build": 1, "kind": tag, "log": strings.Repeat("z", 40)})
		res, err := http.Post(tsSeq.URL+"/v1/report", "application/json", bytes.NewReader(b))
		if err != nil || res.StatusCode != http.StatusAccepted {
			t.Fatalf("post %s: %v %v", tag, res, err)
		}
		res.Body.Close()
		fs, _ := filepath.Glob(filepath.Join(dirSeq, "*-"+tag+"-*.json"))
		if len(fs) == 1 {
			os.Chtimes(fs[0], mtime, mtime)
		}
	}
	base := time.Now().Add(-time.Minute)
	postSeq("first", base)
	postSeq("second", base.Add(10*time.Second))
	postSeq("third", base.Add(20*time.Second))

	kept, _ := filepath.Glob(filepath.Join(dirSeq, "*.json"))
	var names string
	var seqBytes int64
	for _, f := range kept {
		names += filepath.Base(f) + " "
		if st, err := os.Stat(f); err == nil {
			seqBytes += st.Size()
		}
	}
	if strings.Contains(names, "-first-") || !strings.Contains(names, "-third-") || seqBytes > 420 {
		t.Fatalf("after budget eviction: files=%q bytes=%d", names, seqBytes)
	}

	// Concurrent requests cannot exceed ReportMaxBytes.
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			postIP(fmt.Sprintf("10.2.0.%d", i+1), map[string]any{
				"platform": "linux", "build": i, "kind": "user",
				"log": strings.Repeat("m", 80),
				"zip": []byte("PK\x03\x04zzzzzzzz"),
			})
		}(i)
	}
	wg.Wait()
	all, _ := os.ReadDir(dir)
	var total int64
	for _, e := range all {
		if st, err := e.Info(); err == nil {
			total += st.Size()
		}
	}
	if total == 0 || total > 900 {
		t.Fatalf("concurrent report bytes = %d, want 1..900", total)
	}
}

func TestRunSweepsAllLimiters(t *testing.T) {
	s := New(Config{DiscordClientID: "42", DiscordSecret: "shh", DiscordBotToken: "bot", Log: quietLog()})
	s.sweepEvery = 15 * time.Millisecond
	s.lookIP.Period = 5 * time.Millisecond
	s.pwIP.Period = 5 * time.Millisecond
	// Point the bot at a local server so Run doesn't dial Discord.
	fake := httptest.NewServer(http.NotFoundHandler())
	defer fake.Close()
	s.discord.bot.APIURL = fake.URL

	s.lookIP.Add("1.1.1.1")
	s.pwIP.Add("2.2.2.2")
	s.discord.mu.Lock()
	s.discord.pending["expired"] = pendingLink{key: "k1", expires: time.Now().Add(-time.Second)}
	s.discord.pendingByKey["k1"] = "expired"
	s.discord.mu.Unlock()

	if s.lookIP.Keys() != 1 || s.pwIP.Keys() != 1 {
		t.Fatal("limiters did not record keys")
	}
	time.Sleep(10 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		s.discord.mu.Lock()
		n := len(s.discord.pending)
		s.discord.mu.Unlock()
		if n == 0 && s.lookIP.Keys() == 0 && s.pwIP.Keys() == 0 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("Run sweep incomplete: pending=%d lookIP=%d pwIP=%d", n, s.lookIP.Keys(), s.pwIP.Keys())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
