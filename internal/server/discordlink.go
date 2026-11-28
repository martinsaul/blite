package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"blite/internal/discordbot"
	"blite/internal/identity"
	"blite/internal/proto"
	"blite/internal/ratelimit"
)

// Discord links: a user ties their install (identity key) to their Discord
// account once, in the browser (OAuth2, scope identify). While their app
// is connected to GET /v1/discord/follow, the bot follows their voice
// channel; nobody else's (discordbot).

// discordAPI is Discord's REST API (a var for tests).
var discordAPI = "https://discord.com/api/v10"

// discordLink is one install's link: their Discord user id and name.
type discordLink struct {
	User string    `json:"user"`
	Name string    `json:"name,omitempty"`
	At   time.Time `json:"at"`
}

// discordLinks are the links, by identity key, kept in a file when there
// is one (blue and green share it: a link made on one is read by the other
// when it changes).
type discordLinks struct {
	file string

	mu    sync.Mutex
	links map[string]discordLink
	mtime time.Time
}

func (l *discordLinks) reloadLocked() {
	if l.file == "" {
		return
	}
	fi, err := os.Stat(l.file)
	if err != nil || fi.ModTime().Equal(l.mtime) {
		return
	}
	b, err := os.ReadFile(l.file)
	if err != nil {
		return
	}
	var m map[string]discordLink
	if json.Unmarshal(b, &m) == nil {
		l.links, l.mtime = m, fi.ModTime()
	}
}

func (l *discordLinks) get(key string) (discordLink, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reloadLocked()
	k, ok := l.links[key]
	return k, ok
}

// set links key to dl, or unlinks it (dl nil). It reports whether the link
// state changed.
func (l *discordLinks) set(key string, dl *discordLink) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reloadLocked()
	if l.links == nil {
		l.links = map[string]discordLink{}
	}
	if dl != nil {
		l.links[key] = *dl
	} else {
		if _, ok := l.links[key]; !ok {
			return false, nil
		}
		delete(l.links, key)
	}
	if l.file == "" {
		return true, nil
	}
	b, _ := json.Marshal(l.links)
	tmp, err := os.CreateTemp(filepath.Dir(l.file), ".discord-links-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmp.Name(), l.file); err != nil {
		return false, err
	}
	if fi, err := os.Stat(l.file); err == nil {
		l.mtime = fi.ModTime()
	}
	return true, nil
}

// follower is one connected app.
type follower struct {
	key  string
	user string // Discord user id; "" = not linked
	out  chan proto.DiscordMsg
}

func (f *follower) send(m proto.DiscordMsg) {
	select {
	case f.out <- m:
	default: // a stuck client misses an update; the next one replaces it
	}
}

const (
	maxDiscordPending = 10000
	discordMsgPerMin  = 5
)

// discordFollow is the server's side of Discord following.
type discordFollow struct {
	bot        *discordbot.Bot
	links      *discordLinks
	maxPending int

	mu           sync.Mutex
	byUser       map[string]map[*follower]bool // linked followers
	byKey        map[string]map[*follower]bool // all followers
	pending      map[string]pendingLink        // OAuth state -> who asked
	pendingByKey map[string]string             // identity key -> OAuth state
}

type pendingLink struct {
	key     string
	expires time.Time
}

func newDiscordFollow(s *Server) *discordFollow {
	file := s.cfg.DiscordLinksFile
	if file == "" && s.cfg.StateFile != "" {
		file = filepath.Join(filepath.Dir(s.cfg.StateFile), "discord-links.json")
	}
	d := &discordFollow{
		links:        &discordLinks{file: file},
		maxPending:   maxDiscordPending,
		byUser:       map[string]map[*follower]bool{},
		byKey:        map[string]map[*follower]bool{},
		pending:      map[string]pendingLink{},
		pendingByKey: map[string]string{},
	}
	d.bot = &discordbot.Bot{Token: s.cfg.DiscordBotToken, Log: s.cfg.Log, Active: d.active, OnVoice: d.onVoice}
	return d
}

func (d *discordFollow) sweepPending() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sweepPendingLocked(time.Now())
}

func (d *discordFollow) sweepPendingLocked(now time.Time) {
	for st, p := range d.pending {
		if now.After(p.expires) {
			delete(d.pending, st)
			if d.pendingByKey[p.key] == st {
				delete(d.pendingByKey, p.key)
			}
		}
	}
}

// active: someone with this Discord id has a linked app connected now.
func (d *discordFollow) active(user string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.byUser[user]) > 0
}

func (d *discordFollow) onVoice(user string, v *discordbot.Voice) {
	d.mu.Lock()
	fs := make([]*follower, 0, len(d.byUser[user]))
	for f := range d.byUser[user] {
		fs = append(fs, f)
	}
	d.mu.Unlock()
	for _, f := range fs {
		f.send(d.state(f))
	}
}

