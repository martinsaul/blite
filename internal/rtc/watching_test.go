package rtc

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"

	"blite/internal/identity"
	"blite/internal/proto"
	"blite/internal/room"
	"blite/internal/server"
	"blite/internal/sigclient"
)

type capRecord struct {
	to     string
	origin string
	wire   int
}

// startCountingServer runs a signaling server fronted by a WebSocket proxy that
// records every MaxLayer signal and counts SDP signals sent by clients.
// If stripWatchers is true (via setStripWatchers), the proxy strips the
// "watchers" field from MaxLayer signals to simulate an older relay build.
func startCountingServer(t *testing.T) (wsURL string, caps func() []capRecord, sdps func() int, resetCounts func()) {
	ws, c, s, r, _ := startCountingServerWithLegacy(t)
	return ws, c, s, r
}

func startCountingServerWithLegacy(t *testing.T) (wsURL string, caps func() []capRecord, sdps func() int, resetCounts func(), setStripWatchers func(bool)) {
	t.Helper()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	upstream := httptest.NewServer(server.New(server.Config{Log: quiet}))
	t.Cleanup(upstream.Close)
	upWS := "ws" + strings.TrimPrefix(upstream.URL, "http") + "/ws"

	var mu sync.Mutex
	var recorded []capRecord
	var sdpCount int
	var stripWatchers atomic.Bool

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cConn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer cConn.CloseNow()
		cConn.SetReadLimit(1 << 20)

		uConn, _, err := websocket.Dial(r.Context(), upWS, nil)
		if err != nil {
			return
		}
		defer uConn.CloseNow()
		uConn.SetReadLimit(1 << 20)

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		go func() {
			defer cancel()
			for {
				typ, data, err := uConn.Read(ctx)
				if err != nil {
					return
				}
				if err := cConn.Write(ctx, typ, data); err != nil {
					return
				}
			}
		}()

		for {
			typ, data, err := cConn.Read(ctx)
			if err != nil {
				return
			}
			var env proto.Envelope
			if json.Unmarshal(data, &env) == nil && env.T == proto.TSignal {
				var sig proto.Signal
				if json.Unmarshal(data, &sig) == nil {
					var sm signalMsg
					if json.Unmarshal(sig.Data, &sm) == nil {
						mu.Lock()
						if sm.SDP != nil {
							sdpCount++
						}
						if sm.MaxLayer != nil {
							recorded = append(recorded, capRecord{
								to:     sig.To,
								origin: sm.Origin,
								wire:   *sm.MaxLayer,
							})
						}
						mu.Unlock()
						if stripWatchers.Load() && sm.MaxLayer != nil && sm.Watchers != nil {
							sm.Watchers = nil
							if raw, err := json.Marshal(sm); err == nil {
								sig.Data = raw
								if out, err := json.Marshal(sig); err == nil {
									data = out
								}
							}
						}
					}
				}
			}
			if err := uConn.Write(ctx, typ, data); err != nil {
				return
			}
		}
	}))
	t.Cleanup(proxy.Close)

	return "ws" + strings.TrimPrefix(proxy.URL, "http") + "/ws",
		func() []capRecord {
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(recorded)
		},
		func() int {
			mu.Lock()
			defer mu.Unlock()
			return sdpCount
		},
		func() {
			mu.Lock()
			recorded = nil
			sdpCount = 0
			mu.Unlock()
		},
		func(on bool) {
			stripWatchers.Store(on)
		}
}

func waitMesh(t *testing.T, desc string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", desc)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func startIDRWriter(t *testing.T, m *Mesh) {
	t.Helper()
	idr := []byte{0, 0, 0, 1, 0x67, 1, 2, 0, 0, 1, 0x68, 3, 0, 0, 1, 0x65, 9}
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		tk := time.NewTicker(25 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-m.Done():
				return
			case <-tk.C:
				m.WriteVideo(layerHigh, idr, 33*time.Millisecond)
				m.WriteVideo(layerLow, idr, 33*time.Millisecond)
			}
		}
	}()
}

// TestInitialWireCapsUnit verifies initialWireCaps across mesh, relayed viewer,
// and relay roles (where relays return nil so all upstream caps go through
// m.demand).
func TestInitialWireCapsUnit(t *testing.T) {
	m := newTestMeshForUpload(t)
	m.members = map[string]bool{"alice": true, "bob": true, "rel": true}
	m.caps = map[string]int{}
	m.demand = map[demandKey]demandVal{}
	m.served = map[string]bool{}
	m.sig = &sigclient.Client{Welcome: proto.Welcome{PeerID: "me"}}

	// 1. DefaultOff false, no explicit caps -> nil.
	if got := m.initialWireCaps("alice"); len(got) != 0 {
		t.Fatalf("DefaultOff=false initialWireCaps = %v, want empty", got)
	}

	// 2. DefaultOff true in mesh mode -> direct wireOff to target only.
	m.SetDefaultOff(true)
	wantMesh := []initCap{{wire: wireOff}}
	if got := m.initialWireCaps("alice"); !slices.Equal(got, wantMesh) {
		t.Fatalf("DefaultOff=true mesh initialWireCaps = %v, want %v", got, wantMesh)
	}

	// 3. Explicit wireHigh overrides DefaultOff and is included so new
	// connections learn the explicit wish.
	m.caps["alice"] = wireHigh
	wantExplicit := []initCap{{wire: wireHigh}}
	if got := m.initialWireCaps("alice"); !slices.Equal(got, wantExplicit) {
		t.Fatalf("explicit wireHigh initialWireCaps = %v, want %v", got, wantExplicit)
	}
	delete(m.caps, "alice")

	// 4. Relayed viewer (homed on "rel"): sends direct wireOff to rel plus
	// per-origin wireOff for non-direct members sorted by ID.
	m.plan = proto.Plan{
		Epoch:  1,
		Relays: []string{"rel"},
		Home:   map[string]string{"me": "rel", "alice": "rel", "bob": "rel"},
	}
	if got := m.initialWireCaps("alice"); len(got) != 0 {
		t.Fatalf("non-direct target initialWireCaps = %v, want empty", got)
	}
	wantRelayed := []initCap{
		{wire: wireOff},
		{origin: "alice", wire: wireOff},
		{origin: "bob", wire: wireOff},
	}
	if got := m.initialWireCaps("rel"); !slices.Equal(got, wantRelayed) {
		t.Fatalf("relayed viewer initialWireCaps(rel) = %v, want %v", got, wantRelayed)
	}

	// 5. Relay peer itself (forwardingLocked == true): returns nil even when
	// Headless or DefaultOff and len(m.peers) <= 1, so upstream "off" always
	// goes through m.demand.
	m.opt.Headless = true
	m.plan = proto.Plan{
		Epoch:  2,
		Relays: []string{"me"},
		Home:   map[string]string{"alice": "me", "bob": "me"},
	}
	addTestRemote(m, "alice", time.Now())
	if got := m.initialWireCaps("alice"); got != nil {
		t.Fatalf("relay initialWireCaps = %v, want nil", got)
	}
}

