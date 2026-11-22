// Package discordbot follows Discord voice channels for Bellion Share users
// through a bot on the Discord Gateway (intents GUILDS and
// GUILD_VOICE_STATES), so their app learns at once which voice channel they
// are in, without polling Discord per user.
//
// Only people Active reports (a linked Bellion Share app is connected right
// now) are followed: every other voice state the Gateway sends is dropped
// as it arrives, never stored and never logged. When someone becomes active
// their current channel is asked for once (REST, per guild the bot is in).
package discordbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Intents: GUILDS (1<<0) for guild and channel names, GUILD_VOICE_STATES
// (1<<7). Neither is privileged.
const Intents = 1<<0 | 1<<7

// Voice is where a followed user is: a voice channel and its guild.
type Voice struct {
	GuildID   string `json:"guildId"`
	Guild     string `json:"guild,omitempty"`
	ChannelID string `json:"channelId"`
	Channel   string `json:"channel,omitempty"`
}

// Bot is one Gateway connection (resumed or re-identified when it drops).
type Bot struct {
	Token string
	Log   *slog.Logger
	// Active reports whether user is followed. Called for every voice state
	// the Gateway sends; must be cheap.
	Active func(user string) bool
	// OnVoice is told when an active user's channel changes (nil: left voice).
	OnVoice func(user string, v *Voice)

	// GatewayURL and APIURL are Discord's (vars for tests).
	GatewayURL string
	APIURL     string
	HTTP       *http.Client

	mu     sync.Mutex
	guilds map[string]*guild
	voice  map[string]Voice  // active users in voice only
	gen    map[string]uint64 // per user: bumped by each Gateway update
}

type guild struct {
	name     string
	channels map[string]string // id -> name
}

func (b *Bot) init() {
	if b.Log == nil {
		b.Log = slog.Default()
	}
	if b.GatewayURL == "" {
		b.GatewayURL = "wss://gateway.discord.gg"
	}
	if b.APIURL == "" {
		b.APIURL = "https://discord.com/api/v10"
	}
	if b.HTTP == nil {
		b.HTTP = &http.Client{Timeout: 15 * time.Second}
	}
	b.mu.Lock()
	if b.guilds == nil {
		b.guilds = map[string]*guild{}
		b.voice = map[string]Voice{}
		b.gen = map[string]uint64{}
	}
	b.mu.Unlock()
}

// Voice returns where an active user is (false: not in voice, or not known).
func (b *Bot) Voice(user string) (Voice, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	v, ok := b.voice[user]
	return v, ok
}

// Forget drops what the bot knows about user (they are no longer active).
func (b *Bot) Forget(user string) {
	b.mu.Lock()
	delete(b.voice, user)
	delete(b.gen, user)
	b.mu.Unlock()
}

// Activate looks up the voice channel of a user who just became active and
// reports it through OnVoice, unless a Gateway update for them came first.
func (b *Bot) Activate(ctx context.Context, user string) {
	b.init()
	b.mu.Lock()
	start := b.gen[user]
	ids := make([]string, 0, len(b.guilds))
	for id := range b.guilds {
		ids = append(ids, id)
	}
	b.mu.Unlock()
	var found *Voice
	for _, gid := range ids {
		ch, err := b.voiceState(ctx, gid, user)
		if err != nil {
			b.Log.Info("discord bot: voice state lookup", "err", err)
			continue
		}
		if ch != "" {
			v := b.named(gid, ch)
			found = &v
			break
		}
	}
	if !b.Active(user) {
		return // they left meanwhile
	}
	b.mu.Lock()
	if b.gen[user] != start {
		b.mu.Unlock()
		return // the Gateway told us since
	}
	if found != nil {
		b.voice[user] = *found
	} else {
		delete(b.voice, user)
	}
	b.mu.Unlock()
	if b.OnVoice != nil {
		b.OnVoice(user, found)
	}
}

// voiceState asks Discord which channel user is in on guild ("" = none).
func (b *Bot) voiceState(ctx context.Context, gid, user string) (string, error) {
	for try := 0; ; try++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, b.APIURL+"/guilds/"+gid+"/voice-states/"+user, nil)
		req.Header.Set("Authorization", "Bot "+b.Token)
		res, err := b.HTTP.Do(req)
		if err != nil {
			return "", err
		}
		body, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		res.Body.Close()
		switch {
		case res.StatusCode == http.StatusOK:
			var vs struct {
				ChannelID string `json:"channel_id"`
			}
			if err := json.Unmarshal(body, &vs); err != nil {
				return "", err
			}
			return vs.ChannelID, nil
		case res.StatusCode == http.StatusNotFound:
			return "", nil // not in voice there (or not a member)
		case res.StatusCode == http.StatusTooManyRequests && try < 2:
			var rl struct {
				RetryAfter float64 `json:"retry_after"`
			}
			json.Unmarshal(body, &rl)
			wait := time.Duration(min(max(rl.RetryAfter, 0.1), 5) * float64(time.Second))
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(wait):
			}
		default:
			return "", fmt.Errorf("voice state: %s", res.Status)
		}
	}
}

