package ui

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type fake struct {
	Broadcaster
	created   int
	watched   []string
	autoWatch []bool
	peerQual  string
}

func (f *fake) State() any                                 { return map[string]int{"created": f.created} }
func (f *fake) Create(context.Context, string, bool) error { f.created++; return nil }
func (f *fake) JoinLink(context.Context, string) error     { return nil }
func (f *fake) ShowUI()                                    {}
func (f *fake) Leave()                                     {}
func (f *fake) SetSource(string) error                     { return nil }

func start(t *testing.T) (*Server, *fake, string) {
	t.Helper()
	be := &fake{}
	s, err := Start(be, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, be, "http://127.0.0.1:" + strconv.Itoa(s.Port)
}

func noRedirect() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func do(t *testing.T, c *http.Client, method, u string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, u, strings.NewReader("{}"))
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res
}

func TestRejectsForeignHost(t *testing.T) {
	_, _, base := start(t)
	res := do(t, http.DefaultClient, "GET", base+"/", map[string]string{"Host": "evil.example:80"})
	if res.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("DNS-rebinding host got %d", res.StatusCode)
	}
}

func TestTokenCookieFlow(t *testing.T) {
	s, be, base := start(t)
	c := noRedirect()

	if r := do(t, c, "GET", base+"/api/state", nil); r.StatusCode != 401 {
		t.Fatalf("state without cookie: %d", r.StatusCode)
	}
	if r := do(t, c, "GET", base+"/?t=wrong", nil); r.StatusCode != 401 {
		t.Fatalf("wrong token: %d", r.StatusCode)
	}
	r := do(t, c, "GET", s.URL(), nil)
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/" {
		t.Fatalf("token exchange: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	ck := r.Cookies()
	if len(ck) != 1 || !ck[0].HttpOnly || ck[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie: %+v", ck)
	}
	cookie := map[string]string{"Cookie": ck[0].Name + "=" + ck[0].Value}

	if r := do(t, c, "GET", base+"/", cookie); r.StatusCode != 200 {
		t.Fatalf("index with cookie: %d", r.StatusCode)
	}
	// POST without the custom header = what a cross-site form could send.
	if r := do(t, c, "POST", base+"/api/create", cookie); r.StatusCode != 403 || be.created != 0 {
		t.Fatalf("create without X-Blite: %d created=%d", r.StatusCode, be.created)
	}
	hdr := map[string]string{"Cookie": cookie["Cookie"], "X-Blite": "1"}
	if r := do(t, c, "POST", base+"/api/create", hdr); r.StatusCode != 200 || be.created != 1 {
		t.Fatalf("create: %d created=%d", r.StatusCode, be.created)
	}
	// Second-instance path: bearer token.
	if r := do(t, c, "POST", base+"/api/join", map[string]string{"Authorization": "Bearer " + s.Token, "X-Blite": "1"}); r.StatusCode != 200 {
		t.Fatalf("bearer join: %d", r.StatusCode)
	}
	if r := do(t, c, "POST", base+"/api/join", map[string]string{"Authorization": "Bearer nope", "X-Blite": "1"}); r.StatusCode != 401 {
		t.Fatalf("bad bearer: %d", r.StatusCode)
	}
	_ = url.Values{}
}

func (f *fake) AudioOptions() any                                    { return nil }
func (f *fake) SetAudio(string, []string) error                      { return nil }
func (f *fake) SetOutput(string) error                               { return nil }
func (f *fake) CheckUpdate(context.Context) error                    { return nil }
func (f *fake) InstallUpdate(context.Context) error                  { return nil }
func (f *fake) DismissUpdateResult()                                 {}
func (f *fake) SetAutostart(bool) error                              { return nil }
func (f *fake) Sources() any                                         { return nil }
func (f *fake) SetPeer(_ string, q string, _ float64, _ bool) error  { f.peerQual = q; return nil }
func (f *fake) Volunteer(bool) error                                 { return nil }
func (f *fake) Transfer(string) error                                { return nil }
func (f *fake) SetName(string) error                                 { return nil }
func (f *fake) Kick(string, bool) error                              { return nil }
func (f *fake) SetResolution(string) error                           { return nil }
func (f *fake) SetMode(string) error                                 { return nil }
func (f *fake) SetShareOptions(int, string, int, bool, int) error    { return nil }
func (f *fake) SetRoomSecurity(bool, string, bool) error             { return nil }
func (f *fake) AddQualityPreset(string, int) error                   { return nil }
func (f *fake) RemoveQualityPreset(string, int) error                { return nil }
func (f *fake) Diagnostics() string                                  { return "" }
func (f *fake) Feedback(context.Context, string, string, bool) error { return nil }
func (f *fake) ToggleWatch(id string) error                          { f.watched = append(f.watched, id); return nil }
func (f *fake) PopOut(string) error                                  { return nil }
func (f *fake) Pin(string) error                                     { return nil }
func (f *fake) SetAutoWatch(on bool) error                           { f.autoWatch = append(f.autoWatch, on); return nil }
func (f *fake) SetPlaySounds(bool) error                             { return nil }
func (f *fake) SetPortMap(bool) error                                { return nil }
func (f *fake) DismissLeavePrompt()                                  {}
func (f *fake) SendLogsNow(context.Context) error                    { return nil }
func (f *fake) OpenLogFolder() error                                 { return nil }
func (f *fake) ProbeRecentRooms()                                    {}
func (f *fake) JoinRecentRoom(context.Context, string, string) error { return nil }
func (f *fake) RemoveRecentRoom(string, string) error                { return nil }

func TestLegacyWebUIUpdatedAPI(t *testing.T) {
	s, be, base := start(t)
	c := noRedirect()
	r := do(t, c, "GET", s.URL(), nil)
	ck := r.Cookies()[0]
	hdr := map[string]string{"Cookie": ck.Name + "=" + ck.Value, "X-Blite": "1"}

	fetchText := func(path string) string {
		t.Helper()
		req, _ := http.NewRequest("GET", base+path, nil)
		req.Header.Set("Cookie", hdr["Cookie"])
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("GET %s = %d", path, res.StatusCode)
		}
		b, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	appJS := fetchText("/static/app.js")
	indexHTML := fetchText("/")

	for _, dead := range []string{"/api/layout", "/api/window", "Off (audio only)", `["off"`} {
		if strings.Contains(appJS, dead) {
			t.Errorf("app.js still contains obsolete reference %q", dead)
		}
	}
	for _, dead := range []string{"showGrid", "gridRows", "gridCols", `name="layout"`} {
		if strings.Contains(indexHTML, dead) {
			t.Errorf("index.html still contains obsolete element %q", dead)
		}
	}
	for _, want := range []string{"/api/watch", "/api/autowatch", `["auto", "Best"]`, `["low", "Low"]`} {
		if !strings.Contains(appJS, want) {
			t.Errorf("app.js missing expected control %q", want)
		}
	}

	postBody := func(path, body string) int {
		t.Helper()
		req, _ := http.NewRequest("POST", base+path, strings.NewReader(body))
		req.Header.Set("Cookie", hdr["Cookie"])
		req.Header.Set("X-Blite", "1")
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if code := postBody("/api/watch", `{"id":"peer-1"}`); code != 200 || len(be.watched) != 1 || be.watched[0] != "peer-1" {
		t.Fatalf("POST /api/watch = %d, watched=%v", code, be.watched)
	}
	if code := postBody("/api/autowatch", `{"on":false}`); code != 200 || len(be.autoWatch) != 1 || be.autoWatch[0] {
		t.Fatalf("POST /api/autowatch = %d, autoWatch=%v", code, be.autoWatch)
	}
	if code := postBody("/api/layout", `{}`); code != 404 {
		t.Fatalf("POST /api/layout = %d, want 404", code)
	}
	if code := postBody("/api/window", `{"id":"*"}`); code != 404 {
		t.Fatalf("POST /api/window = %d, want 404", code)
	}
}

// panicky's ShowUI panics, like a handler bug.
type panicky struct{ fake }

func (*panicky) ShowUI() { panic("boom in handler") }

// syncBuf is a bytes.Buffer safe for the server's goroutines.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// A handler panic is one Warn record in our log, not a raw traceback whose
// "goroutine N [running]:" line looks like a crash to the crash reporter.
func TestHandlerPanicLoggedViaSlog(t *testing.T) {
	var buf syncBuf
	s, err := Start(&panicky{}, 0, slog.New(slog.NewTextHandler(&buf, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	req, _ := http.NewRequest("POST", "http://127.0.0.1:"+strconv.Itoa(s.Port)+"/api/show", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("X-Blite", "1")
	if res, err := http.DefaultClient.Do(req); err == nil {
		res.Body.Close()
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "boom in handler") {
		if time.Now().After(deadline) {
			t.Fatalf("panic not logged: %q", buf.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	out := buf.String()
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if !strings.HasPrefix(l, "time=") {
			t.Fatalf("raw line in the log: %q", l)
		}
	}
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "http: panic serving") {
		t.Fatalf("panic not a Warn record: %q", out)
	}
}