// TestCheckWatchingSetTopZeroNoStaleState exercises the exact interleaving
// where checkWatching starts while top > 0, SetTop(0) runs concurrently and
// clears watchingPeer, and checkWatching then finishes acquiring m.mu.
func TestCheckWatchingSetTopZeroNoStaleState(t *testing.T) {
	wsURL, _, _, _ := startCountingServer(t)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	var notifications atomic.Int32
	ak := identity.Ephemeral()
	asig, err := sigclient.Create(ctx, wsURL, proto.Create{Name: "alice", IDKey: ak.Public()}, sigclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	alice, err := New(asig, Options{
		Identity:         ak,
		OnViewerWatching: func(string) { notifications.Add(1) },
		Log:              quiet,
	})
	if err != nil {
		asig.Leave()
		t.Fatal(err)
	}
	defer alice.Close()

	bk := identity.Ephemeral()
	bsig, err := sigclient.Join(ctx, wsURL, proto.Join{Room: asig.Welcome.Room, Name: "bob", IDKey: bk.Public()}, sigclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := New(bsig, Options{Identity: bk, Log: quiet})
	if err != nil {
		bsig.Leave()
		t.Fatal(err)
	}
	defer bob.Close()

	waitMesh(t, "bob connected to alice", 20*time.Second, func() bool {
		alice.mu.Lock()
		r := alice.peers[bob.Me()]
		alice.mu.Unlock()
		return r != nil && r.connected.Load()
	})
	if err := bob.SetMaxLayer(alice.Me(), wireHigh); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)

	alice.mu.Lock()
	r := alice.peers[bob.Me()]
	alice.mu.Unlock()

	// Start sharing while connected=false so checkAllWatching leaves watchingPeer
	// empty, then restore connected=true right before parking checkWatching on r.polMu.
	r.connected.Store(false)
	alice.SetTop(720)
	r.connected.Store(true)
	notifications.Store(0)

	r.polMu.Lock()
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		alice.checkWatching(bob.Me())
	}()
	time.Sleep(15 * time.Millisecond)

	stopDone := make(chan struct{})
	go func() {
		defer close(stopDone)
		alice.SetTop(0)
	}()
	time.Sleep(15 * time.Millisecond)

	r.polMu.Unlock()
	<-watchDone
	<-stopDone

	alice.mu.Lock()
	stale := alice.watchingPeer[bob.Me()]
	alice.mu.Unlock()
	if stale || notifications.Load() != 0 {
		t.Fatalf("stale watching state after SetTop(0): watchingPeer[bob]=%v notifications=%d, want false/0",
			stale, notifications.Load())
	}
}

