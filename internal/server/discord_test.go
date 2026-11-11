package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
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
