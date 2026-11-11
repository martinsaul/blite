// Package discord talks to the Discord app on this computer over its local
// RPC (IPC) socket, to learn which voice channel the user is in. Reading it
// needs the "rpc" and "rpc.voice.read" scopes, which Discord grants to the
// application's owner and its App Testers: the user approves once in Discord,
// the server exchanges the code for a token (our client secret stays there),
// and we keep the refresh token.
package discord

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// ClientID is the Bellion Discord application (public).
const ClientID = "1569899965323415693"

// Scopes the voice-channel lookup needs.
var Scopes = []string{"rpc", "rpc.voice.read"}

// ErrNotRunning: no Discord app answers on this computer.
var ErrNotRunning = errors.New("discord is not running")

const (
	opHandshake = 0
	opFrame     = 1
	opClose     = 2
)

// Channel is the voice channel the user is in.
type Channel struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	GuildID string `json:"guild_id"`
}

// Conn is one RPC connection to the local Discord app.
type Conn struct {
	rw     io.ReadWriteCloser
	wmu    sync.Mutex
	mu     sync.Mutex
	wait   map[string]chan Event
	events chan Event
	done   chan struct{}
	err    error
}

// Event is one RPC message; subscribed events arrive on Conn.Events.
type Event struct {
	Cmd   string          `json:"cmd"`
	Evt   string          `json:"evt"`
	Nonce string          `json:"nonce"`
	Data  json.RawMessage `json:"data"`
}

// Dial connects and does the handshake. Events (VOICE_CHANNEL_SELECT) come
// on Events.
func Dial() (*Conn, error) {
	rw, err := dial()
	if err != nil {
		return nil, fmt.Errorf("%w (%v)", ErrNotRunning, err)
	}
	c := &Conn{rw: rw, wait: map[string]chan Event{}, events: make(chan Event, 16), done: make(chan struct{})}
	if err := c.write(opHandshake, map[string]any{"v": 1, "client_id": ClientID}); err != nil {
		rw.Close()
		return nil, err
	}
	ready := make(chan Event, 1)
	c.wait["READY"] = ready
	go c.read()
	select {
	case f := <-ready:
		if f.Evt == "ERROR" {
			c.Close()
			return nil, rpcError(f.Data)
		}
	case <-c.done:
		return nil, c.err
	case <-time.After(5 * time.Second):
		c.Close()
		return nil, errors.New("discord: no handshake answer")
	}
	return c, nil
}

// Close ends the connection.
func (c *Conn) Close() error { return c.rw.Close() }

// Done is closed when the connection ends (Discord quit).
func (c *Conn) Done() <-chan struct{} { return c.done }

// Events receives subscribed events.
func (c *Conn) Events() <-chan Event { return c.events }

func (c *Conn) write(op uint32, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	h := make([]byte, 8)
	binary.LittleEndian.PutUint32(h, op)
	binary.LittleEndian.PutUint32(h[4:], uint32(len(b)))
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.rw.Write(append(h, b...))
	return err
}

func (c *Conn) read() {
	defer close(c.done)
	h := make([]byte, 8)
	for {
		if _, err := io.ReadFull(c.rw, h); err != nil {
			c.err = err
			return
		}
		op, n := binary.LittleEndian.Uint32(h), binary.LittleEndian.Uint32(h[4:])
		if n > 1<<20 {
			c.err = errors.New("discord: Event too big")
			return
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(c.rw, b); err != nil {
			c.err = err
			return
		}
		if op == opClose {
			c.err = fmt.Errorf("discord closed the connection: %s", b)
			return
		}
		var f Event
		if json.Unmarshal(b, &f) != nil {
			continue
		}
		key := f.Nonce
		if f.Cmd == "DISPATCH" && f.Evt == "READY" {
			key = "READY"
		}
		c.mu.Lock()
		ch := c.wait[key]
		delete(c.wait, key)
		c.mu.Unlock()
		switch {
		case ch != nil:
			ch <- f
		case f.Cmd == "DISPATCH":
			select {
			case c.events <- f:
			default:
			}
		}
	}
}

// call sends a command and waits for its answer.
func (c *Conn) call(cmd string, args any, evt string, timeout time.Duration) (json.RawMessage, error) {
	n := make([]byte, 8)
	rand.Read(n)
	nonce := hex.EncodeToString(n)
	ch := make(chan Event, 1)
	c.mu.Lock()
	c.wait[nonce] = ch
	c.mu.Unlock()
	msg := map[string]any{"cmd": cmd, "args": args, "nonce": nonce}
	if evt != "" {
		msg["evt"] = evt
	}
	if err := c.write(opFrame, msg); err != nil {
		return nil, err
	}
	select {
	case f := <-ch:
		if f.Evt == "ERROR" {
			return nil, rpcError(f.Data)
		}
		return f.Data, nil
	case <-c.done:
		return nil, errors.New("discord connection closed")
	case <-time.After(timeout):
		c.mu.Lock()
		delete(c.wait, nonce)
		c.mu.Unlock()
		return nil, fmt.Errorf("discord: %s timed out", cmd)
	}
}

func rpcError(d json.RawMessage) error {
	var e struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	json.Unmarshal(d, &e)
	return fmt.Errorf("discord: %s (%d)", e.Message, e.Code)
}

// Authorize asks the user, in Discord, to let us read their voice channel.
// It returns the code to exchange; it waits up to two minutes for them.
func (c *Conn) Authorize() (string, error) {
	d, err := c.call("AUTHORIZE", map[string]any{"client_id": ClientID, "scopes": Scopes}, "", 2*time.Minute)
	if err != nil {
		return "", err
	}
	var r struct{ Code string }
	json.Unmarshal(d, &r)
	if r.Code == "" {
		return "", errors.New("discord: no authorization code")
	}
	return r.Code, nil
}

// Authenticate logs this connection in with an access token.
func (c *Conn) Authenticate(token string) error {
	_, err := c.call("AUTHENTICATE", map[string]any{"access_token": token}, "", 10*time.Second)
	return err
}

// VoiceChannel is the voice channel the user is in, or nil.
func (c *Conn) VoiceChannel() (*Channel, error) {
	d, err := c.call("GET_SELECTED_VOICE_CHANNEL", map[string]any{}, "", 10*time.Second)
	if err != nil {
		return nil, err
	}
	if len(d) == 0 || string(d) == "null" {
		return nil, nil
	}
	var ch Channel
	if err := json.Unmarshal(d, &ch); err != nil || ch.ID == "" {
		return nil, nil
	}
	return &ch, nil
}

// GuildName is a server's name ("" if unknown).
func (c *Conn) GuildName(id string) string {
	d, err := c.call("GET_GUILD", map[string]any{"guild_id": id, "timeout": 5}, "", 8*time.Second)
	if err != nil {
		return ""
	}
	var g struct{ Name string }
	json.Unmarshal(d, &g)
	return g.Name
}

// WatchVoice subscribes to the user switching voice channels; each switch
// arrives on Events.
func (c *Conn) WatchVoice() error {
	_, err := c.call("SUBSCRIBE", map[string]any{}, "VOICE_CHANNEL_SELECT", 10*time.Second)
	return err
}