// TestCheckWatchingExplicitCapAndNoExtraSDPTraffic verifies:
// 1. A joining peer connecting without sending MaxLayer does NOT count as watching.
// 2. Sending wireOff still does not count as watching; sending wireLow/wireHigh does.
// 3. ICE restarts (offer + answer) emit zero extra MaxLayer messages.
// 4. New connections do not restore stale wireCap from a previous connection.
func TestCheckWatchingExplicitCapAndNoExtraSDPTraffic(t *testing.T) {
	wsURL, caps, sdps, resetCounts := startCountingServer(t)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	cm := &proto.ClientMeta{AppVersion: "0.2.0", Build: room.DefaultMinRelayBuild}

	var watchMu sync.Mutex
	var watchedBy []string
	recordWatch := func(id string) {
		watchMu.Lock()
		watchedBy = append(watchedBy, id)
		watchMu.Unlock()
	}
	watched := func() []string {
		watchMu.Lock()
		defer watchMu.Unlock()
		return slices.Clone(watchedBy)
	}

	aliceKey := identity.Ephemeral()
	aliceSig, err := sigclient.Create(ctx, wsURL, proto.Create{
		Name:   "alice",
		IDKey:  aliceKey.Public(),
		Client: cm,
	}, sigclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	alice, err := New(aliceSig, Options{
		Identity:         aliceKey,
		OnViewerWatching: recordWatch,
		Log:              quiet,
	})
	if err != nil {
		aliceSig.Leave()
		t.Fatal(err)
	}
	defer alice.Close()
	alice.SetTop(720)
	startIDRWriter(t, alice)

	// Bob joins with DefaultOff=false and does NOT call SetMaxLayer yet.
	var bobFrames atomic.Int32
	bobKey := identity.Ephemeral()
	bobSig, err := sigclient.Join(ctx, wsURL, proto.Join{
		Room:   aliceSig.Welcome.Room,
		Name:   "bob",
		IDKey:  bobKey.Public(),
		Client: cm,
	}, sigclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := New(bobSig, Options{
		Identity: bobKey,
		OnVideo:  func(string, []byte) { bobFrames.Add(1) },
		Log:      quiet,
	})
	if err != nil {
		bobSig.Leave()
		t.Fatal(err)
	}
	defer bob.Close()

	waitMesh(t, "bob connected and receiving frames", 20*time.Second, func() bool {
		return bobFrames.Load() >= 3
	})
	time.Sleep(150 * time.Millisecond)

	// Item 1: Bob connected without explicitly sending MaxLayer -> must NOT count as watching.
	if got := watched(); len(got) != 0 {
		t.Fatalf("watched before explicit MaxLayer = %v, want empty", got)
	}
	if got := caps(); len(got) != 0 {
		t.Fatalf("unexpected initial MaxLayer messages with DefaultOff=false: %v", got)
	}

	// Explicit wireOff -> still not watching, and forwarder stops sending video.
	if err := bob.SetMaxLayer(alice.Me(), wireOff); err != nil {
		t.Fatal(err)
	}
	waitMesh(t, "alice layer to bob becomes off", 5*time.Second, func() bool {
		return alice.Layers()[bob.Me()] == layerOff
	})
	if got := watched(); len(got) != 0 {
		t.Fatalf("watched after wireOff = %v, want empty", got)
	}

	// Explicit wireLow -> counts as watching once!
	if err := bob.SetMaxLayer(alice.Me(), wireLow); err != nil {
		t.Fatal(err)
	}
	waitMesh(t, "alice notified bob is watching", 5*time.Second, func() bool {
		return slices.Equal(watched(), []string{bob.Me()})
	})

	// Switching wireLow -> wireHigh while continuously watching does not re-notify.
	if err := bob.SetMaxLayer(alice.Me(), wireHigh); err != nil {
		t.Fatal(err)
	}
	waitMesh(t, "alice layer to bob becomes high", 5*time.Second, func() bool {
		return alice.Layers()[bob.Me()] == layerHigh
	})
	if got := watched(); !slices.Equal(got, []string{bob.Me()}) {
		t.Fatalf("watched after wireLow->wireHigh = %v, want [%s]", got, bob.Me())
	}

	// Item 6: ICE restart (offer + answer) must send ZERO MaxLayer messages.
	resetCounts()
	bob.mu.Lock()
	rAlice := bob.peers[alice.Me()]
	bob.mu.Unlock()
	if rAlice == nil {
		t.Fatal("bob missing remote for alice")
	}
	if err := bob.restartICE(rAlice); err != nil {
		t.Fatal(err)
	}
	waitMesh(t, "ICE restart offer+answer completed", 5*time.Second, func() bool {
		return sdps() >= 2 && rAlice.pc.SignalingState() == webrtc.SignalingStateStable
	})
	time.Sleep(100 * time.Millisecond)
	if got := caps(); len(got) != 0 {
		t.Fatalf("MaxLayer messages during ICE restart = %v, want empty", got)
	}

	// Item 3: Capping to wireOff on old connection must NOT be cached onto a
	// brand-new connection on alice (no stale wireCaps restoration).
	if err := bob.SetMaxLayer(alice.Me(), wireOff); err != nil {
		t.Fatal(err)
	}
	waitMesh(t, "alice capped to off before newPeer check", 5*time.Second, func() bool {
		return alice.Layers()[bob.Me()] == layerOff
	})
	fresh, err := alice.newPeer(bob.Me())
	if err != nil {
		t.Fatal(err)
	}
	fresh.polMu.Lock()
	freshCap, freshHas := fresh.wireCap, fresh.hasWireCap
	fresh.polMu.Unlock()
	if freshCap != levelHigh || freshHas {
		t.Fatalf("fresh newPeer restored stale cap: wireCap=%d hasWireCap=%v, want levelHigh/false", freshCap, freshHas)
	}
}

// TestDefaultOffAndRelayTopology verifies:
//   - Relay peer itself is NOT misreported as a watching viewer; OnViewerWatching
//     receives the real child viewer's ID when the relay forwards watchers (Item 2 & Fix 7).
//   - Relay "off" decision goes through m.demand and matches what was sent (Item 4).
//   - DefaultOff in a relay topology keeps upstream off until a child explicitly
//     requests video, then delivers video and returns upstream to off when unwatched (Item 7).
func TestDefaultOffAndRelayTopology(t *testing.T) {
	wsURL, _, _, _ := startCountingServer(t)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	cm := &proto.ClientMeta{AppVersion: "0.2.0", Build: room.DefaultMinRelayBuild}

	var watchMu sync.Mutex
	var watchedBy []string
	aliceKey := identity.Ephemeral()
	aliceSig, err := sigclient.Create(ctx, wsURL, proto.Create{
		Name:   "alice",
		IDKey:  aliceKey.Public(),
		Client: cm,
	}, sigclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	alice, err := New(aliceSig, Options{
		Identity:   aliceKey,
		DefaultOff: true,
		OnViewerWatching: func(id string) {
			watchMu.Lock()
			watchedBy = append(watchedBy, id)
			watchMu.Unlock()
		},
		Log: quiet,
	})
	if err != nil {
		aliceSig.Leave()
		t.Fatal(err)
	}
	defer alice.Close()
	alice.SetTop(720)
	startIDRWriter(t, alice)

	// Relay joins with DefaultOff=true and volunteers as relay before any viewer joins.
	relKey := identity.Ephemeral()
	relSig, err := sigclient.Join(ctx, wsURL, proto.Join{
		Room:   aliceSig.Welcome.Room,
		Name:   "relay",
		IDKey:  relKey.Public(),
		Client: cm,
	}, sigclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	rel, err := New(relSig, Options{
		Identity:   relKey,
		DefaultOff: true,
		Log:        quiet,
	})
	if err != nil {
		relSig.Leave()
		t.Fatal(err)
	}
	defer rel.Close()
	if err := rel.Volunteer(true); err != nil {
		t.Fatal(err)
	}

	// Item 4: Relay's "off" decision goes through rel.demand and alice caps rel to off.
	waitMesh(t, "relay active and demand[alice]=wireOff", 20*time.Second, func() bool {
		rel.mu.Lock()
		d, ok := rel.demand[demandKey{to: alice.Me()}]
		rel.mu.Unlock()
		return slices.Contains(alice.Relays(), rel.Me()) &&
			ok && d.wire == wireOff && len(d.watchers) == 0 &&
			alice.Layers()[rel.Me()] == layerOff
	})

	// Viewer carol joins with DefaultOff=true behind the relay.
	var carolFrames atomic.Int32
	var carolTop atomic.Int32
	carolKey := identity.Ephemeral()
	carolSig, err := sigclient.Join(ctx, wsURL, proto.Join{
		Room:   aliceSig.Welcome.Room,
		Name:   "carol",
		IDKey:  carolKey.Public(),
		Client: cm,
	}, sigclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	carol, err := New(carolSig, Options{
		Identity:   carolKey,
		DefaultOff: true,
		OnVideo: func(id string, _ []byte) {
			if id == alice.Me() {
				carolFrames.Add(1)
			}
		},
		OnSending: func(origin string, _, top int, _ bool) {
			if origin == alice.Me() {
				carolTop.Store(int32(top))
			}
		},
		Log: quiet,
	})
	if err != nil {
		carolSig.Leave()
		t.Fatal(err)
	}
	defer carol.Close()

	// Carol connects only to relay, learns alice is sharing (top=720), and gets 0 frames while DefaultOff.
	waitMesh(t, "carol homed on relay and learns alice top=720", 20*time.Second, func() bool {
		return carol.Home(carol.Me()) == rel.Me() && carolTop.Load() == 720
	})
	time.Sleep(250 * time.Millisecond)
	if n := carolFrames.Load(); n != 0 {
		t.Fatalf("carol received %d frames while DefaultOff=true, want 0", n)
	}
	rel.mu.Lock()
	dOff := rel.demand[demandKey{to: alice.Me()}]
	rel.mu.Unlock()
	if dOff.wire != wireOff || alice.Layers()[rel.Me()] != layerOff {
		t.Fatalf("expected upstream still off with DefaultOff viewer: demand=%+v layer=%d", dOff, alice.Layers()[rel.Me()])
	}
	watchMu.Lock()
	gotBefore := slices.Clone(watchedBy)
	watchMu.Unlock()
	if len(gotBefore) != 0 {
		t.Fatalf("OnViewerWatching before carol watched = %v, want empty", gotBefore)
	}

	// Carol explicitly requests alice's video -> relay updates demand to wireHigh
	// with watchers=[carol] and forwards frames; alice gets carol's ID (not relay's).
	if err := carol.SetMaxLayer(alice.Me(), wireHigh); err != nil {
		t.Fatal(err)
	}
	waitMesh(t, "carol receives alice frames via relay and alice chimes for carol", 20*time.Second, func() bool {
		rel.mu.Lock()
		d := rel.demand[demandKey{to: alice.Me()}]
		rel.mu.Unlock()
		watchMu.Lock()
		got := slices.Clone(watchedBy)
		watchMu.Unlock()
		return d.wire == wireHigh && slices.Equal(d.watchers, []string{carol.Me()}) &&
			carolFrames.Load() >= 5 && slices.Equal(got, []string{carol.Me()})
	})
	alice.mu.Lock()
	relMarked := alice.watchingPeer[rel.Me()]
	carolMarked := alice.watchingPeer[carol.Me()]
	alice.mu.Unlock()
	if relMarked || !carolMarked {
		t.Fatalf("alice.watchingPeer[rel]=%v [carol]=%v, want false/true", relMarked, carolMarked)
	}

	// Carol turns alice off -> relay demand returns to wireOff with empty watchers.
	if err := carol.SetMaxLayer(alice.Me(), wireOff); err != nil {
		t.Fatal(err)
	}
	waitMesh(t, "relay demand returns to wireOff and carol cleared from watchingPeer", 5*time.Second, func() bool {
		rel.mu.Lock()
		d := rel.demand[demandKey{to: alice.Me()}]
		rel.mu.Unlock()
		alice.mu.Lock()
		stillWatching := alice.watchingPeer[carol.Me()]
		alice.mu.Unlock()
		return d.wire == wireOff && len(d.watchers) == 0 &&
			alice.Layers()[rel.Me()] == layerOff && !stillWatching
	})
}

// TestPushDemandUpdatesSentCap verifies that pushDemand's demand sends and undo
// path update r.sentCap via signalCap so a later deduped resendCaps never
// suppresses a needed cap resend (both direct and trunk/relayed origins).
func TestPushDemandUpdatesSentCap(t *testing.T) {
	wsURL, caps, _, resetCounts := startCountingServer(t)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	cm := &proto.ClientMeta{AppVersion: "0.2.0", Build: proto.MultiRelayBuild}

	joinPeer := func(roomCode, name string, defOff bool) (*sigclient.Client, *Mesh) {
		t.Helper()
		k := identity.Ephemeral()
		var sig *sigclient.Client
		var err error
		if roomCode == "" {
			sig, err = sigclient.Create(ctx, wsURL, proto.Create{Name: name, IDKey: k.Public(), Client: cm}, sigclient.Options{})
		} else {
			sig, err = sigclient.Join(ctx, wsURL, proto.Join{Room: roomCode, Name: name, IDKey: k.Public(), Client: cm}, sigclient.Options{})
		}
		if err != nil {
			t.Fatal(err)
		}
		m, err := New(sig, Options{Identity: k, DefaultOff: defOff, Log: quiet})
		if err != nil {
			sig.Leave()
			t.Fatal(err)
		}
		t.Cleanup(m.Close)
		return sig, m
	}

	// 1. Direct demand + undo: carol starts with DefaultOff=true (sending wireOff
	// to alice -> sentCap[""]=wireOff), becomes relay for watching bob (pushDemand
	// sends wireHigh -> must update sentCap[""]=wireHigh), then bob leaves and
	// carol steps down with demand cleared: resendCaps must NOT be suppressed by
	// a stale sentCap[""]=wireOff.
	aliceSig, alice := joinPeer("", "alice", false)
	alice.SetTop(720)
	startIDRWriter(t, alice)

	_, carol := joinPeer(aliceSig.Welcome.Room, "carol", true)
	waitMesh(t, "alice initially capped to off by carol", 20*time.Second, func() bool {
		return alice.Layers()[carol.Me()] == layerOff
	})

	if err := carol.Volunteer(true); err != nil {
		t.Fatal(err)
	}
	_, bob := joinPeer(aliceSig.Welcome.Room, "bob", false)
	if err := bob.SetMaxLayer(alice.Me(), wireHigh); err != nil {
		t.Fatal(err)
	}
	waitMesh(t, "carol relays alice at high for bob", 20*time.Second, func() bool {
		return alice.Layers()[carol.Me()] == layerHigh
	})

	carol.mu.Lock()
	rAlice := carol.peers[alice.Me()]
	carol.mu.Unlock()
	if rAlice == nil {
		t.Fatal("carol missing remote for alice")
	}
	rAlice.polMu.Lock()
	gotSent := rAlice.sentCap[""]
	rAlice.polMu.Unlock()
	if gotSent != wireHigh {
		t.Fatalf("carol sentCap to alice after pushDemand = %d, want wireHigh (%d)", gotSent, wireHigh)
	}

	// Clear demand to isolate resendCaps deduplication when carol stops relaying.
	bob.Close()
	waitMesh(t, "bob gone from carol", 5*time.Second, func() bool {
		carol.mu.Lock()
		defer carol.mu.Unlock()
		return !carol.members[bob.Me()]
	})
	// Re-arm alice at high via pushDemand before clearing demand and stepping down.
	if err := carol.SetMaxLayer(alice.Me(), wireHigh); err != nil {
		t.Fatal(err)
	}
	waitMesh(t, "alice back at high", 5*time.Second, func() bool {
		return alice.Layers()[carol.Me()] == layerHigh
	})
	carol.mu.Lock()
	delete(carol.caps, alice.Me()) // carol's own wish returns to DefaultOff (wireOff)
	clear(carol.demand)            // force resendCaps (not demand map) to carry the wireOff resend
	carol.mu.Unlock()
	if err := carol.Volunteer(false); err != nil {
		t.Fatal(err)
	}
	waitMesh(t, "resendCaps sends wireOff to alice after pushDemand updated sentCap", 20*time.Second, func() bool {
		return len(carol.Relays()) == 0 && alice.Layers()[carol.Me()] == layerOff
	})

	// 2. Undo path on trunk origin (k.origin != "") + sentCap update:
	// Join a second peer r2, put carol in relayed-on-r2 state where carol had
	// previously demanded wireLow for alice over the trunk, and verify pushDemand's
	// undo path sends wireOff to r2 once and updates r2.sentCap[alice]=wireOff.
	_, r2 := joinPeer(aliceSig.Welcome.Room, "r2", true)
	waitMesh(t, "carol connected to r2", 20*time.Second, func() bool {
		carol.mu.Lock()
		defer carol.mu.Unlock()
		return carol.peers[r2.Me()] != nil
	})
	carol.mu.Lock()
	rR2 := carol.peers[r2.Me()]
	clear(carol.served)
	carol.plan = proto.Plan{
		Epoch:  99,
		Relays: []string{r2.Me()},
		Home:   map[string]string{carol.Me(): r2.Me(), alice.Me(): r2.Me()},
	}
	carol.demand = map[demandKey]demandVal{{to: r2.Me(), origin: alice.Me()}: {wire: wireLow, watchers: []string{}}}
	carol.mu.Unlock()
	rR2.polMu.Lock()
	rR2.sentCap[alice.Me()] = wireLow
	rR2.sentWatchers = map[string][]string{alice.Me(): {}}
	rR2.polMu.Unlock()

	resetCounts()
	carol.pushDemand() // undo path (!forwardingLocked)
	carol.resendCaps() // must be deduped because undo updated sentCap[alice]=wireOff
	time.Sleep(100 * time.Millisecond)

	rR2.polMu.Lock()
	undoSent := rR2.sentCap[alice.Me()]
	_, stillHasWatchers := rR2.sentWatchers[alice.Me()]
	rR2.polMu.Unlock()
	if undoSent != wireOff || stillHasWatchers {
		t.Fatalf("r2.sentCap[alice] after pushDemand undo = %d (hasWatchers=%v), want wireOff (%d)/false", undoSent, stillHasWatchers, wireOff)
	}
	var toR2ForAlice []capRecord
	for _, c := range caps() {
		if c.to == r2.Me() && c.origin == alice.Me() {
			toR2ForAlice = append(toR2ForAlice, c)
		}
	}
	if len(toR2ForAlice) != 1 || toR2ForAlice[0].wire != wireOff {
		t.Fatalf("expected exactly 1 undo wireOff to r2 for alice, got %v", toR2ForAlice)
	}
}

// TestRelayWatchersTrunkSwitchLeaveAndLegacy covers all M11.7 watcher-set cases:
// 1. Viewer behind one relay (and relay's own local viewer).
// 2. Viewer behind two relays over a trunk (plus deduplication across relays).
// 3. Viewer switching relays without a second chime.
// 4. Relay leaving clearing its reported watcher set.
// 5. Old-style relay that never sends the Watchers field (no chime).
func TestRelayWatchersTrunkSwitchLeaveAndLegacy(t *testing.T) {
	wsURL, _, _, _, setStripWatchers := startCountingServerWithLegacy(t)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	cm := &proto.ClientMeta{AppVersion: "0.2.0", Build: proto.MultiRelayBuild}

	var watchMu sync.Mutex
	var events []string
	watched := func() []string {
		watchMu.Lock()
		defer watchMu.Unlock()
		return slices.Clone(events)
	}
	clearWatched := func() {
		watchMu.Lock()
		events = nil
		watchMu.Unlock()
	}

	joinPeer := func(roomCode, name string, defOff bool, onWatch func(string)) (*sigclient.Client, *Mesh) {
		t.Helper()
		k := identity.Ephemeral()
		var sig *sigclient.Client
		var err error
		if roomCode == "" {
			sig, err = sigclient.Create(ctx, wsURL, proto.Create{Name: name, IDKey: k.Public(), Client: cm}, sigclient.Options{})
		} else {
			sig, err = sigclient.Join(ctx, wsURL, proto.Join{Room: roomCode, Name: name, IDKey: k.Public(), Client: cm}, sigclient.Options{})
		}
		if err != nil {
			t.Fatal(err)
		}
		m, err := New(sig, Options{
			Identity:         k,
			DefaultOff:       defOff,
			OnViewerWatching: onWatch,
			Log:              quiet,
		})
		if err != nil {
			sig.Leave()
			t.Fatal(err)
		}
		t.Cleanup(m.Close)
		return sig, m
	}

	// Setup multi-relay room: r1 relays first, r2 queues, alice shares on r1.
	r1Sig, r1 := joinPeer("", "r1", true, nil)
	if err := r1.Volunteer(true); err != nil {
		t.Fatal(err)
	}
	roomCode := r1Sig.Welcome.Room

	_, r2 := joinPeer(roomCode, "r2", true, nil)
	if err := r2.Volunteer(true); err != nil {
		t.Fatal(err)
	}

	_, alice := joinPeer(roomCode, "alice", true, func(id string) {
		watchMu.Lock()
		events = append(events, id)
		watchMu.Unlock()
	})
	alice.SetTop(720)
	startIDRWriter(t, alice)

	_, bob := joinPeer(roomCode, "bob", true, nil)
	waitMesh(t, "alice and bob homed on r1", 20*time.Second, func() bool {
		return alice.Home(alice.Me()) == r1.Me() && bob.Home(bob.Me()) == r1.Me()
	})

	// 1. Viewer bob behind one relay r1, plus r1's own local viewer.
	if err := bob.SetMaxLayer(alice.Me(), wireHigh); err != nil {
		t.Fatal(err)
	}
	waitMesh(t, "alice chimes for bob behind r1", 20*time.Second, func() bool {
		return slices.Equal(watched(), []string{bob.Me()})
	})
	if err := r1.SetMaxLayer(alice.Me(), wireLow); err != nil {
		t.Fatal(err)
	}
	waitMesh(t, "alice chimes for r1 local viewer", 20*time.Second, func() bool {
		return slices.Equal(watched(), []string{bob.Me(), r1.Me()})
	})
	if err := r1.SetMaxLayer(alice.Me(), wireOff); err != nil {
		t.Fatal(err)
	}
	waitMesh(t, "r1 local viewer removed from watchingPeer", 5*time.Second, func() bool {
		alice.mu.Lock()
		defer alice.mu.Unlock()
		return !alice.watchingPeer[r1.Me()] && alice.watchingPeer[bob.Me()]
	})

	// 2. Outgrow r1 (4 children: r2-was-child, alice, bob, carol, dave -> r2 becomes
	// active second relay; erin joins homed on r2 and watches alice over trunk r2->r1->alice).
	_, _ = joinPeer(roomCode, "carol", true, nil)
	_, _ = joinPeer(roomCode, "dave", true, nil)
	waitMesh(t, "two active relays r1 and r2", 20*time.Second, func() bool {
		return len(alice.Relays()) == 2
	})
	_, erin := joinPeer(roomCode, "erin", true, nil)
	waitMesh(t, "erin homed on r2 and r1-r2 trunk connected", 20*time.Second, func() bool {
		r1.mu.Lock()
		tr := r1.peers[r2.Me()]
		r1.mu.Unlock()
		return erin.Home(erin.Me()) == r2.Me() && tr != nil && tr.connected.Load()
	})

	clearWatched()
	if err := erin.SetMaxLayer(alice.Me(), wireHigh); err != nil {
		t.Fatal(err)
	}
	waitMesh(t, "alice chimes for erin behind two relays (r2->r1->alice)", 20*time.Second, func() bool {
		return slices.Equal(watched(), []string{erin.Me()})
	})

	// 3. Viewer switching relays without a second chime: r1 withdraws so bob and
	// alice move from r1 onto r2 while bob and erin stay watching.
	clearWatched()
	if err := r1.Volunteer(false); err != nil {
		t.Fatal(err)
	}
	waitMesh(t, "bob and alice re-homed onto r2 and r2 reports bob+erin", 15*time.Second, func() bool {
		alice.mu.Lock()
		rR2 := alice.peers[r2.Me()]
		alice.mu.Unlock()
		if rR2 == nil || !rR2.connected.Load() {
			return false
		}
		rR2.polMu.Lock()
		ws := slices.Clone(rR2.watchers)
		rR2.polMu.Unlock()
		return alice.Home(alice.Me()) == r2.Me() &&
			bob.Home(bob.Me()) == r2.Me() &&
			slices.Contains(ws, bob.Me()) &&
			slices.Contains(ws, erin.Me())
	})
	time.Sleep(250 * time.Millisecond)
	if got := watched(); len(got) != 0 {
		t.Fatalf("unexpected second chime when switching relays r1->r2: %v", got)
	}
	// Verify r1's old watcher set on alice was cleared on re-home.
	alice.mu.Lock()
	rR1 := alice.peers[r1.Me()]
	alice.mu.Unlock()
	if rR1 != nil {
		rR1.polMu.Lock()
		staleR1 := slices.Clone(rR1.watchers)
		rR1.polMu.Unlock()
		if len(staleR1) != 0 {
			t.Fatalf("r1 watchers not cleared on re-home: %v", staleR1)
		}
	}

	// 4. Relay leaving: r2 leaves the room -> r2's reported watcher set is cleared
	// and alice.watchingPeer clears bob and erin (who have DefaultOff / haven't connected directly yet).
	// Set bob and erin back to DefaultOff without explicit caps before r2 leaves so
	// they do not immediately re-watch when mesh fallback connects.
	bob.mu.Lock()
	delete(bob.caps, alice.Me())
	bob.mu.Unlock()
	erin.mu.Lock()
	delete(erin.caps, alice.Me())
	erin.mu.Unlock()

	r2.Close()
	waitMesh(t, "r2 left and alice.watchingPeer cleared", 20*time.Second, func() bool {
		alice.mu.Lock()
		defer alice.mu.Unlock()
		return !alice.members[r2.Me()] && len(alice.watchingPeer) == 0
	})

	// 5. Old-style relay that never sends Watchers: in a fresh room with
	// Watchers stripped on the wire, have bob2 watch alice2 through oldRel ->
	// video flows (layerHigh), 0 chimes.
	setStripWatchers(true)
	defer setStripWatchers(false)

	var legacyEvents []string
	oldSig, oldRel := joinPeer("", "oldrel", true, nil)
	if err := oldRel.Volunteer(true); err != nil {
		t.Fatal(err)
	}
	legacyRoom := oldSig.Welcome.Room
	_, alice2 := joinPeer(legacyRoom, "alice2", true, func(id string) {
		watchMu.Lock()
		legacyEvents = append(legacyEvents, id)
		watchMu.Unlock()
	})
	alice2.SetTop(720)
	startIDRWriter(t, alice2)
	_, bob2 := joinPeer(legacyRoom, "bob2", true, nil)
	waitMesh(t, "alice2 and bob2 homed on oldrel", 20*time.Second, func() bool {
		return alice2.Home(alice2.Me()) == oldRel.Me() && bob2.Home(bob2.Me()) == oldRel.Me()
	})
	if err := bob2.SetMaxLayer(alice2.Me(), wireHigh); err != nil {
		t.Fatal(err)
	}
	waitMesh(t, "alice2 sends high to old-style relay", 20*time.Second, func() bool {
		return alice2.Layers()[oldRel.Me()] == layerHigh
	})
	time.Sleep(250 * time.Millisecond)
	watchMu.Lock()
	gotLegacy := slices.Clone(legacyEvents)
	watchMu.Unlock()
	if len(gotLegacy) != 0 {
		t.Fatalf("expected 0 chimes behind old-style relay without Watchers field, got %v", gotLegacy)
	}
}

// TestRelayReassertBypassesSentCapOnSharerResume verifies round-2 fix 1 on the
// rtc layer, including when the sharer does NOT send Resumed=true (older build)
// and only reports Height=0: the relay's Height=0 re-assert must clear sentCap
// and re-send wireHigh rather than being swallowed by sentCap deduplication.
func TestRelayReassertBypassesSentCapOnSharerResume(t *testing.T) {
	wsURL, _, _, _ := startCountingServer(t)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	cm := &proto.ClientMeta{AppVersion: "0.2.0", Build: room.DefaultMinRelayBuild}

	joinPeer := func(roomCode, name string, defOff bool, onVideo func(string, []byte)) (*sigclient.Client, *Mesh) {
		t.Helper()
		k := identity.Ephemeral()
		var sig *sigclient.Client
		var err error
		if roomCode == "" {
			sig, err = sigclient.Create(ctx, wsURL, proto.Create{Name: name, IDKey: k.Public(), Client: cm}, sigclient.Options{})
		} else {
			sig, err = sigclient.Join(ctx, wsURL, proto.Join{Room: roomCode, Name: name, IDKey: k.Public(), Client: cm}, sigclient.Options{})
		}
		if err != nil {
			t.Fatal(err)
		}
		m, err := New(sig, Options{Identity: k, DefaultOff: defOff, OnVideo: onVideo, Log: quiet})
		if err != nil {
			sig.Leave()
			t.Fatal(err)
		}
		t.Cleanup(m.Close)
		return sig, m
	}

	aliceSig, alice := joinPeer("", "alice", true, nil)
	alice.SetTop(720)
	startIDRWriter(t, alice)

	_, rel := joinPeer(aliceSig.Welcome.Room, "relay", true, nil)
	if err := rel.Volunteer(true); err != nil {
		t.Fatal(err)
	}
	var bobFrames atomic.Int32
	_, bob := joinPeer(aliceSig.Welcome.Room, "bob", true, func(id string, _ []byte) {
		if id == alice.Me() {
			bobFrames.Add(1)
		}
	})
	waitMesh(t, "alice and bob connected to relay and alice capped to off", 20*time.Second, func() bool {
		rel.mu.Lock()
		ra := rel.peers[alice.Me()]
		rb := rel.peers[bob.Me()]
		rel.mu.Unlock()
		return ra != nil && ra.connected.Load() &&
			rb != nil && rb.connected.Load() &&
			alice.Layers()[rel.Me()] == layerOff &&
			strings.HasPrefix(bob.Stats()[alice.Me()].Pair, "via ")
	})

	// Simulate the exact scenario from round-2 fix 1:
	// bob wants high -> relay demand & sentCap record wireHigh, while alice never
	// received that signal (stays at layerOff) and then reports Height=0 on resume.
	rel.mu.Lock()
	rAlice := rel.peers[alice.Me()]
	rel.demand[demandKey{to: alice.Me()}] = demandVal{wire: wireHigh, watchers: []string{bob.Me()}}
	rBob := rel.peers[bob.Me()]
	rel.mu.Unlock()
	if rAlice == nil || rBob == nil {
		t.Fatal("relay missing peer connections")
	}
	rBob.polMu.Lock()
	if rBob.originCap == nil {
		rBob.originCap = map[string]int{}
	}
	rBob.originCap[alice.Me()] = levelHigh
	rBob.polMu.Unlock()
	rAlice.polMu.Lock()
	rAlice.sentCap[""] = wireHigh
	rAlice.sentWatchers = map[string][]string{"": {bob.Me()}}
	rAlice.polMu.Unlock()

	// Alice re-sends Sending{Height: 0, Top: 720} on her existing connection (without Resumed=true).
	alice.mu.Lock()
	rRelOnAlice := alice.peers[rel.Me()]
	alice.mu.Unlock()
	alice.resendSending(rRelOnAlice)

	waitMesh(t, "relay re-asserts wireHigh past sentCap dedupe and bob receives frames", 20*time.Second, func() bool {
		return alice.Layers()[rel.Me()] == layerHigh && bobFrames.Load() >= 5
	})
}

// TestLogPairsReadsFailedConnectionBeforeClose verifies round-2 fix 2:
// getStats / logPairs still reads stats when ConnectionState is Failed (before
// closePC sets r.closing), and stops once closePC runs.
func TestLogPairsReadsFailedConnectionBeforeClose(t *testing.T) {
	var logBuf bytes.Buffer
	m := newTestMeshForUpload(t)
	m.opt.Log = slog.New(slog.NewTextHandler(&logBuf, nil))

	var se1 webrtc.SettingEngine
	se1.SetIncludeLoopbackCandidate(true)
	se1.SetICETimeouts(80*time.Millisecond, 300*time.Millisecond, 25*time.Millisecond)
	se1.SetICEMaxBindingRequests(2)
	me1 := &webrtc.MediaEngine{}
	if err := registerCodecs(me1); err != nil {
		t.Fatal(err)
	}
	api1 := webrtc.NewAPI(webrtc.WithMediaEngine(me1), webrtc.WithSettingEngine(se1))

	pc2Conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer pc2Conn.Close()
	var se2 webrtc.SettingEngine
	se2.SetIncludeLoopbackCandidate(true)
	se2.SetICEUDPMux(webrtc.NewICEUDPMux(nil, pc2Conn))
	me2 := &webrtc.MediaEngine{}
	if err := registerCodecs(me2); err != nil {
		t.Fatal(err)
	}
	api2 := webrtc.NewAPI(webrtc.WithMediaEngine(me2), webrtc.WithSettingEngine(se2))

	pc1, err := api1.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc1.Close()
	pc2, err := api2.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc2.Close()

	tr, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: clockRate}, "screen", "blite")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pc1.AddTrack(tr); err != nil {
		t.Fatal(err)
	}

	g1 := webrtc.GatheringCompletePromise(pc1)
	offer, err := pc1.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := pc1.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-g1
	if err := pc2.SetRemoteDescription(*pc1.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	g2 := webrtc.GatheringCompletePromise(pc2)
	ans, err := pc2.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := pc2.SetLocalDescription(ans); err != nil {
		t.Fatal(err)
	}
	<-g2
	if err := pc1.SetRemoteDescription(*pc2.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	waitMesh(t, "pc1 connects to pc2", 5*time.Second, func() bool {
		return pc1.ConnectionState() == webrtc.PeerConnectionStateConnected
	})

	r := &remote{id: "bob", pc: pc1, fwd: newForwarder(tr, layerHigh)}

	// Drop pc2's UDP socket without DTLS close_notify: pc1 first enters
	// Disconnected (where snapshotPairs captures candidate pairs), then Failed
	// (where Pion wipes ICE agent pairs, so logPairs uses the snapshot).
	_ = pc2Conn.Close()
	waitMesh(t, "pc1 reaches PeerConnectionStateDisconnected", 5*time.Second, func() bool {
		return pc1.ConnectionState() == webrtc.PeerConnectionStateDisconnected
	})
	m.snapshotPairs(r)

	waitMesh(t, "pc1 reaches PeerConnectionStateFailed", 5*time.Second, func() bool {
		return pc1.ConnectionState() == webrtc.PeerConnectionStateFailed
	})
	st, ok := m.getStats(r)
	if !ok || len(st) == 0 {
		t.Fatalf("getStats in PeerConnectionStateFailed = (%d entries, %v), want non-empty/true", len(st), ok)
	}
	logBuf.Reset()
	m.logPairs(r)
	if !strings.Contains(logBuf.String(), "ice pair") {
		t.Fatalf("expected logPairs in Failed state to log snapshotted pairs, got %q", logBuf.String())
	}

	// If no snapshot was ever recorded and Pion wiped pairs on Failed, logPairs
	// logs the explicit "no pairs: ICE failed" diagnostic instead of nothing.
	r.pairsMu.Lock()
	r.lastPairs = nil
	r.pairsMu.Unlock()
	logBuf.Reset()
	m.logPairs(r)
	if !strings.Contains(logBuf.String(), "no pairs: ICE failed") {
		t.Fatalf("expected fallback 'no pairs: ICE failed', got %q", logBuf.String())
	}

	m.closePC(r)
	if _, ok := m.getStats(r); ok {
		t.Fatal("getStats after closePC returned true, want false")
	}
}

// TestRehomeWatchGraceExpiryClearsStaleWatchingPeer verifies round-2 fix 3:
// if a relayed viewer stops watching inside the re-home grace window and no
// further event occurs, checkAllWatching runs when the grace ends and clears
// watchingPeer.
func TestRehomeWatchGraceExpiryClearsStaleWatchingPeer(t *testing.T) {
	m := newTestMeshForUpload(t)
	m.rehomeGrace = 120 * time.Millisecond
	m.members = map[string]bool{"r1": true, "r2": true, "carol": true}
	m.watchingPeer = map[string]bool{"carol": true}
	m.rehomingWatch = map[string]time.Time{}
	m.caps = map[string]int{}
	m.demand = map[demandKey]demandVal{}
	m.served = map[string]bool{}
	m.plan = proto.Plan{
		Epoch:  1,
		Relays: []string{"r1"},
		Home:   map[string]string{"": "r1", "carol": "r1"},
	}
	m.rend.updateCfg(func(c *shareCfg) { c.top = 720 })
	m.joinedAt = map[string]time.Time{"r1": time.Now(), "r2": time.Now(), "carol": time.Now()}

	r1 := addTestRemote(m, "r1", time.Now())
	r1.connected.Store(true)
	r1.watchers = []string{"carol"}

	r2 := addTestRemote(m, "r2", time.Now())
	r2.connected.Store(true)
	r2.watchers = []string{} // carol stopped watching inside the window

	// Before re-home, carol is actively reported by r1.
	m.checkAllWatching()
	m.mu.Lock()
	if !m.watchingPeer["carol"] {
		m.mu.Unlock()
		t.Fatal("expected carol to be active via r1 before re-home")
	}
	m.mu.Unlock()

	// Re-home from r1 onto r2: setPlan starts the re-home grace timer.
	m.setPlan(proto.Plan{
		Epoch:  2,
		Relays: []string{"r2"},
		Home:   map[string]string{"": "r2", "carol": "r2"},
	})

	// Immediately inside the grace window, watchingPeer[carol] is still held true.
	m.mu.Lock()
	heldDuringGrace := m.watchingPeer["carol"]
	m.mu.Unlock()
	if !heldDuringGrace {
		t.Fatal("expected watchingPeer[carol] to be held during re-home grace")
	}

	// Without any further events, once rehomeWatchGrace expires the scheduled
	// checkAllWatching clears watchingPeer[carol].
	waitMesh(t, "stale watchingPeer cleared after rehomeWatchGrace expires", 2*time.Second, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return !m.watchingPeer["carol"] && len(m.rehomingWatch) == 0
	})
}

// TestSignalingResumedResendsCapsDemandAndSlots exercises signalMsg{Resumed: true}
// end to end across real signaling drops and resumes:
//  1. Sharer resumes while at layerLow (Height > 0, so Height==0 re-assert does NOT
//     fire): relay receives Resumed=true and re-sends its updated wireHigh demand.
//  2. Relay resumes: relayed viewer receives Resumed=true and re-sends its cap.
//  3. Viewer resumes: relay receives Resumed=true and re-sends its slot map.
func TestSignalingResumedResendsCapsDemandAndSlots(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(server.Config{Log: quiet})

	var gateMu sync.Mutex
	var holdCh chan struct{}
	holdNextWS := func() (release func()) {
		ch := make(chan struct{})
		gateMu.Lock()
		holdCh = ch
		gateMu.Unlock()
		var once sync.Once
		return func() {
			gateMu.Lock()
			if holdCh == ch {
				holdCh = nil
			}
			gateMu.Unlock()
			once.Do(func() { close(ch) })
		}
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws" {
			gateMu.Lock()
			ch := holdCh
			gateMu.Unlock()
			if ch != nil {
				select {
				case <-ch:
				case <-r.Context().Done():
					return
				}
			}
		}
		srv.ServeHTTP(w, r)
	}))
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	ctx := context.Background()
	cm := &proto.ClientMeta{AppVersion: "0.2.0", Build: proto.MultiRelayBuild}

	type sigStateTracker struct {
		reconnecting atomic.Bool
	}
	joinPeer := func(roomCode, name string, defOff bool) (*sigclient.Client, *Mesh, *sigStateTracker) {
		t.Helper()
		k := identity.Ephemeral()
		st := &sigStateTracker{}
		opt := sigclient.Options{
			OnState: func(state string) {
				st.reconnecting.Store(state == "reconnecting")
			},
		}
		var sig *sigclient.Client
		var err error
		if roomCode == "" {
			sig, err = sigclient.Create(ctx, wsURL, proto.Create{Name: name, IDKey: k.Public(), Client: cm}, opt)
		} else {
			sig, err = sigclient.Join(ctx, wsURL, proto.Join{Room: roomCode, Name: name, IDKey: k.Public(), Client: cm}, opt)
		}
		if err != nil {
			t.Fatal(err)
		}
		m, err := New(sig, Options{Identity: k, DefaultOff: defOff, Build: proto.MultiRelayBuild, Log: quiet})
		if err != nil {
			sig.Leave()
			t.Fatal(err)
		}
		t.Cleanup(m.Close)
		return sig, m, st
	}

	aliceSig, alice, aliceST := joinPeer("", "alice", true)
	alice.SetTop(720)
	startIDRWriter(t, alice)

	relSig, rel, relST := joinPeer(aliceSig.Welcome.Room, "relay", true)
	if err := rel.Volunteer(true); err != nil {
		t.Fatal(err)
	}

	bobSig, bob, bobST := joinPeer(aliceSig.Welcome.Room, "bob", true)
	if err := bob.SetMaxLayer(alice.Me(), wireLow); err != nil {
		t.Fatal(err)
	}
	waitMesh(t, "alice sending layerLow to relay and bob receiving slot map", 20*time.Second, func() bool {
		return alice.Layers()[rel.Me()] == layerLow &&
			strings.HasPrefix(bob.Stats()[alice.Me()].Pair, "via ")
	})

	// 1. Sharer signaling drops while sending layerLow (Height=360 > 0, so the
	// Height==0 fallback does NOT apply); bob switches to wireHigh while alice
	// is offline (relay's signal to alice is dropped, sentCap records wireHigh).
	// On resume, alice sends Resumed=true -> relay clears sentCap/demand and
	// re-sends wireHigh demand -> alice climbs to layerHigh.
	relAlice := holdNextWS()
	defer relAlice()
	aliceSig.Drop()
	waitMesh(t, "alice enters signaling reconnecting", 5*time.Second, func() bool {
		return aliceST.reconnecting.Load()
	})
	if err := bob.SetMaxLayer(alice.Me(), wireHigh); err != nil {
		t.Fatal(err)
	}
	waitMesh(t, "relay records wireHigh demand while alice is offline", 5*time.Second, func() bool {
		rel.mu.Lock()
		defer rel.mu.Unlock()
		return rel.demand[demandKey{to: alice.Me()}].wire == wireHigh
	})
	relAlice()
	waitMesh(t, "alice resumes and relay re-sends wireHigh demand via Resumed=true", 8*time.Second, func() bool {
		return !aliceST.reconnecting.Load() && alice.Layers()[rel.Me()] == layerHigh
	})

	// 2. Relay signaling drops; while relay is offline, bob caps alice to wireLow
	// (bob's signal to relay is dropped, bob's sentCap records wireLow).
	// On resume, relay sends Resumed=true -> bob clears sentCap and resendCaps()
	// re-sends wireLow -> relay updates demand and caps alice to layerLow.
	relRelay := holdNextWS()
	defer relRelay()
	relSig.Drop()
	waitMesh(t, "relay enters signaling reconnecting", 5*time.Second, func() bool {
		return relST.reconnecting.Load()
	})
	if err := bob.SetMaxLayer(alice.Me(), wireLow); err != nil {
		t.Fatal(err)
	}
	waitMesh(t, "bob records sentCap=wireLow to offline relay", 5*time.Second, func() bool {
		bob.mu.Lock()
		rr := bob.peers[rel.Me()]
		bob.mu.Unlock()
		if rr == nil {
			return false
		}
		rr.polMu.Lock()
		defer rr.polMu.Unlock()
		return rr.sentCap[alice.Me()] == wireLow
	})
	relRelay()
	waitMesh(t, "relay resumes, bob re-sends wireLow cap, and alice steps to layerLow", 8*time.Second, func() bool {
		return !relST.reconnecting.Load() && alice.Layers()[rel.Me()] == layerLow
	})

	// 3. Viewer signaling drops and its rxSlots are lost; on resume, bob sends
	// Resumed=true -> relay calls sendSlots(rBob) -> bob's rxSlots are restored.
	relBob := holdNextWS()
	defer relBob()
	bobSig.Drop()
	waitMesh(t, "bob enters signaling reconnecting", 5*time.Second, func() bool {
		return bobST.reconnecting.Load()
	})
	bob.clearRx()
	if strings.HasPrefix(bob.Stats()[alice.Me()].Pair, "via ") {
		t.Fatal("expected bob's via-relay path cleared after clearRx")
	}
	relBob()
	waitMesh(t, "bob resumes and relay re-sends slots via Resumed=true", 8*time.Second, func() bool {
		return !bobST.reconnecting.Load() && strings.HasPrefix(bob.Stats()[alice.Me()].Pair, "via ")
	})
}
