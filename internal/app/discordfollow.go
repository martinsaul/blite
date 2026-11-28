package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/coder/websocket"

	"blite/internal/proto"
)

var (
	// errNoFollow: the server has no Discord bot (old or self-hosted servers).
	errNoFollow = errors.New("the server doesn't follow Discord voice channels")
	// errUnlinked: this install isn't linked; wait for Connect Discord.
	errUnlinked = errors.New("discord not linked")
)

// followURL is the server's Discord follow socket for its signaling URL.
func followURL(server string) string {
	u, err := url.Parse(server)
	if err != nil {
		return ""
	}
	u.Path, u.RawQuery = "/v1/discord/follow", ""
	return u.String()
}

// validDiscordLinkURL reports whether raw is a Discord OAuth2 authorize URL
// safe to open in the user's browser.
func validDiscordLinkURL(raw string) bool {
	if raw == "" || strings.TrimSpace(raw) != raw || strings.Contains(raw, `\`) {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil {
		return false
	}
	if strings.Contains(strings.ToLower(u.RawPath), "%5c") {
		return false
	}
	switch strings.ToLower(u.Host) {
	case "discord.com", "www.discord.com", "discord.com:443", "www.discord.com:443":
	default:
		return false
	}
	if !strings.HasPrefix(u.Path, "/oauth2/authorize") {
		return false
	}
	clean := path.Clean(u.Path)
	return clean == "/oauth2/authorize" || strings.HasPrefix(clean, "/oauth2/authorize/")
}

// linkURLHost returns only the host of raw for logs, or "" if raw has none.
func linkURLHost(raw string) string {
	if strings.Contains(raw, `\`) {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func (a *App) discordLoginState() Discord {
	return Discord{Status: "login", Via: "bot", BotURL: httpBase(a.discordServer()) + "/bot"}
}

func (a *App) setDiscordLogin(errText string) {
	nd := a.discordLoginState()
	nd.Error = errText
	a.setDiscord(func(d *Discord) { *d = nd })
}

func (a *App) discordLinking() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.discord.Status == "linking"
}

// DiscordUnlink disconnects this install from its Discord account (the
// server's bot stops following it).
func (a *App) DiscordUnlink() {
	select {
	case a.discordUnlink <- struct{}{}:
	default:
	}
}

// probeFollow checks whether the server offers Discord following without
// opening a WebSocket or sending our identity key.
func (a *App) probeFollow(ctx context.Context) error {
	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, httpBase(a.discordServer())+"/v1/discord/follow", nil)
	if err != nil {
		return err
	}
	c := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	res, err := c.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return errNoFollow
	}
	if res.StatusCode >= 500 {
		return fmt.Errorf("discord follow: %s", res.Status)
	}
	return nil
}

func (a *App) waitProbe(ctx context.Context) error {
	backoff := 2 * time.Second
	for ctx.Err() == nil {
		err := a.probeFollow(ctx)
		if err == nil || errors.Is(err, errNoFollow) {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		a.log.Info("discord follow probe", "err", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
	return nil
}

// runFollow follows the user's voice channel through our server's Discord
// bot, reconnecting until ctx ends. When this install isn't linked yet it
// probes once without sending our key and waits for Connect Discord before
// opening the socket. It returns errNoFollow at once when the server has no
// bot.
func (a *App) runFollow(ctx context.Context) error {
	wantConnect := false
	if !discordLinked() {
		if err := a.waitProbe(ctx); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		a.setDiscordLogin("")
	}
	backoff := 2 * time.Second
	for ctx.Err() == nil {
		if !discordLinked() && !a.discordLinking() && !wantConnect {
			select {
			case <-ctx.Done():
				return nil
			case <-a.discordAuth:
				wantConnect = true
				backoff = 2 * time.Second
			case <-a.discordUnlink:
				continue
			}
		}
		start := time.Now()
		started, err := a.follow(ctx, wantConnect)
		if started {
			wantConnect = false
		}
		if errors.Is(err, errNoFollow) {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, errUnlinked) {
			backoff = 2 * time.Second
			continue
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

// follow runs one follow socket. loginClick is true when Connect Discord
// opened this socket; started reports whether the first server state was
// handled.
func (a *App) follow(ctx context.Context, loginClick bool) (bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	dctx, dcancel := context.WithTimeout(ctx, 20*time.Second)
	ws, res, err := websocket.Dial(dctx, followURL(a.discordServer()), nil)
	dcancel()
	if err != nil {
		if res != nil && res.StatusCode == http.StatusNotFound {
			return false, errNoFollow
		}
		return false, err
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
		return false, err
	}
	ch, err := base64.StdEncoding.DecodeString(hello.Challenge)
	if hello.T != proto.TDiscordHello || err != nil || len(ch) == 0 {
		return false, errors.New("discord follow: no hello")
	}
	key := a.cfg.Identity
	if key.Private == nil {
		return false, errNoFollow
	}
	sig := key.Sign(proto.DiscordFollowPurpose, ch)
	if err := write(proto.DiscordMsg{T: proto.TDiscordAuth, Key: key.Public(), Sig: base64.StdEncoding.EncodeToString(sig)}); err != nil {
		return false, err
	}

	closing := make(chan struct{})
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
			case <-closing:
				continue
			default:
			}
			select {
			case states <- m:
			case <-closing:
			case <-ctx.Done():
				return
			}
		}
	}()
	closeClean := func() {
		close(closing)
		done := make(chan struct{})
		go func() {
			ws.Close(websocket.StatusNormalClosure, "")
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
			ws.CloseNow()
		case <-ctx.Done():
			ws.CloseNow()
		}
	}

	var (
		cur      proto.DiscordMsg
		started  bool
		wantLink bool
	)
	requestLink := func() error {
		wantLink = true
		base := a.discordLoginState()
		a.setDiscord(func(d *Discord) {
			if d.Status == "" {
				*d = base
			}
			d.Status, d.Error = "linking", ""
		})
		return write(proto.DiscordMsg{T: proto.TDiscordLink})
	}
	apply := func() {
		nd := a.discordLoginState()
		if cur.Linked {
			nd.Status, nd.User = "ready", cur.User
			if v := cur.Voice; v != nil {
				nd.ChannelID, nd.Channel, nd.Guild = v.ChannelID, v.Channel, v.Guild
				cr := a.lookupChannel(ctx, v.ChannelID)
				nd.Room, nd.Open, nd.People, nd.Sharers, nd.Members = cr.Room, cr.Open, cr.People, cr.Sharers, cr.Members
			}
		}
		if a.discordLinking() && !cur.Linked {
			nd.Status = "linking" // until the browser comes back
		}
		a.setDiscord(func(d *Discord) { *d = nd })
	}
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return started, nil
		case err := <-readErr:
			return started, err
		case m := <-states:
			if m.T != proto.TDiscordState {
				continue
			}
			if m.LinkURL != "" {
				if !wantLink || !a.discordLinking() {
					a.log.Warn("discord link URL without request", "host", linkURLHost(m.LinkURL))
					continue
				}
				wantLink = false
				if !validDiscordLinkURL(m.LinkURL) {
					a.log.Warn("discord link URL rejected", "host", linkURLHost(m.LinkURL))
					continue
				}
				if err := OpenBrowser(m.LinkURL); err != nil {
					a.log.Warn("open browser", "err", err)
					a.setDiscordLinked(false)
					a.setDiscordLogin(err.Error())
					closeClean()
					return started, errUnlinked
				}
				continue
			}
			first := !started
			started = true
			if m.Linked {
				wantLink = false
				if !cur.Linked {
					a.log.Info("discord linked")
				}
				a.setDiscordLinked(true)
				cur = m
				apply()
				continue
			}
			if first && loginClick {
				select {
				case <-a.discordAuth:
				default:
				}
				if err := requestLink(); err != nil {
					return started, err
				}
				continue
			}
			if a.discordLinking() {
				cur = m
				apply()
				continue
			}
			a.log.Info("discord not linked on server")
			a.setDiscordLinked(false)
			a.setDiscordLogin("")
			closeClean()
			return started, errUnlinked
		case <-a.discordAuth:
			if cur.Linked || (loginClick && !started) {
				continue
			}
			if err := requestLink(); err != nil {
				return started, err
			}
		case <-a.discordUnlink:
			a.log.Info("discord unlink")
			wantLink = false
			a.setDiscordLinked(false)
			_ = write(proto.DiscordMsg{T: proto.TDiscordUnlink})
			a.setDiscordLogin("")
			closeClean()
			return started, errUnlinked
		case <-tick.C:
			if cur.Voice != nil {
				apply() // a session may have started or ended in the channel
			}
		}
	}
}
