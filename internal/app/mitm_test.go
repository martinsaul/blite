package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"blite/internal/media"
	"blite/internal/server"
	"blite/internal/session"
)

var fpRe = regexp.MustCompile(`a=fingerprint:sha-256 [0-9A-F:]+`)

// mitm forwards a WebSocket to upstream, rewriting DTLS fingerprints in every
// message sent DOWN to the client: what a compromised signaling server would do.
func mitm(t *testing.T, upstream string) string {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		down, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer down.CloseNow()
		up, _, err := websocket.Dial(r.Context(), upstream, nil)
		if err != nil {
			return
		}
		defer up.CloseNow()
		ctx := r.Context()
		go func() {
			for {
				typ, b, err := down.Read(ctx)
				if err != nil {
					return
				}
				up.Write(ctx, typ, b)
			}
		}()
		for {
			typ, b, err := up.Read(ctx)
			if err != nil {
				return
			}
			b = fpRe.ReplaceAll(b, []byte("a=fingerprint:sha-256 "+strings.Repeat("AA:", 31)+"AA"))
			down.Write(ctx, typ, b)
		}
	})
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return "ws" + strings.TrimPrefix(ts.URL, "http")
}

func TestTamperedFingerprintIsRejected(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	direct := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	a := New(session.Config{Server: direct, Name: "alice", View: media.ViewCount, Log: quiet}, "none")
	b := New(session.Config{Server: mitm(t, direct), Name: "bob", View: media.ViewCount, Log: quiet}, "none")
	defer a.Shutdown()
	defer b.Shutdown()

	ctx := context.Background()
	if err := a.Create(ctx, ""); err != nil {
		t.Fatal(err)
	}
	link := a.State().(UIState).Session.Link
	if err := b.JoinLink(ctx, link); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, m := range b.State().(UIState).Session.Members {
			if m.Name == "alice" && m.State == "untrusted" {
				return // bob refused the forged answer
			}
			if m.Name == "alice" && m.State == "connected" {
				t.Fatal("bob connected to alice through a forged fingerprint")
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no verdict: %+v", b.State().(UIState).Session.Members)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestSafetyCodeMatches(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	a := New(session.Config{Server: ws, Name: "alice", View: media.ViewCount, Log: quiet}, "none")
	b := New(session.Config{Server: ws, Name: "bob", View: media.ViewCount, Log: quiet}, "none")
	defer a.Shutdown()
	defer b.Shutdown()
	ctx := context.Background()
	a.Create(ctx, "")
	if err := b.JoinLink(ctx, a.State().(UIState).Session.Link); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		sa, sb := a.State().(UIState).Session, b.State().(UIState).Session
		if len(sa.Members) == 1 && len(sb.Members) == 1 && sa.Members[0].State == "connected" && sb.Members[0].State == "connected" {
			if sa.Safety != sb.Safety {
				t.Fatalf("safety codes differ: %q vs %q", sa.Safety, sb.Safety)
			}
			t.Logf("safety code %s", sa.Safety)
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("peers never connected")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
