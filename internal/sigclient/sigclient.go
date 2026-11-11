// Package sigclient is the client side of the Blite signaling protocol.
//
// If the WebSocket drops (Wi-Fi blip, proxy restart) the client resumes its
// slot with the server's resume token for up to ResumeWindow, so peer
// connections and media keep running; only a failed resume ends the session.
package sigclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"blite/internal/proto"
)

// ResumeWindow matches the server's slot grace period.
const ResumeWindow = 55 * time.Second

// Event is one message from the server after the welcome.
type Event struct {
	Joined  *proto.Peer   // peer-joined
	Left    string        // peer-left: peerId
	Signal  *proto.Signal // signal from a peer
	Resumed []proto.Peer  // reconnected; the room's current members
	// Topology is the room's new relay queue (head = relay; empty = mesh).
	// Also sent right after Resumed.
	Topology *proto.Topology
	// Plan is the room's new relay plan (servers that send plans send no
	// Topology). Also sent right after Resumed.
	Plan *proto.Plan
	// RelayRefused: the server refused our relay volunteering
	// (proto.ErrRelayUnsupported).
	RelayRefused bool
	// Renamed: a member's new display name.
	Renamed *proto.PeerRenamed
	// Owner is the room's new owner; also sent after Resumed.
	Owner *string
	// Kicked: the owner removed us; the server closes us next.
	Kicked *proto.Kicked
	Err    error // session over: left, kicked, or resume failed
}

// Client is a connected, seated signaling session.
type Client struct {
	Welcome proto.Welcome
	Events  chan Event

	url    string
	opt    Options
	wmu    sync.Mutex // serialises writes and guards ws swaps
	ws     *websocket.Conn
	early  [][]byte // messages that came before the welcome, handled first by run
	left   chan struct{}
	kicked chan struct{} // closed on TKicked: don't resume
	once   sync.Once
}

// Options for Dial. HTTPClient may be nil.
type Options struct {
	HTTPClient *http.Client
	// OnState is told "reconnecting" / "connected" while resuming.
	OnState func(string)
}

// Create dials url and creates a room.
func Create(ctx context.Context, url string, m proto.Create, o Options) (*Client, error) {
	m.T = proto.TCreate
	return enter(ctx, url, m, o)
}

// Join dials url and joins a room.
func Join(ctx context.Context, url string, m proto.Join, o Options) (*Client, error) {
	m.T = proto.TJoin
	return enter(ctx, url, m, o)
}

// ServerError is an error code returned by the server.
type ServerError string

func (e ServerError) Error() string { return "server: " + string(e) }

// maxEarly caps the messages we keep that arrive before the welcome: the
// server seats us before sending it, so members may already signal us (an
// end-to-end exchange key).
const maxEarly = 64

// dialSeat opens a socket, sends first, and reads the welcome. Messages
// that arrive before it are returned in early, in order.
func dialSeat(ctx context.Context, url string, first any, o Options) (*websocket.Conn, proto.Welcome, [][]byte, error) {
	var w proto.Welcome
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(dctx, url, &websocket.DialOptions{HTTPClient: o.HTTPClient})
	if err != nil {
		return nil, w, nil, fmt.Errorf("dial %s: %w", url, err)
	}
	ws.SetReadLimit(64 << 10)
	b, _ := json.Marshal(first)
	if err := ws.Write(dctx, websocket.MessageText, b); err != nil {
		ws.CloseNow()
		return nil, w, nil, err
	}
	var early [][]byte
	for {
		_, b, err = ws.Read(dctx)
		if err != nil {
			ws.CloseNow()
			return nil, w, nil, err
		}
		var env proto.Envelope
		json.Unmarshal(b, &env)
		switch env.T {
		case proto.TWelcome:
			if err := json.Unmarshal(b, &w); err != nil {
				ws.CloseNow()
				return nil, w, nil, err
			}
			return ws, w, early, nil
		case proto.TError:
			var e proto.Error
			json.Unmarshal(b, &e)
			ws.CloseNow()
			return nil, w, nil, ServerError(e.Code)
		}
		if len(early) == maxEarly {
			ws.CloseNow()
			return nil, w, nil, fmt.Errorf("no welcome after %d messages (last %q)", maxEarly, env.T)
		}
		early = append(early, b)
	}
}

func enter(ctx context.Context, url string, first any, o Options) (*Client, error) {
	ws, w, early, err := dialSeat(ctx, url, first, o)
	if err != nil {
		return nil, err
	}
	c := &Client{Welcome: w, Events: make(chan Event, 64), url: url, opt: o, ws: ws, early: early, left: make(chan struct{}), kicked: make(chan struct{})}
	go c.run()
	return c, nil
}

func (c *Client) conn() *websocket.Conn {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.ws
}

func (c *Client) state(s string) {
	if c.opt.OnState != nil {
		c.opt.OnState(s)
	}
}

func (c *Client) hasLeft() bool {
	select {
	case <-c.left:
		return true
	default:
		return false
	}
}

// run reads until the socket drops, then resumes; it closes Events at the end.
func (c *Client) run() {
	defer close(c.Events)
	for {
		for _, b := range c.early {
			c.handle(b)
		}
		c.early = nil
		err := c.readLoop(c.conn())
		if c.hasLeft() {
			c.Events <- Event{Err: errors.New("left")}
			return
		}
		select {
		case <-c.kicked:
			c.Events <- Event{Err: errors.New("kicked")}
			return
		default:
		}
		c.state("reconnecting")
		w, rerr := c.resume()
		if rerr != nil {
			c.Events <- Event{Err: fmt.Errorf("signaling lost (%v); resume failed: %w", err, rerr)}
			return
		}
		c.state("connected")
		c.Events <- Event{Resumed: w.Peers}
		if w.Plan != nil {
			c.Events <- Event{Plan: w.Plan}
		} else {
			c.Events <- Event{Topology: &proto.Topology{T: proto.TTopology, Queue: w.RelayQueue, Paused: w.RelayPaused, Waiting: w.RelayWaiting}}
		}
		owner := w.Owner
		c.Events <- Event{Owner: &owner}
	}
}

