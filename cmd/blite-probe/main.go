// Command blite-probe checks a blite-signal deployment end to end: it creates a
// room, joins it from a second connection, relays a signal both ways, optionally
// idles to prove keepalives survive the proxy, then leaves.
//
//	blite-probe -url wss://blite.example.com/ws -idle 5m
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/coder/websocket"

	"blite/internal/proto"
)

func main() {
	url := flag.String("url", "ws://localhost:8080/ws", "signaling URL")
	ip := flag.String("ip", "", "connect to this IP instead of resolving the host (DNS cache workaround)")
	idle := flag.Duration("idle", 0, "idle this long before the final signal")
	flag.Parse()

	httpc := http.DefaultClient
	if *ip != "" {
		d := &net.Dialer{Timeout: 10 * time.Second}
		httpc = &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{},
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				_, port, _ := net.SplitHostPort(addr)
				return d.DialContext(ctx, network, net.JoinHostPort(*ip, port))
			},
		}}
	}
	if err := run(*url, httpc, *idle); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("PASS")
}

// peer runs one long-lived reader (which also answers server pings); recv
// takes from its channel so waiting never cancels a Read.
type peer struct {
	ws   *websocket.Conn
	msgs chan map[string]any
	err  chan error
}

func dial(url string, c *http.Client) (*peer, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: c})
	if err != nil {
		return nil, err
	}
	p := &peer{ws: ws, msgs: make(chan map[string]any, 16), err: make(chan error, 1)}
	go func() {
		for {
			_, b, err := ws.Read(context.Background())
			if err != nil {
				p.err <- err
				return
			}
			var m map[string]any
			json.Unmarshal(b, &m)
			p.msgs <- m
		}
	}()
	return p, nil
}

func (p *peer) send(v any) error {
	b, _ := json.Marshal(v)
	return p.ws.Write(context.Background(), websocket.MessageText, b)
}

func (p *peer) recv(want string, d time.Duration) (map[string]any, error) {
	select {
	case m := <-p.msgs:
		if m["t"] != want {
			return m, fmt.Errorf("want %s, got %v", want, m)
		}
		return m, nil
	case err := <-p.err:
		return nil, fmt.Errorf("connection closed: %w", err)
	case <-time.After(d):
		return nil, fmt.Errorf("timeout waiting for %s", want)
	}
}

func run(url string, c *http.Client, idle time.Duration) error {
	const pw = "probe-password-1"
	a, err := dial(url, c)
	if err != nil {
		return fmt.Errorf("dial a: %w", err)
	}
	defer a.ws.CloseNow()
	a.send(proto.Create{T: proto.TCreate, Password: pw, Name: "probe-a"})
	wa, err := a.recv(proto.TWelcome, 10*time.Second)
	if err != nil {
		return err
	}
	fmt.Printf("created room %v, ice=%v\n", wa["room"], wa["iceServers"])

	b, err := dial(url, c)
	if err != nil {
		return fmt.Errorf("dial b: %w", err)
	}
	defer b.ws.CloseNow()
	b.send(proto.Join{T: proto.TJoin, Room: wa["room"].(string), Password: pw, Name: "probe-b"})
	wb, err := b.recv(proto.TWelcome, 10*time.Second)
	if err != nil {
		return err
	}
	if _, err := a.recv(proto.TPeerJoined, 5*time.Second); err != nil {
		return err
	}

	ping := func(label string) error {
		b.send(map[string]any{"t": "signal", "to": wa["peerId"], "data": map[string]string{"probe": label}})
		if _, err := a.recv(proto.TSignal, 10*time.Second); err != nil {
			return fmt.Errorf("b->a %s: %w", label, err)
		}
		a.send(map[string]any{"t": "signal", "to": wb["peerId"], "data": map[string]string{"probe": label}})
		if _, err := b.recv(proto.TSignal, 10*time.Second); err != nil {
			return fmt.Errorf("a->b %s: %w", label, err)
		}
		return nil
	}
	if err := ping("first"); err != nil {
		return err
	}
	fmt.Println("signal relayed both ways")

	if idle > 0 {
		fmt.Println("idling", idle)
		select {
		case err := <-a.err:
			return fmt.Errorf("a died while idle: %w", err)
		case err := <-b.err:
			return fmt.Errorf("b died while idle: %w", err)
		case <-time.After(idle):
		}
		if err := ping("after-idle"); err != nil {
			return err
		}
		fmt.Println("still connected after idle")
	}

	b.send(map[string]any{"t": "leave"})
	if _, err := a.recv(proto.TPeerLeft, 10*time.Second); err != nil {
		return err
	}
	return nil
}
