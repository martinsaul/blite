package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// handleChannel tells people in a Discord voice channel which room is tied
// to it: 404 when none. Anyone can ask: being in the channel is enough,
// the server doesn't check it.
func (s *Server) handleChannel(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if s.lookIP.Blocked(ip) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	s.lookIP.Add(ip)
	cr, ok := s.rooms.Channel(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(cr)
}

// discordTokenURL is Discord's OAuth2 token endpoint (a var for tests).
var discordTokenURL = "https://discord.com/api/oauth2/token"

// DiscordRedirect is the redirect URI registered on the Discord app; RPC
// authorization codes are exchanged with it.
const DiscordRedirect = "http://127.0.0.1"

// handleDiscordToken exchanges a Discord RPC authorization code, or a
// refresh token, for an access token with our client secret, which stays
// on the server. Body: {"code": "..."} or {"refresh": "..."}. The answer is
// Discord's token JSON, passed through.
func (s *Server) handleDiscordToken(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if s.lookIP.Blocked(ip) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	s.lookIP.Add(ip)
	var in struct{ Code, Refresh string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil || (in.Code == "") == (in.Refresh == "") {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	f := url.Values{"client_id": {s.cfg.DiscordClientID}, "client_secret": {s.cfg.DiscordSecret}}
	if in.Code != "" {
		f.Set("grant_type", "authorization_code")
		f.Set("code", in.Code)
		f.Set("redirect_uri", DiscordRedirect)
	} else {
		f.Set("grant_type", "refresh_token")
		f.Set("refresh_token", in.Refresh)
	}
	c := http.Client{Timeout: 15 * time.Second}
	res, err := c.Post(discordTokenURL, "application/x-www-form-urlencoded", strings.NewReader(f.Encode()))
	if err != nil {
		s.cfg.Log.Warn("discord token", "err", err)
		http.Error(w, "discord unreachable", http.StatusBadGateway)
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		s.cfg.Log.Warn("discord token", "status", res.Status, "body", string(b))
		http.Error(w, "discord refused", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	io.Copy(w, io.LimitReader(res.Body, 16384))
}