// resume retries with backoff until ResumeWindow runs out or the server says no.
func (c *Client) resume() (proto.Welcome, error) {
	deadline := time.Now().Add(ResumeWindow)
	wait := 500 * time.Millisecond
	msg := proto.Resume{T: proto.TResume, Room: c.Welcome.Room, PeerID: c.Welcome.PeerID, Token: c.Welcome.Resume}
	for {
		if c.hasLeft() {
			return proto.Welcome{}, errors.New("left")
		}
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		ws, w, early, err := dialSeat(ctx, c.url, msg, c.opt)
		cancel()
		if err == nil {
			c.wmu.Lock()
			c.ws = ws
			c.wmu.Unlock()
			c.early = early // handled by run after the resume events
			return w, nil
		}
		var se ServerError
		if errors.As(err, &se) {
			return w, err // resume_failed: slot gone, retrying won't help
		}
		if time.Now().Add(wait).After(deadline) {
			return w, err
		}
		time.Sleep(wait)
		if wait < 5*time.Second {
			wait *= 2
		}
	}
}

func (c *Client) readLoop(ws *websocket.Conn) error {
	for {
		_, b, err := ws.Read(context.Background())
		if err != nil {
			return err
		}
		c.handle(b)
	}
}

// handle turns one server message into events.
func (c *Client) handle(b []byte) {
	var env proto.Envelope
	if json.Unmarshal(b, &env) != nil {
		return
	}
	switch env.T {
	case proto.TPeerJoined:
		var m proto.PeerJoined
		if json.Unmarshal(b, &m) == nil {
			p := m.Peer
			c.Events <- Event{Joined: &p}
		}
	case proto.TPeerLeft:
		var m proto.PeerLeft
		if json.Unmarshal(b, &m) == nil {
			c.Events <- Event{Left: m.PeerID}
		}
	case proto.TSignal:
		var m proto.Signal
		if json.Unmarshal(b, &m) == nil {
			c.Events <- Event{Signal: &m}
		}
	case proto.TTopology:
		var m proto.Topology
		if json.Unmarshal(b, &m) == nil {
			c.Events <- Event{Topology: &m}
		}
	case proto.TPlan:
		var m proto.Plan
		if json.Unmarshal(b, &m) == nil {
			c.Events <- Event{Plan: &m}
		}
	case proto.TPeerRenamed:
		var m proto.PeerRenamed
		if json.Unmarshal(b, &m) == nil {
			c.Events <- Event{Renamed: &m}
		}
	case proto.TOwner:
		var m proto.Owner
		if json.Unmarshal(b, &m) == nil {
			c.Events <- Event{Owner: &m.PeerID}
		}
	case proto.TKicked:
		var m proto.Kicked
		if json.Unmarshal(b, &m) == nil {
			select {
			case <-c.kicked:
			default:
				close(c.kicked)
			}
			c.Events <- Event{Kicked: &m}
		}
	case proto.TError:
		// no_peer etc. are informational.
		var e proto.Error
		if json.Unmarshal(b, &e) == nil && e.Code == proto.ErrRelayUnsupported {
			c.Events <- Event{RelayRefused: true}
		}
	}
}

func (c *Client) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return c.ws.Write(ctx, websocket.MessageText, b)
}

// Signal sends opaque data to a peer. While reconnecting it fails; callers
// (ICE trickle) tolerate that.
func (c *Client) Signal(to string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return c.send(proto.Signal{T: proto.TSignal, To: to, Data: raw})
}

// Volunteer joins (on) or leaves the room's relay queue.
func (c *Client) Volunteer(on bool) error {
	return c.send(proto.RelayVolunteer{T: proto.TRelayVolunteer, On: on})
}

// Caps reports what we could do as a relay.
func (c *Client) Caps(m proto.Caps) error {
	m.T = proto.TCaps
	return c.send(m)
}

// Rename sets our display name in the room.
func (c *Client) Rename(name string) error {
	return c.send(proto.Rename{T: proto.TRename, Name: name})
}

// Transfer (owner) makes peerID the room's owner.
func (c *Client) Transfer(peerID string) error {
	return c.send(proto.Transfer{T: proto.TTransfer, PeerID: peerID})
}

// Kick (owner) removes peerID from the room; ban also bars it from coming back.
func (c *Client) Kick(peerID string, ban bool) error {
	return c.send(proto.Kick{T: proto.TKick, PeerID: peerID, Ban: ban})
}

// Leave tells the server we're gone and closes the socket.
func (c *Client) Leave() { c.LeaveFor("") }

// LeaveFor is Leave with a reason (proto.LeaveUpdate).
func (c *Client) LeaveFor(reason string) {
	c.once.Do(func() {
		close(c.left)
		c.send(proto.Leave{T: proto.TLeave, Reason: reason})
		c.conn().Close(websocket.StatusNormalClosure, "")
	})
}

// Drop closes the socket without leaving (tests: simulates a network blip).
func (c *Client) Drop() { c.conn().CloseNow() }

// IsServerError reports whether err is the given server error code.
func IsServerError(err error, code string) bool {
	var se ServerError
	return errors.As(err, &se) && string(se) == code
}
