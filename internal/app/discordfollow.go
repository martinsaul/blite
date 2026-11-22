package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/coder/websocket"

	"blite/internal/proto"
)

// errNoFollow: the server has no Discord bot (old or self-hosted servers).
var errNoFollow = errors.New("the server doesn't follow Discord voice channels")

// followURL is the server's Discord follow socket for its signaling URL.
func followURL(server string) string {
	u, err := url.Parse(server)
	if err != nil {
		return ""
	}
	u.Path, u.RawQuery = "/v1/discord/follow", ""
	return u.String()
}

// DiscordUnlink disconnects this install from its Discord account (the
// server's bot stops following it).
func (a *App) DiscordUnlink() {
	select {
	case a.discordUnlink <- struct{}{}:
	default:
	}
}

// runFollow follows the user's voice channel through our server's Discord
// bot, reconnecting until ctx ends. It returns errNoFollow at once when
// the server has no bot.
func (a *App) runFollow(ctx context.Context) error {
	backoff := 2 * time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := a.follow(ctx)
		if errors.Is(err, errNoFollow) {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		a.log.Info("discord follow", "err", err)
		if time.Since(start) > time.Minute {
			backoff = 2 * time.Second
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
	return nil
}

// follow runs one follow socket.
func (a *App) follow(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	dctx, dcancel := context.WithTimeout(ctx, 20*time.Second)
	ws, res, err := websocket.Dial(dctx, followURL(a.discordServer()), nil)
	dcancel()
	if err != nil {
		if res != nil && res.StatusCode == http.StatusNotFound {
			return errNoFollow
		}
		return err
	}
	defer ws.CloseNow()
	ws.SetReadLimit(64 << 10)

	read := func() (proto.DiscordMsg, error) {
		var m proto.DiscordMsg
		_, b, err := ws.Read(ctx)
		if err == nil {
			err = json.Unmarshal(b, &m)
		}
		return m, err
	}
	write := func(m proto.DiscordMsg) error {
		b, _ := json.Marshal(m)
		wctx, c := context.WithTimeout(ctx, 10*time.Second)
		defer c()
		return ws.Write(wctx, websocket.MessageText, b)
	}

	hello, err := read()
	if err != nil {
		return err
	}
	ch, err := base64.StdEncoding.DecodeString(hello.Challenge)
	if hello.T != proto.TDiscordHello || err != nil || len(ch) == 0 {
		return errors.New("discord follow: no hello")
	}
	key := a.cfg.Identity
	sig := key.Sign(proto.DiscordFollowPurpose, ch)
	if err := write(proto.DiscordMsg{T: proto.TDiscordAuth, Key: key.Public(), Sig: base64.StdEncoding.EncodeToString(sig)}); err != nil {
		return err
	}

	states := make(chan proto.DiscordMsg)
	readErr := make(chan error, 1)
	go func() {
		for {
			m, err := read()
			if err != nil {
				readErr <- err
				return
			}
			select {
			case states <- m:
			case <-ctx.Done():
				return
			}
		}
	}()

	var cur proto.DiscordMsg
	apply := func() {
		nd := Discord{Status: "login", Via: "bot", BotURL: httpBase(a.discordServer()) + "/bot"}
		if cur.Linked {
			nd.Status, nd.User = "ready", cur.User
			if v := cur.Voice; v != nil {
				nd.ChannelID, nd.Channel, nd.Guild = v.ChannelID, v.Channel, v.Guild
				cr := a.lookupChannel(ctx, v.ChannelID)
				nd.Room, nd.Open, nd.People, nd.Sharers, nd.Members = cr.Room, cr.Open, cr.People, cr.Sharers, cr.Members
			}
		}
		a.mu.Lock()
		linking := a.discord.Status == "linking"
		a.mu.Unlock()
		if linking && !cur.Linked {
			nd.Status = "linking" // until the browser comes back
		}
		a.setDiscord(func(d *Discord) { *d = nd })
	}
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-readErr:
			return err
		case m := <-states:
			if m.T != proto.TDiscordState {
				continue
			}
			if m.LinkURL != "" {
				if err := OpenBrowser(m.LinkURL); err != nil {
					a.log.Warn("open browser", "err", err)
					a.setDiscord(func(d *Discord) { d.Status, d.Error = "login", err.Error() })
				}
				continue
			}
			if m.Linked && !cur.Linked {
				a.log.Info("discord linked")
			}
			cur = m
			apply()
		case <-a.discordAuth:
			a.setDiscord(func(d *Discord) { d.Status, d.Error = "linking", "" })
			if err := write(proto.DiscordMsg{T: proto.TDiscordLink}); err != nil {
				return err
			}
		case <-a.discordUnlink:
			a.log.Info("discord unlink")
			if err := write(proto.DiscordMsg{T: proto.TDiscordUnlink}); err != nil {
				return err
			}
		case <-tick.C:
			if cur.Voice != nil {
				apply() // a session may have started or ended in the channel
			}
		}
	}
}
