package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"blite/internal/audio"
	"blite/internal/identity"
	"blite/internal/media"
	"blite/internal/proto"
	"blite/internal/server"
)

// End-to-end encryption: in an encrypted room, members decode
// each other's video and audio in a mesh and through a relay, a late
// joiner gets the keys, and media keeps flowing after a member leaves
// (everyone switches to a new key).
func TestE2EEMeshRelayAndRekey(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	cm := &proto.ClientMeta{AppVersion: "0.1.0", Build: proto.E2EEBuild}
	cfg := func(n string) Config {
		return Config{Server: ws, Name: n, View: media.ViewCount, Client: cm, E2EE: true, Log: quiet}
	}
	ctx := context.Background()

	a, err := Create(ctx, cfg("alice"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if !a.State().Encrypted {
		t.Fatal("room not encrypted")
	}
	if _, _, _, linkE2EE, err := ParseLinkServer(a.State().Link); err != nil || !linkE2EE {
		t.Fatalf("encrypted room link lacks marker: %q (%v)", a.State().Link, err)
	}
	if !a.Rejoin().Encrypted {
		t.Fatal("Rejoin did not record Encrypted=true")
	}
	a.SetSource(media.SourceTest)
	a.SetAudio(audio.Selection{Mode: "test"})
	b, err := Join(ctx, cfg("bob"), a.sig.Welcome.Room, a.pw)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	b.SetSource(media.SourceTest)
	if !b.State().Encrypted {
		t.Fatal("joiner doesn't know the room is encrypted")
	}
	framesGrow(t, "mesh: bob decodes alice", b, "alice")
	framesGrow(t, "mesh: alice decodes bob", a, "bob")
	waitFor(t, "bob decodes alice's audio", 20*time.Second, func() bool { return member(b, "alice").Audio > 20 })

	// An older build may not join.
	old := cfg("old")
	old.Client = &proto.ClientMeta{AppVersion: "0.1.0", Build: proto.E2EEBuild - 1}
	if _, err := Join(ctx, old, a.sig.Welcome.Room, a.pw); err == nil || !strings.Contains(err.Error(), proto.ErrUpdateRequired) {
		t.Fatalf("old build joined an encrypted room: %v", err)
	}

	// Carol relays; alice and bob have no direct connection.
	c, err := Join(ctx, cfg("carol"), a.sig.Welcome.Room, a.pw)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	framesGrow(t, "mesh: carol decodes alice", c, "alice")
	if err := c.Volunteer(true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice-bob direct connection pruned", 20*time.Second, func() bool {
		return strings.HasPrefix(member(b, "alice").Pair, "via ") && strings.HasPrefix(member(a, "bob").Pair, "via ")
	})
	framesGrow(t, "relay: bob decodes alice via carol", b, "alice")
	framesGrow(t, "relay: alice decodes bob via carol", a, "bob")

	// A late joiner gets everyone's keys.
	d, err := Join(ctx, cfg("dave"), a.sig.Welcome.Room, a.pw)
	if err != nil {
		t.Fatal(err)
	}
	framesGrow(t, "relay: dave decodes alice", d, "alice")
	framesGrow(t, "relay: dave decodes bob", d, "bob")

	// Dave leaves: everyone rekeys, and media keeps flowing.
	d.Close()
	time.Sleep(2 * keySwitchDelayForTest)
	framesGrow(t, "after rekey: bob decodes alice", b, "alice")
	framesGrow(t, "after rekey: alice decodes bob", a, "bob")
	if a.State().Safety != b.State().Safety {
		t.Fatal("safety codes differ")
	}

	// Without alice's key, bob decodes nothing more from her: the media
	// really is encrypted.
	b.mesh.ForgetKeysOf(a.sig.Welcome.PeerID)
	time.Sleep(time.Second)
	before := member(b, "alice").Frames
	time.Sleep(3 * time.Second)
	if got := member(b, "alice").Frames; got > before+2 {
		t.Fatalf("bob decoded %d frames from alice without her key", got-before)
	}
	framesGrow(t, "alice still decodes bob", a, "bob")
}

const keySwitchDelayForTest = time.Second

// A server that answers an encrypted create (regular or hosted) or an
// encrypted join with a plain room is refused before any media or signaling
// exchange starts. Old links (no marker) still join plain rooms, and dev
// builds log that they create unencrypted rooms.
func TestE2EEDowngradeRefused(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	var welcomeE2EE atomic.Bool
	var signals atomic.Int32
	var leaves atomic.Int32
	peerKey := identity.Ephemeral()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		if _, _, err := ws.Read(r.Context()); err != nil {
			return
		}
		b, _ := json.Marshal(proto.Welcome{
			T:      proto.TWelcome,
			Room:   "7K3QX9MD",
			PeerID: "alice",
			Resume: "tok",
			E2EE:   welcomeE2EE.Load(),
			Peers:  []proto.Peer{{PeerID: "host", Name: "host", IDKey: peerKey.Public(), Build: proto.E2EEBuild}},
		})
		ws.Write(r.Context(), websocket.MessageText, b)
		for {
			_, msg, err := ws.Read(r.Context())
			if err != nil {
				return
			}
			var env proto.Envelope
			if json.Unmarshal(msg, &env) == nil {
				switch env.T {
				case proto.TSignal:
					signals.Add(1)
				case proto.TLeave:
					leaves.Add(1)
				}
			}
		}
	}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http")
	cm := &proto.ClientMeta{AppVersion: "0.1.0", Build: proto.E2EEBuild}
	ctx := context.Background()

	// 1. Regular Create with Welcome.E2EE=false -> ErrE2EEDowngrade, no signals/media.
	cfg := Config{Server: ws, Name: "alice", View: media.ViewCount, Client: cm, E2EE: true, Log: quiet}
	if s, err := Create(ctx, cfg, ""); !errors.Is(err, ErrE2EEDowngrade) {
		if s != nil {
			s.Close()
		}
		t.Fatalf("create with a downgrading server: %v, want ErrE2EEDowngrade", err)
	}
	if signals.Load() != 0 {
		t.Fatalf("downgraded create sent %d signals", signals.Load())
	}

	// 2. A hosted relay rejoining its fixed room keeps the room's setting:
	// an older plain room only warns.
	hostedCfg := cfg
	hostedCfg.HostRoom = "MAMAC0"
	hostedCfg.HostKey = "hk"
	s, err := Create(ctx, hostedCfg, "")
	if err != nil {
		t.Fatalf("hosted create of a plain room: %v", err)
	}
	s.Close()
	signals.Store(0)

	// 3. Join with marker (E2EE=true) + Welcome.E2EE=false -> ErrE2EEDowngrade, leaves, no media/offer.
	leavesBefore := leaves.Load()
	_, code, pw, linkE2EE, err := ParseLinkServer(ShareLink(ws, "7K3QX9MD", "pw1234", true))
	if err != nil || !linkE2EE {
		t.Fatalf("ParseLinkServer: %v e2ee=%v", err, linkE2EE)
	}
	joinCfg := Config{Server: ws, Name: "bob", View: media.ViewCount, Client: cm, E2EE: linkE2EE, Log: quiet}
	if s, err := Join(ctx, joinCfg, code, pw); !errors.Is(err, ErrE2EEDowngrade) {
		if s != nil {
			s.Close()
		}
		t.Fatalf("join with marker against plain room: %v, want ErrE2EEDowngrade", err)
	}
	if signals.Load() != 0 {
		t.Fatalf("downgraded join offered/signaled %d times (media/WebRTC started)", signals.Load())
	}
	waitFor(t, "downgraded join sent leave", 2*time.Second, func() bool { return leaves.Load() > leavesBefore })

	// 4. Join with no marker (old link) + Welcome.E2EE=false -> joins plain room.
	_, code, pw, oldE2EE, err := ParseLinkServer(ShareLink(ws, "7K3QX9MD", "pw1234", false))
	if err != nil || oldE2EE {
		t.Fatalf("ParseLinkServer plain: %v e2ee=%v", err, oldE2EE)
	}
	plainJoinCfg := Config{Server: ws, Name: "bob", View: media.ViewCount, Client: cm, E2EE: oldE2EE, Log: quiet}
	sPlain, err := Join(ctx, plainJoinCfg, code, pw)
	if err != nil {
		t.Fatalf("join without marker against plain room: %v", err)
	}
	if sPlain.State().Encrypted {
		t.Fatal("plain room reported Encrypted=true")
	}
	sPlain.Close()

	// 5. Join with marker + Welcome.E2EE=true -> joins encrypted room.
	welcomeE2EE.Store(true)
	sEnc, err := Join(ctx, joinCfg, code, pw)
	if err != nil {
		t.Fatalf("join with marker against encrypted room: %v", err)
	}
	if !sEnc.State().Encrypted {
		t.Fatal("encrypted room reported Encrypted=false")
	}
	sEnc.Close()

	// 6. Dev build (build 0) creates a plain room on purpose and logs so.
	welcomeE2EE.Store(false)
	var logBuf bytes.Buffer
	var logMu sync.Mutex
	devLog := slog.New(slog.NewTextHandler(&syncWriter{mu: &logMu, w: &logBuf}, nil))
	devCfg := Config{Server: ws, Name: "dev", View: media.ViewCount, Client: &proto.ClientMeta{AppVersion: "dev", Build: 0}, E2EE: true, Log: devLog}
	sDev, err := Create(ctx, devCfg, "")
	if err != nil {
		t.Fatalf("dev create: %v", err)
	}
	sDev.Close()
	logMu.Lock()
	logged := logBuf.String()
	logMu.Unlock()
	if !strings.Contains(logged, "dev build: creating an unencrypted room") {
		t.Fatalf("dev build did not log plain room creation: %s", logged)
	}
}