// named fills in guild and channel names.
func (b *Bot) named(gid, cid string) Voice {
	b.mu.Lock()
	defer b.mu.Unlock()
	v := Voice{GuildID: gid, ChannelID: cid}
	if g := b.guilds[gid]; g != nil {
		v.Guild, v.Channel = g.name, g.channels[cid]
	}
	return v
}

// Gateway opcodes.
const (
	opDispatch       = 0
	opHeartbeat      = 1
	opIdentify       = 2
	opResume         = 6
	opReconnect      = 7
	opInvalidSession = 9
	opHello          = 10
	opHeartbeatAck   = 11
)

type payload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d,omitempty"`
	S  *int64          `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
}

// errFatal: Discord refused the bot for good (bad token, intents).
type errFatal struct{ code websocket.StatusCode }

func (e errFatal) Error() string { return fmt.Sprintf("gateway closed with %d", e.code) }

// session is what a resume needs.
type session struct {
	id        string
	resumeURL string
	seq       int64
}

// Run keeps the bot connected until ctx ends or Discord refuses it for good.
func (b *Bot) Run(ctx context.Context) error {
	b.init()
	var s *session
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		var err error
		s, err = b.connect(ctx, s)
		var fatal errFatal
		if errors.As(err, &fatal) {
			b.Log.Error("discord bot stopped", "err", err)
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		b.Log.Info("discord bot disconnected", "err", err, "resume", s != nil)
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff + rand.N(time.Second)):
		}
		backoff = min(backoff*2, time.Minute)
	}
	return nil
}

// connect runs one Gateway connection. It returns the session to resume
// (nil: identify afresh next time).
func (b *Bot) connect(ctx context.Context, s *session) (*session, error) {
	url := b.GatewayURL
	if s != nil && s.resumeURL != "" {
		url = s.resumeURL
	}
	ws, _, err := websocket.Dial(ctx, url+"/?v=10&encoding=json", nil)
	if err != nil {
		return s, err
	}
	defer ws.CloseNow()
	ws.SetReadLimit(64 << 20) // GUILD_CREATE of a big guild

	var hello struct {
		Interval int64 `json:"heartbeat_interval"`
	}
	p, err := read(ctx, ws)
	if err != nil {
		return s, err
	}
	if p.Op != opHello || json.Unmarshal(p.D, &hello) != nil || hello.Interval <= 0 {
		return s, errors.New("gateway: no hello")
	}
	if s != nil {
		err = write(ctx, ws, opResume, map[string]any{"token": b.Token, "session_id": s.id, "seq": s.seq})
	} else {
		err = write(ctx, ws, opIdentify, map[string]any{
			"token":      b.Token,
			"intents":    Intents,
			"properties": map[string]string{"os": "linux", "browser": "blite-signal", "device": "blite-signal"},
		})
	}
	if err != nil {
		return s, err
	}

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex // seq and acked
	var seq *int64
	if s != nil {
		seq = &s.seq
	}
	acked := true
	beatErr := make(chan error, 1)
	go func() {
		every := time.Duration(hello.Interval) * time.Millisecond
		next := time.Duration(rand.Float64() * float64(every))
		for {
			select {
			case <-cctx.Done():
				return
			case <-time.After(next):
			}
			next = every
			mu.Lock()
			if !acked {
				mu.Unlock()
				beatErr <- errors.New("gateway: heartbeat not acknowledged")
				ws.Close(4000, "zombie")
				return
			}
			acked = false
			var d any
			if seq != nil {
				d = *seq
			}
			mu.Unlock()
			if err := write(cctx, ws, opHeartbeat, d); err != nil {
				return
			}
		}
	}()

	cur := s
	for {
		p, err := read(cctx, ws)
		if err != nil {
			select {
			case e := <-beatErr:
				err = e
			default:
			}
			switch code := websocket.CloseStatus(err); code {
			case 4004, 4010, 4011, 4012, 4013, 4014: // auth, shard, version, intents
				return nil, errFatal{code}
			case 4007, 4009: // bad seq, session timed out
				return nil, err
			}
			return cur, err
		}
		if p.S != nil {
			mu.Lock()
			v := *p.S
			seq = &v
			mu.Unlock()
			if cur != nil {
				cur.seq = v
			}
		}
		switch p.Op {
		case opHeartbeat:
			mu.Lock()
			var d any
			if seq != nil {
				d = *seq
			}
			mu.Unlock()
			write(cctx, ws, opHeartbeat, d)
		case opHeartbeatAck:
			mu.Lock()
			acked = true
			mu.Unlock()
		case opReconnect:
			return cur, errors.New("gateway asked to reconnect")
		case opInvalidSession:
			var resumable bool
			json.Unmarshal(p.D, &resumable)
			if resumable {
				return cur, errors.New("gateway: invalid session (resumable)")
			}
			return nil, errors.New("gateway: invalid session")
		case opDispatch:
			if p.T == "READY" {
				var r struct {
					SessionID string `json:"session_id"`
					ResumeURL string `json:"resume_gateway_url"`
				}
				json.Unmarshal(p.D, &r)
				cur = &session{id: r.SessionID, resumeURL: r.ResumeURL}
				if p.S != nil {
					cur.seq = *p.S
				}
				// A fresh session: GUILD_CREATE follows for each guild with
				// everyone in voice, so what we knew may be stale.
				b.mu.Lock()
				b.guilds = map[string]*guild{}
				var was []string
				for u := range b.voice {
					was = append(was, u)
					b.gen[u]++
				}
				clear(b.voice)
				b.mu.Unlock()
				for _, u := range was {
					if b.OnVoice != nil {
						b.OnVoice(u, nil)
					}
				}
				b.Log.Info("discord bot ready")
			}
			b.dispatch(p.T, p.D)
		}
	}
}

// voiceState is the part of a Gateway voice state we look at.
type voiceState struct {
	GuildID   string  `json:"guild_id"`
	ChannelID *string `json:"channel_id"`
	UserID    string  `json:"user_id"`
}

type channel struct {
	ID      string `json:"id"`
	GuildID string `json:"guild_id"`
	Name    string `json:"name"`
}

// dispatch handles one Gateway event.
func (b *Bot) dispatch(t string, d json.RawMessage) {
	switch t {
	case "GUILD_CREATE", "GUILD_UPDATE":
		var g struct {
			ID          string       `json:"id"`
			Name        string       `json:"name"`
			Unavailable bool         `json:"unavailable"`
			Channels    []channel    `json:"channels"`
			VoiceStates []voiceState `json:"voice_states"`
		}
		if json.Unmarshal(d, &g) != nil || g.Unavailable {
			return
		}
		b.mu.Lock()
		gg := b.guilds[g.ID]
		if gg == nil {
			gg = &guild{channels: map[string]string{}}
			b.guilds[g.ID] = gg
		}
		gg.name = g.Name
		for _, c := range g.Channels {
			gg.channels[c.ID] = c.Name
		}
		b.mu.Unlock()
		for _, vs := range g.VoiceStates {
			vs.GuildID = g.ID
			b.voiceUpdate(vs)
		}
	case "GUILD_DELETE":
		var g struct {
			ID          string `json:"id"`
			Unavailable bool   `json:"unavailable"`
		}
		if json.Unmarshal(d, &g) != nil || g.Unavailable {
			return
		}
		var left []string
		b.mu.Lock()
		delete(b.guilds, g.ID)
		for u, v := range b.voice {
			if v.GuildID == g.ID {
				delete(b.voice, u)
				b.gen[u]++
				left = append(left, u)
			}
		}
		b.mu.Unlock()
		for _, u := range left {
			if b.OnVoice != nil {
				b.OnVoice(u, nil)
			}
		}
	case "CHANNEL_CREATE", "CHANNEL_UPDATE":
		var c channel
		if json.Unmarshal(d, &c) != nil || c.GuildID == "" {
			return
		}
		b.mu.Lock()
		if g := b.guilds[c.GuildID]; g != nil {
			g.channels[c.ID] = c.Name
		}
		b.mu.Unlock()
	case "CHANNEL_DELETE":
		var c channel
		if json.Unmarshal(d, &c) != nil {
			return
		}
		b.mu.Lock()
		if g := b.guilds[c.GuildID]; g != nil {
			delete(g.channels, c.ID)
		}
		b.mu.Unlock()
	case "VOICE_STATE_UPDATE":
		var vs voiceState
		if json.Unmarshal(d, &vs) == nil {
			b.voiceUpdate(vs)
		}
	}
}

// voiceUpdate applies one voice state, for active users only: anyone else's
// is dropped here, unread beyond the user id.
func (b *Bot) voiceUpdate(vs voiceState) {
	if vs.UserID == "" || b.Active == nil || !b.Active(vs.UserID) {
		return
	}
	var nv *Voice
	b.mu.Lock()
	b.gen[vs.UserID]++
	old, had := b.voice[vs.UserID]
	switch {
	case vs.ChannelID != nil && *vs.ChannelID != "":
		v := Voice{GuildID: vs.GuildID, ChannelID: *vs.ChannelID}
		if g := b.guilds[vs.GuildID]; g != nil {
			v.Guild, v.Channel = g.name, g.channels[v.ChannelID]
		}
		if had && old == v {
			b.mu.Unlock()
			return
		}
		b.voice[vs.UserID] = v
		nv = &v
	case had && old.GuildID == vs.GuildID:
		delete(b.voice, vs.UserID) // left voice
	default:
		b.mu.Unlock()
		return // left a channel elsewhere that we didn't know about
	}
	b.mu.Unlock()
	if b.OnVoice != nil {
		b.OnVoice(vs.UserID, nv)
	}
}

func read(ctx context.Context, ws *websocket.Conn) (payload, error) {
	var p payload
	_, data, err := ws.Read(ctx)
	if err != nil {
		return p, err
	}
	return p, json.Unmarshal(data, &p)
}

func write(ctx context.Context, ws *websocket.Conn, op int, d any) error {
	b, err := json.Marshal(map[string]any{"op": op, "d": d})
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return ws.Write(wctx, websocket.MessageText, b)
}