// state is what f is told: its link and, if linked, the voice channel.
func (d *discordFollow) state(f *follower) proto.DiscordMsg {
	m := proto.DiscordMsg{T: proto.TDiscordState}
	l, ok := d.links.get(f.key)
	if !ok {
		return m
	}
	m.Linked, m.User = true, l.Name
	if v, ok := d.bot.Voice(l.User); ok {
		m.Voice = &proto.DiscordVoice{GuildID: v.GuildID, Guild: v.Guild, ChannelID: v.ChannelID, Channel: v.Channel}
	}
	return m
}

// follow adds f under user (""= not linked); the first app of a user
// starts following them.
func (d *discordFollow) follow(ctx context.Context, f *follower, user string) {
	d.mu.Lock()
	if d.byKey[f.key] == nil {
		d.byKey[f.key] = map[*follower]bool{}
	}
	d.byKey[f.key][f] = true
	f.user = user
	first := false
	if user != "" {
		if d.byUser[user] == nil {
			d.byUser[user] = map[*follower]bool{}
			first = true
		}
		d.byUser[user][f] = true
	}
	d.mu.Unlock()
	f.send(d.state(f))
	if first {
		go d.bot.Activate(context.WithoutCancel(ctx), user)
	}
}

// unfollow removes f; once a user's last app is gone, the bot forgets them.
func (d *discordFollow) unfollow(f *follower) {
	d.mu.Lock()
	delete(d.byKey[f.key], f)
	if len(d.byKey[f.key]) == 0 {
		delete(d.byKey, f.key)
	}
	user, last := f.user, false
	if user != "" {
		delete(d.byUser[user], f)
		if len(d.byUser[user]) == 0 {
			delete(d.byUser, user)
			last = true
		}
	}
	f.user = ""
	d.mu.Unlock()
	if last {
		d.bot.Forget(user)
	}
}

// relink moves every app of key to its current link (after linking or
// unlinking).
func (d *discordFollow) relink(ctx context.Context, key string) {
	l, _ := d.links.get(key)
	d.mu.Lock()
	fs := make([]*follower, 0, len(d.byKey[key]))
	for f := range d.byKey[key] {
		fs = append(fs, f)
	}
	d.mu.Unlock()
	for _, f := range fs {
		d.unfollow(f)
		d.follow(ctx, f, l.User)
	}
}

func randomToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// discordRedirect is where Discord sends the browser back after linking.
func (s *Server) discordRedirect() string {
	if s.cfg.DiscordLinkRedirect != "" {
		return s.cfg.DiscordLinkRedirect
	}
	return siteURL + "/v1/discord/linked"
}

// linkURL starts linking key: the Discord page to open in a browser, or ""
// when the pending map is full.
func (s *Server) linkURL(key string) string {
	d := s.discord
	st := randomToken()
	now := time.Now()
	d.mu.Lock()
	if old, ok := d.pendingByKey[key]; ok {
		delete(d.pending, old)
		delete(d.pendingByKey, key)
	}
	if d.maxPending > 0 && len(d.pending) >= d.maxPending {
		d.sweepPendingLocked(now)
		if len(d.pending) >= d.maxPending {
			d.mu.Unlock()
			return ""
		}
	}
	d.pending[st] = pendingLink{key: key, expires: now.Add(15 * time.Minute)}
	d.pendingByKey[key] = st
	d.mu.Unlock()
	q := url.Values{
		"client_id":     {s.cfg.DiscordClientID},
		"response_type": {"code"},
		"scope":         {"identify"},
		"redirect_uri":  {s.discordRedirect()},
		"state":         {st},
	}
	return "https://discord.com/oauth2/authorize?" + q.Encode()
}

