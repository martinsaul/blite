package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
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
	for _, m := range regexp.MustCompile(`src="(/static/[^"]+)"`).FindAllStringSubmatch(body, -1) {
		r := httptest.NewRequest("GET", m[1], nil)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("image %s: %d", m[1], w.Code)
		}
	}
}
