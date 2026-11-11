package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"blite/internal/discord"
)

// Discord is the voice channel the user is in on Discord and the session
// tied to it, for one-click watch or go live.
type Discord struct {
	// Status: "off" (Discord isn't running), "login" (the user hasn't let
	// us read their voice channel yet), "waiting" (approving in Discord),
	// "ready".
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	ChannelID string `json:"channelId,omitempty"` // "" = not in a voice channel
	Channel   string `json:"channel,omitempty"`
	Guild     string `json:"guild,omitempty"`
	// The session tied to the channel, if any.
	Room   string `json:"room,omitempty"`
	Open   bool   `json:"open,omitempty"`
	People int    `json:"people,omitempty"`
}

type discordToken struct {
	Access  string    `json:"access_token"`
	Refresh string    `json:"refresh_token"`
	Expires time.Time `json:"expires"`
}

func discordTokenFile() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "blite", "discord.json"), nil
}

func loadDiscordToken() *discordToken {
	p, err := discordTokenFile()
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var t discordToken
	if json.Unmarshal(b, &t) != nil || t.Refresh == "" {
		return nil
	}
	return &t
}

func saveDiscordToken(t *discordToken) {
	p, err := discordTokenFile()
	if err != nil {
		return
	}
	if t == nil {
		os.Remove(p)
		return
	}
	os.MkdirAll(filepath.Dir(p), 0o700)
	b, _ := json.Marshal(t)
	os.WriteFile(p, b, 0o600)
}

// httpBase turns the signaling URL (wss://host/ws) into https://host.
func httpBase(server string) string {
	u, err := url.Parse(server)
	if err != nil {
		return ""
	}
	scheme := "https"
	if u.Scheme == "ws" {
		scheme = "http"
	}
	return scheme + "://" + u.Host
}

// exchange gets a Discord token from our server for a code or a refresh token.
func (a *App) exchange(ctx context.Context, code, refresh string) (*discordToken, error) {
	body, _ := json.Marshal(map[string]string{"code": code, "refresh": refresh})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, httpBase(a.config().Server)+"/v1/discord/token", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange: %s", res.Status)
	}
	var r struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Expires int    `json:"expires_in"`
	}
	if err := json.NewDecoder(res.Body).Decode(&r); err != nil || r.Access == "" {
		return nil, errors.New("token exchange: no token")
	}
	return &discordToken{Access: r.Access, Refresh: r.Refresh, Expires: time.Now().Add(time.Duration(r.Expires) * time.Second)}, nil
}

// lookupChannel asks the server which session is tied to a voice channel.
func (a *App) lookupChannel(ctx context.Context, id string) (room string, open bool, people int) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, httpBase(a.config().Server)+"/v1/channel/"+url.PathEscape(id), nil)
	res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", false, 0
	}
	defer res.Body.Close()
	var r struct {
		Room   string `json:"room"`
		Open   bool   `json:"open"`
		People int    `json:"people"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&r) != nil {
		return "", false, 0
	}
	return r.Room, r.Open, r.People
}

func (a *App) setDiscord(f func(d *Discord)) {
	a.mu.Lock()
	f(&a.discord)
	a.mu.Unlock()
	a.bc.Notify()
}

// DiscordLogin asks Discord (once) to let us read the user's voice channel.
func (a *App) DiscordLogin() {
	select {
	case a.discordAuth <- struct{}{}:
	default:
	}
}

// RunDiscord follows the user's Discord voice channel until ctx ends.
func (a *App) RunDiscord(ctx context.Context) {
	lastErr := ""
	for ctx.Err() == nil {
		c, err := discord.Dial()
		if err != nil {
			if e := err.Error(); e != lastErr { // once per kind, not every 15 s
				a.log.Info("discord not reachable", "err", e)
				lastErr = e
			}
			a.setDiscord(func(d *Discord) { *d = Discord{Status: "off", Error: err.Error()} })
			select {
			case <-ctx.Done():
				return
			case <-time.After(15 * time.Second):
			}
			continue
		}
		lastErr = ""
		a.log.Info("discord connected")
		err = a.followDiscord(ctx, c)
		c.Close()
		if err != nil {
			a.log.Info("discord", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

// followDiscord logs in on one connection, then tracks the channel.
func (a *App) followDiscord(ctx context.Context, c *discord.Conn) error {
	tok := loadDiscordToken()
	for {
		if tok != nil && time.Until(tok.Expires) < time.Hour {
			if t, err := a.exchange(ctx, "", tok.Refresh); err == nil {
				tok = t
				saveDiscordToken(tok)
			}
		}
		if tok != nil {
			if err := c.Authenticate(tok.Access); err == nil {
				break
			} else if strings.Contains(err.Error(), "connection closed") {
				return err
			}
			tok = nil // revoked or expired: ask again
			saveDiscordToken(nil)
		}
		a.log.Info("discord: waiting for Connect Discord")
		a.setDiscord(func(d *Discord) { *d = Discord{Status: "login"} })
		select {
		case <-ctx.Done():
			return nil
		case <-c.Done():
			return errors.New("discord quit")
		case <-a.discordAuth:
		}
		a.setDiscord(func(d *Discord) { d.Status, d.Error = "waiting", "" })
		code, err := c.Authorize()
		if err == nil {
			tok, err = a.exchange(ctx, code, "")
		}
		if err != nil {
			a.log.Info("discord authorize", "err", err)
			msg := err.Error()
			if strings.Contains(msg, "invalid_scope") {
				// Until Discord approves the app, only its owner and
				// testers may grant the voice channel scopes.
				msg = "Discord didn't allow it: your Discord account isn't a tester of the Bellion app yet. Ask the app's owner to add you, then accept the invite."
			}
			a.setDiscord(func(d *Discord) { d.Status, d.Error = "login", msg })
			continue
		}
		a.log.Info("discord authorized")
		saveDiscordToken(tok)
	}
	if err := c.WatchVoice(); err != nil {
		return err
	}
	guilds := map[string]string{}
	refresh := func() {
		ch, err := c.VoiceChannel()
		if err != nil {
			return
		}
		nd := Discord{Status: "ready"}
		if ch != nil {
			nd.ChannelID, nd.Channel = ch.ID, ch.Name
			if _, ok := guilds[ch.GuildID]; !ok && ch.GuildID != "" {
				guilds[ch.GuildID] = c.GuildName(ch.GuildID)
			}
			nd.Guild = guilds[ch.GuildID]
			nd.Room, nd.Open, nd.People = a.lookupChannel(ctx, ch.ID)
		}
		a.setDiscord(func(d *Discord) { *d = nd })
	}
	refresh()
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-c.Done():
			return errors.New("discord quit")
		case <-c.Events():
			refresh()
		case <-tick.C:
			refresh() // a session may have started or ended in the channel
		}
	}
}

// GoLiveInChannel starts a session tied to the user's Discord voice channel.
func (a *App) GoLiveInChannel(ctx context.Context, password string, e2ee bool) error {
	a.mu.Lock()
	id := a.discord.ChannelID
	a.mu.Unlock()
	if id == "" {
		return errors.New("you're not in a Discord voice channel")
	}
	return a.create(ctx, password, e2ee, id)
}

// JoinChannel joins the session tied to the user's Discord voice channel.
func (a *App) JoinChannel(ctx context.Context, password string) error {
	a.mu.Lock()
	code := a.discord.Room
	a.mu.Unlock()
	if code == "" {
		return errors.New("nobody shares in this Discord channel yet")
	}
	return a.join(ctx, code, password)
}
