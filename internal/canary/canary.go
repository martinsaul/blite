// Package canary is blite-signal's synthetic end-to-end check:
// a deploy runs it against a new instance before giving it traffic.
package canary

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"blite/internal/proto"
	"blite/internal/room"
	"blite/internal/sigclient"
)

// Timeout is Run's overall deadline when ctx has none sooner.
const Timeout = 15 * time.Second

// Options for Run.
type Options struct {
	// Connect, if set, is the host:port to dial instead of the URL's own
	// host; the Host header and TLS server name still come from the URL
	// (reach a canary router through a given proxy address).
	Connect string
	// Build is the client build number the canary reports (at least the
	// server's BLITE_MIN_COMPATIBLE_BUILD).
	Build int
}

// Run checks the signaling server at url (wss://host/ws): it creates a
// room, joins it with a second identity, exchanges signals both ways, drops
// the joiner's socket, resumes its seat and exchanges signals again, then
// both leave. The room is a canary room (room.CanaryPrefix): never saved,
// gone once both have left. It returns nil if all of that worked.
func Run(ctx context.Context, url string, o Options) error {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	so := sigclient.Options{Renew: -1, HTTPClient: httpClient(o.Connect)}
	meta := &proto.ClientMeta{Platform: "canary", AppVersion: "canary", Build: o.Build}
	pw := token()

	a, err := sigclient.Create(ctx, url, proto.Create{Password: pw, Name: room.CanaryPrefix + "-a", Client: meta}, so)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	defer a.Leave()
	b, err := sigclient.Join(ctx, url, proto.Join{Room: a.Welcome.Room, Password: pw, Name: room.CanaryPrefix + "-b", Client: meta}, so)
	if err != nil {
		return fmt.Errorf("join: %w", err)
	}
	defer b.Leave()
	aID, bID := a.Welcome.PeerID, b.Welcome.PeerID
	if len(b.Welcome.Peers) != 1 || b.Welcome.Peers[0].PeerID != aID {
		return fmt.Errorf("join: welcome lists %v, want only the creator", b.Welcome.Peers)
	}
	if err := wait(ctx, a, "creator sees the joiner", func(e sigclient.Event) bool { return e.Joined != nil && e.Joined.PeerID == bID }); err != nil {
		return err
	}
	if err := exchange(ctx, a, b, "before resume"); err != nil {
		return err
	}

	b.Drop() // the client resumes on its own
	if err := wait(ctx, b, "joiner resumes", func(e sigclient.Event) bool {
		if e.Resumed == nil {
			return false
		}
		for _, p := range e.Resumed {
			if p.PeerID == aID {
				return true
			}
		}
		return false
	}); err != nil {
		return err
	}
	if b.Welcome.PeerID != bID {
		return errors.New("resume: seat changed")
	}
	return exchange(ctx, a, b, "after resume")
}

// exchange sends a signal a -> b and one b -> a, and waits for both.
func exchange(ctx context.Context, a, b *sigclient.Client, when string) error {
	aID, bID := a.Welcome.PeerID, b.Welcome.PeerID
	for _, d := range []struct {
		from, to *sigclient.Client
		toID     string
		fromID   string
	}{{a, b, bID, aID}, {b, a, aID, bID}} {
		nonce := token()
		if err := d.from.Signal(d.toID, map[string]string{"canary": nonce}); err != nil {
			return fmt.Errorf("signal %s: %w", when, err)
		}
		what := fmt.Sprintf("signal %s from %s", when, d.fromID)
		if err := wait(ctx, d.to, what, func(e sigclient.Event) bool {
			if e.Signal == nil || e.Signal.From != d.fromID {
				return false
			}
			var m map[string]string
			return json.Unmarshal(e.Signal.Data, &m) == nil && m["canary"] == nonce
		}); err != nil {
			return err
		}
	}
	return nil
}

// wait reads c's events until ok accepts one.
func wait(ctx context.Context, c *sigclient.Client, what string, ok func(sigclient.Event) bool) error {
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: %w", what, ctx.Err())
		case e, open := <-c.Events:
			if !open {
				return fmt.Errorf("%s: session ended", what)
			}
			if e.Err != nil {
				return fmt.Errorf("%s: %w", what, e.Err)
			}
			if ok(e) {
				return nil
			}
		}
	}
}

func httpClient(connect string) *http.Client {
	if connect == "" {
		return nil
	}
	var d net.Dialer
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return d.DialContext(ctx, network, connect)
	}
	return &http.Client{Transport: tr}
}

func token() string {
	b := make([]byte, 12)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
