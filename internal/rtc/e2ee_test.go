package rtc

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"blite/internal/identity"
	"blite/internal/proto"
	"blite/internal/server"
	"blite/internal/sigclient"
)

// Resumed with a departed member (including a member we only reached through a
// relay and never had in m.peers) drops that member's exchange and sender keys,
// fires OnPeer("gone"), and rotates our own sender key so the departed member
// can't decrypt subsequent frames.
func TestResumedMemberGoneRekeys(t *testing.T) {
	origDelay, origGrace := keySwitchDelay, keyGrace
	keySwitchDelay = 40 * time.Millisecond
	keyGrace = 40 * time.Millisecond

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(server.Config{Log: quiet})
	upTS := httptest.NewServer(srv)
	defer upTS.Close()
	upWS := "ws" + strings.TrimPrefix(upTS.URL, "http") + "/ws"

	var gateMu sync.Mutex
	var holdResume chan struct{} // when non-nil, holds resume handshakes until closed

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
				var env proto.Envelope
				if json.Unmarshal(b, &env) == nil && env.T == proto.TResume {
					gateMu.Lock()
					ch := holdResume
					gateMu.Unlock()
					if ch != nil {
						select {
						case <-ch:
						case <-ctx.Done():
							return
						}
					}
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
			if down.Write(ctx, typ, b) != nil {
				return
			}
		}
	}))
	defer proxyTS.Close()
	proxyWS := "ws" + strings.TrimPrefix(proxyTS.URL, "http") + "/ws"

	ctx := context.Background()
	cm := &proto.ClientMeta{AppVersion: "0.1.0", Build: proto.E2EEBuild}

	var goneMu sync.Mutex
	gonePeers := map[string]bool{}

	ak := identity.Ephemeral()
	asig, err := sigclient.Create(ctx, proxyWS, proto.Create{Name: "alice", IDKey: ak.Public(), Client: cm, E2EE: true}, sigclient.Options{Renew: -1})
	if err != nil {
		t.Fatal(err)
	}
	alice, err := New(asig, Options{
		Identity: ak,
		Build:    proto.E2EEBuild,
		Log:      quiet,
		OnPeer: func(id, _, state string) {
			if state == "gone" {
				goneMu.Lock()
				gonePeers[id] = true
				goneMu.Unlock()
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		alice.Close()
		keySwitchDelay, keyGrace = origDelay, origGrace
	}()

	// Carol joins and volunteers as the room's relay.
	ck := identity.Ephemeral()
	csig, err := sigclient.Join(ctx, upWS, proto.Join{Room: asig.Welcome.Room, Name: "carol", IDKey: ck.Public(), Client: cm}, sigclient.Options{Renew: -1})
	if err != nil {
		t.Fatal(err)
	}
	carol, err := New(csig, Options{Identity: ck, Build: proto.E2EEBuild, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer carol.Close()
	if err := carol.Volunteer(true); err != nil {
		t.Fatal(err)
	}
	waitMesh(t, "carol active as relay on alice", 10*time.Second, func() bool {
		return slices.Equal(alice.Relays(), []string{csig.Welcome.PeerID})
	})

	// Dave joins while relay mode is active: Dave connects only to Carol, so
	// Alice never has Dave in m.peers (only in m.members), yet exchanges E2EE keys.
	dk := identity.Ephemeral()
	dsig, err := sigclient.Join(ctx, upWS, proto.Join{Room: asig.Welcome.Room, Name: "dave", IDKey: dk.Public(), Client: cm}, sigclient.Options{Renew: -1})
	if err != nil {
		t.Fatal(err)
	}
	dave, err := New(dsig, Options{Identity: dk, Build: proto.E2EEBuild, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	daveID := dsig.Welcome.PeerID

	waitMesh(t, "alice and dave exchange E2EE keys", 10*time.Second, func() bool {
		alice.crypt.mu.Lock()
		_, aHasKX := alice.crypt.peerKX[daveID]
		aHasOwner := false
		for _, o := range alice.crypt.owner {
			if o == daveID {
				aHasOwner = true
			}
		}
		alice.crypt.mu.Unlock()
		return aHasKX && aHasOwner && dave.crypt.ring.Has(alice.crypt.sender().KID)
	})
	alice.mu.Lock()
	_, daveDirect := alice.peers[daveID]
	alice.mu.Unlock()
	if daveDirect {
		t.Fatal("expected dave to be relayed-only (not in alice.peers)")
	}

	daveSample := dave.encAudio([]byte{1, 2, 3, 4})
	if _, ok := alice.decAudio(daveSample); !ok {
		t.Fatal("alice should decrypt dave's audio before dave leaves")
	}
	oldAliceKID := alice.crypt.sender().KID

	// Hold Alice's resume while Dave leaves, so Alice only learns Dave left via ev.Resumed.
	gate := make(chan struct{})
	gateMu.Lock()
	holdResume = gate
	gateMu.Unlock()

	asig.Drop()
	dave.Close()
	waitMesh(t, "carol sees dave leave", 5*time.Second, func() bool {
		return !slices.Contains(carol.Members(), daveID)
	})
	close(gate)

	// Wait for Alice to process ev.Resumed, drop Dave's keys, and rotate her sender key.
	waitMesh(t, "alice rotates sender key and drops dave on resume", 10*time.Second, func() bool {
		return alice.crypt.sender().KID != oldAliceKID
	})
	waitMesh(t, "dave's retired sender key expires", 2*time.Second, func() bool {
		_, ok := alice.decAudio(daveSample)
		return !ok
	})

	alice.crypt.mu.Lock()
	_, stillHasKX := alice.crypt.peerKX[daveID]
	stillHasOwner := false
	for _, o := range alice.crypt.owner {
		if o == daveID {
			stillHasOwner = true
		}
	}
	alice.crypt.mu.Unlock()
	if stillHasKX || stillHasOwner {
		t.Fatalf("dave's E2EE state kept after resume: peerKX=%v owner=%v", stillHasKX, stillHasOwner)
	}
	goneMu.Lock()
	sawGone := gonePeers[daveID]
	goneMu.Unlock()
	if !sawGone {
		t.Fatal("OnPeer(dave, gone) not fired for relayed member gone on resume")
	}
	// Dave never received Alice's rotated sender key, while Carol did.
	newSample := alice.encAudio([]byte{9, 8, 7})
	if _, ok := dave.decAudio(newSample); ok {
		t.Fatal("departed dave decrypted frame sealed with alice's post-resume key")
	}
	waitMesh(t, "carol receives alice's rotated key", 5*time.Second, func() bool {
		_, ok := carol.decAudio(alice.encAudio([]byte{9, 8, 7}))
		return ok
	})
}

// Resumed with a present member whose identity key changed drops its
// PeerConnection and exchange/sender keys, rotates our sender key without
// sealing to the old exchange key, and raises OnKeyChanged.
func TestResumedChangedIDKeyDropsPeerAndRekeys(t *testing.T) {
	origDelay, origGrace := keySwitchDelay, keyGrace
	keySwitchDelay = 40 * time.Millisecond
	keyGrace = 40 * time.Millisecond

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(server.Config{Log: quiet})
	upTS := httptest.NewServer(srv)
	defer upTS.Close()
	upWS := "ws" + strings.TrimPrefix(upTS.URL, "http") + "/ws"

	var swapIDKey atomic.Value // string: when non-empty, replaces bob's IDKey in Welcome

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
			if fakeKey, _ := swapIDKey.Load().(string); fakeKey != "" {
				var env proto.Envelope
				if json.Unmarshal(b, &env) == nil && env.T == proto.TWelcome {
					var wl proto.Welcome
					if json.Unmarshal(b, &wl) == nil {
						for i := range wl.Peers {
							if wl.Peers[i].Name == "bob" {
								wl.Peers[i].IDKey = fakeKey
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

	var kcMu sync.Mutex
	var keyChangedPeer, keyChangedName string

	ak := identity.Ephemeral()
	asig, err := sigclient.Create(ctx, proxyWS, proto.Create{Name: "alice", IDKey: ak.Public(), Client: cm, E2EE: true}, sigclient.Options{Renew: -1})
	if err != nil {
		t.Fatal(err)
	}
	alice, err := New(asig, Options{
		Identity: ak,
		Build:    proto.E2EEBuild,
		Log:      quiet,
		OnKeyChanged: func(id, name string) {
			kcMu.Lock()
			keyChangedPeer, keyChangedName = id, name
			kcMu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		alice.Close()
		keySwitchDelay, keyGrace = origDelay, origGrace
	}()

	bk := identity.Ephemeral()
	bsig, err := sigclient.Join(ctx, upWS, proto.Join{Room: asig.Welcome.Room, Name: "bob", IDKey: bk.Public(), Client: cm}, sigclient.Options{Renew: -1})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := New(bsig, Options{Identity: bk, Build: proto.E2EEBuild, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer bob.Close()
	bobID := bsig.Welcome.PeerID

	waitMesh(t, "alice-bob connected with E2EE keys", 10*time.Second, func() bool {
		alice.mu.Lock()
		r := alice.peers[bobID]
		alice.mu.Unlock()
		alice.crypt.mu.Lock()
		_, hasKX := alice.crypt.peerKX[bobID]
		alice.crypt.mu.Unlock()
		return r != nil && r.connected.Load() && hasKX && bob.crypt.ring.Has(alice.crypt.sender().KID)
	})
	alice.mu.Lock()
	oldRemote := alice.peers[bobID]
	alice.mu.Unlock()

	bobSample := bob.encAudio([]byte{5, 6, 7, 8})
	if _, ok := alice.decAudio(bobSample); !ok {
		t.Fatal("alice should decrypt bob before resume")
	}
	oldAliceKID := alice.crypt.sender().KID

	// Swap Bob's identity key on Alice's next resume Welcome.
	newBobKey := identity.Ephemeral().Public()
	swapIDKey.Store(newBobKey)
	asig.Drop()

	waitMesh(t, "alice rotates key and fires OnKeyChanged", 10*time.Second, func() bool {
		kcMu.Lock()
		got := keyChangedPeer
		kcMu.Unlock()
		return got == bobID && alice.crypt.sender().KID != oldAliceKID
	})

	kcMu.Lock()
	gotID, gotName := keyChangedPeer, keyChangedName
	kcMu.Unlock()
	if gotID != bobID || gotName != "bob" {
		t.Fatalf("OnKeyChanged = (%q, %q), want (%q, \"bob\")", gotID, gotName, bobID)
	}
	if alice.IDKey(bobID) != newBobKey {
		t.Fatalf("IDKey(bob) = %q, want %q", alice.IDKey(bobID), newBobKey)
	}
	alice.mu.Lock()
	curRemote := alice.peers[bobID]
	alice.mu.Unlock()
	if curRemote != nil || !oldRemote.closing.Load() {
		t.Fatalf("bob's connection was not dropped: peers[bob]=%v closing=%v", curRemote, oldRemote.closing.Load())
	}
	alice.crypt.mu.Lock()
	_, hasKX := alice.crypt.peerKX[bobID]
	alice.crypt.mu.Unlock()
	if hasKX {
		t.Fatal("bob's old verified exchange key remained in peerKX")
	}
	if _, ok := alice.decAudio(bobSample); ok {
		t.Fatal("bob's old sender key was not retired immediately on key change")
	}
	if _, ok := bob.decAudio(alice.encAudio([]byte{1, 1, 1})); ok {
		t.Fatal("bob received alice's rotated sender key despite unverified new identity key")
	}
}
