package ui

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

type fake struct {
	Broadcaster
	created int
}

func (f *fake) State() any                             { return map[string]int{"created": f.created} }
func (f *fake) Create(context.Context, string) error   { f.created++; return nil }
func (f *fake) JoinLink(context.Context, string) error { return nil }
func (f *fake) Leave()                                 {}
func (f *fake) SetSource(string) error                 { return nil }

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

func (f *fake) AudioOptions() any                           { return nil }
func (f *fake) SetAudio(string, []string) error             { return nil }
func (f *fake) SetOutput(string) error                      { return nil }
func (f *fake) CheckUpdate(context.Context) error           { return nil }
func (f *fake) InstallUpdate(context.Context) error         { return nil }
func (f *fake) DismissUpdateResult()                        {}
func (f *fake) SetAutostart(bool) error                     { return nil }
func (f *fake) Sources() any                                { return nil }
func (f *fake) SetPeer(string, string, float64, bool) error { return nil }
func (f *fake) Volunteer(bool) error                        { return nil }
func (f *fake) Transfer(string) error                       { return nil }
func (f *fake) SetName(string) error                        { return nil }
func (f *fake) Kick(string, bool) error                     { return nil }
func (f *fake) SetResolution(string) error                  { return nil }
func (f *fake) SetLayout(string, int, int) error            { return nil }
func (f *fake) SendLogsNow(context.Context) error           { return nil }
func (f *fake) OpenLogFolder() error                        { return nil }