// handleDiscordFollow is the app's follow socket (proto.DiscordMsg).
func (s *Server) handleDiscordFollow(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if s.lookIP.Blocked(ip) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	s.lookIP.Add(ip)
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer ws.CloseNow()
	ws.SetReadLimit(4096)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	write := func(m proto.DiscordMsg) error {
		b, _ := json.Marshal(m)
		wctx, c := context.WithTimeout(ctx, writeTimeout)
		defer c()
		return ws.Write(wctx, websocket.MessageText, b)
	}
	read := func() (proto.DiscordMsg, error) {
		var m proto.DiscordMsg
		_, b, err := ws.Read(ctx)
		if err == nil {
			err = json.Unmarshal(b, &m)
		}
		return m, err
	}

	challenge := make([]byte, 32)
	rand.Read(challenge)
	if write(proto.DiscordMsg{T: proto.TDiscordHello, Challenge: base64.StdEncoding.EncodeToString(challenge)}) != nil {
		return
	}
	actx, acancel := context.WithTimeout(ctx, 20*time.Second)
	_, b, err := ws.Read(actx)
	acancel()
	var auth proto.DiscordMsg
	if err != nil || json.Unmarshal(b, &auth) != nil || auth.T != proto.TDiscordAuth {
		ws.Close(websocket.StatusPolicyViolation, "auth expected")
		return
	}
	sig, _ := base64.StdEncoding.DecodeString(auth.Sig)
	if identity.Verify(auth.Key, proto.DiscordFollowPurpose, challenge, sig) != nil {
		ws.Close(websocket.StatusPolicyViolation, "bad signature")
		return
	}

	d := s.discord
	f := &follower{key: auth.Key, out: make(chan proto.DiscordMsg, 8)}
	l, _ := d.links.get(f.key)
	d.follow(ctx, f, l.User)
	defer d.unfollow(f)

	go func() {
		t := time.NewTicker(pingInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case m := <-f.out:
				if write(m) != nil {
					cancel()
					return
				}
			case <-t.C:
				pctx, c := context.WithTimeout(ctx, writeTimeout)
				err := ws.Ping(pctx)
				c()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()

	linkLim := ratelimit.New(discordMsgPerMin, time.Minute)
	unlinkLim := ratelimit.New(discordMsgPerMin, time.Minute)
	for {
		m, err := read()
		if err != nil {
			return
		}
		switch m.T {
		case proto.TDiscordLink:
			if linkLim.Blocked("") {
				continue
			}
			linkLim.Add("")
			if u := s.linkURL(f.key); u != "" {
				f.send(proto.DiscordMsg{T: proto.TDiscordState, LinkURL: u})
			} else {
				st := d.state(f)
				st.Error = proto.ErrRateLimited
				f.send(st)
			}
		case proto.TDiscordUnlink:
			if unlinkLim.Blocked("") {
				continue
			}
			unlinkLim.Add("")
			if _, err := d.links.set(f.key, nil); err != nil {
				s.cfg.Log.Error("discord unlink", "err", err)
			}
			d.relink(ctx, f.key)
		}
	}
}

// handleDiscordLinked is where Discord sends the browser back: it ties the
// account to the install that asked and tells its app.
func (s *Server) handleDiscordLinked(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if s.lookIP.Blocked(ip) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	s.lookIP.Add(ip)
	d := s.discord
	st := r.URL.Query().Get("state")
	d.mu.Lock()
	p, ok := d.pending[st]
	if ok {
		delete(d.pending, st)
		if d.pendingByKey[p.key] == st {
			delete(d.pendingByKey, p.key)
		}
	}
	d.mu.Unlock()
	if !ok || time.Now().After(p.expires) {
		linkedPage(w, http.StatusBadRequest, "This link has expired. Click Connect Discord in Bellion Share again.")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		linkedPage(w, http.StatusBadRequest, "Discord wasn't connected. You can close this tab.")
		return
	}
	user, name, err := s.discordIdentify(r.Context(), code)
	if err != nil {
		s.cfg.Log.Warn("discord link", "err", err)
		linkedPage(w, http.StatusBadGateway, "Discord didn't answer. Click Connect Discord in Bellion Share to try again.")
		return
	}
	if _, err := d.links.set(p.key, &discordLink{User: user, Name: name, At: time.Now().UTC()}); err != nil {
		s.cfg.Log.Error("discord link", "err", err)
		linkedPage(w, http.StatusInternalServerError, "The server couldn't save the link. Please try again later.")
		return
	}
	d.relink(r.Context(), p.key)
	linkedPage(w, http.StatusOK, "Discord is connected. You can close this tab and go back to Bellion Share.")
}

// discordIdentify exchanges an OAuth code for the user's Discord id and name.
func (s *Server) discordIdentify(ctx context.Context, code string) (id, name string, err error) {
	f := url.Values{
		"client_id": {s.cfg.DiscordClientID}, "client_secret": {s.cfg.DiscordSecret},
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {s.discordRedirect()},
	}
	c := &http.Client{Timeout: 15 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, discordTokenURL, strings.NewReader(f.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := c.Do(req)
	if err != nil {
		return "", "", err
	}
	var tok struct {
		Access string `json:"access_token"`
	}
	err = json.NewDecoder(io.LimitReader(res.Body, 16384)).Decode(&tok)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || err != nil || tok.Access == "" {
		return "", "", errors.New("token exchange: " + res.Status)
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, discordAPI+"/users/@me", nil)
	req.Header.Set("Authorization", "Bearer "+tok.Access)
	res, err = c.Do(req)
	if err != nil {
		return "", "", err
	}
	defer res.Body.Close()
	var me struct {
		ID         string `json:"id"`
		Username   string `json:"username"`
		GlobalName string `json:"global_name"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 16384)).Decode(&me); err != nil || res.StatusCode != http.StatusOK || me.ID == "" {
		return "", "", errors.New("users/@me: " + res.Status)
	}
	name = me.GlobalName
	if name == "" {
		name = me.Username
	}
	return me.ID, name, nil
}

func linkedPage(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	io.WriteString(w, `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Bellion Share</title>
<style>body{background:#0e1016;color:#eef0f7;font:16px system-ui,sans-serif;display:grid;place-items:center;min-height:90vh;margin:0}p{max-width:30em;padding:1em;text-align:center}</style>
</head><body><p>`+html.EscapeString(msg)+`</p></body></html>`)
}