// When a member's identity key changes across a signaling resume, the session
// raises Member.KeyChanged so the UI warns.
func TestSessionResumedKeyChangedWarning(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(server.Config{Log: quiet})
	upTS := httptest.NewServer(srv)
	defer upTS.Close()
	upWS := "ws" + strings.TrimPrefix(upTS.URL, "http") + "/ws"

	var swapKey atomic.Value
	proxyTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		down, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer down.CloseNow()
		up, _, err := websocket.Dial(r.Context(), upWS, nil)
		if err != nil {
			return
		}
		defer up.CloseNow()
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		go func() {
			defer cancel()
			for {
				typ, b, err := down.Read(ctx)
				if err != nil {
					return
				}
				if up.Write(ctx, typ, b) != nil {
					return
				}
			}
		}()
		for {
			typ, b, err := up.Read(ctx)
			if err != nil {
				return
			}
			if fk, _ := swapKey.Load().(string); fk != "" {
				var env proto.Envelope
				if json.Unmarshal(b, &env) == nil && env.T == proto.TWelcome {
					var wl proto.Welcome
					if json.Unmarshal(b, &wl) == nil {
						for i := range wl.Peers {
							if wl.Peers[i].Name == "bob" {
								wl.Peers[i].IDKey = fk
							}
						}
						b, _ = json.Marshal(wl)
					}
				}
			}
			if down.Write(ctx, typ, b) != nil {
				return
			}
		}
	}))
	defer proxyTS.Close()
	proxyWS := "ws" + strings.TrimPrefix(proxyTS.URL, "http") + "/ws"

	ctx := context.Background()
	cm := &proto.ClientMeta{AppVersion: "0.1.0", Build: proto.E2EEBuild}
	a, err := Create(ctx, Config{Server: proxyWS, Name: "alice", View: media.ViewCount, Client: cm, E2EE: true, Log: quiet}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Join(ctx, Config{Server: upWS, Name: "bob", View: media.ViewCount, Client: cm, E2EE: true, Log: quiet}, a.sig.Welcome.Room, a.pw)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	waitFor(t, "alice connected to bob", 15*time.Second, func() bool {
		return member(a, "bob").State == "connected"
	})
	if member(a, "bob").KeyChanged {
		t.Fatal("KeyChanged set before key changed")
	}

	newKey := identity.Ephemeral().Public()
	swapKey.Store(newKey)
	a.sig.Drop()

	waitFor(t, "KeyChanged raised for bob after resume", 10*time.Second, func() bool {
		m := member(a, "bob")
		return m.KeyChanged && m.Tag == identity.Tag(newKey)
	})
}

type syncWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
