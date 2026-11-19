package session

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"blite/internal/feeds"
	"blite/internal/media"
	"blite/internal/proto"
	"blite/internal/room"
	"blite/internal/server"
)

func TestSessionSoundEventsAndAutoWatch(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	ctx := context.Background()

	beforeLive := media.LiveSoundPlayers()

	alice, err := Create(ctx, Config{Server: ws, Name: "alice", View: media.ViewCount, Log: quiet}, "secret-pw")
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close()

	if !alice.PlaySounds() {
		t.Fatal("PlaySounds should default to true")
	}
	if got := alice.PlayedSounds(); len(got) != 0 {
		t.Fatalf("alice initial PlayedSounds = %v, want empty", got)
	}

	// 1. Own stream goes live -> SoundSelfLive + real GStreamer PCM buffers.
	if err := alice.SetSource(media.SourceTest); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice SoundSelfLive buffers", 5*time.Second, func() bool {
		return slices.Equal(alice.PlayedSounds(), []media.SoundKind{media.SoundSelfLive}) &&
			alice.SoundBuffers() >= 10
	})

	// Changing share options while already live does not replay SoundSelfLive.
	alice.soundGate.Reset()
	if err := alice.SetShareOptions(ShareOptions{FPS: 60, MinHeight: 360, SmallerCopy: true}); err != nil {
		t.Fatal(err)
	}
	if got := alice.PlayedSounds(); len(got) != 0 {
		t.Fatalf("PlayedSounds after SetShareOptions while live = %v, want empty", got)
	}

	// 2. Bob joins with AutoWatch off:
	// - Alice hears SoundPeerJoined, NOT SoundViewerWatching (Bob isn't watching yet).
	// - Bob does NOT hear SoundPeerJoined for Alice (who was already in Welcome.Peers).
	// - Bob DOES hear SoundPeerSharing when Alice's share is announced, even with AutoWatch off.
	autoOff := false
	bob, err := Join(ctx, Config{Server: ws, Name: "bob", View: media.ViewCount, AutoWatch: &autoOff, Log: quiet}, alice.sig.Welcome.Room, alice.pw)
	if err != nil {
		t.Fatal(err)
	}
	defer bob.Close()

	waitFor(t, "bob connected and sees alice sharing (unwatched)", 10*time.Second, func() bool {
		mb := member(alice, "bob")
		ma := member(bob, "alice")
		return mb.State == "connected" && mb.Layer == "off" && ma.Sharing && ma.Eye == feeds.EyeClosed
	})

	if got := alice.PlayedSounds(); !slices.Equal(got, []media.SoundKind{media.SoundPeerJoined}) {
		t.Fatalf("alice PlayedSounds after unwatched bob joined = %v, want [peer-joined]", got)
	}
	waitFor(t, "bob hears SoundPeerSharing with AutoWatch off", 5*time.Second, func() bool {
		return slices.Equal(bob.PlayedSounds(), []media.SoundKind{media.SoundPeerSharing}) &&
			bob.SoundBuffers() >= 10
	})

	// 3. Bob clicks the eye to watch Alice -> Alice hears SoundViewerWatching.
	aliceID := member(bob, "alice").ID
	if err := bob.ToggleWatch(aliceID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice hears SoundViewerWatching via eye", 10*time.Second, func() bool {
		return slices.Equal(alice.PlayedSounds(), []media.SoundKind{media.SoundPeerJoined, media.SoundViewerWatching}) &&
			member(bob, "alice").Frames > 5
	})

	// Stopping watching plays NO sound on either peer.
	if err := bob.ToggleWatch(aliceID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice stops sending to bob", 5*time.Second, func() bool {
		return member(alice, "bob").Layer == "off"
	})
	if got := alice.PlayedSounds(); !slices.Equal(got, []media.SoundKind{media.SoundPeerJoined, media.SoundViewerWatching}) {
		t.Fatalf("alice PlayedSounds after stop-watching = %v", got)
	}
	if got := bob.PlayedSounds(); !slices.Equal(got, []media.SoundKind{media.SoundPeerSharing}) {
		t.Fatalf("bob PlayedSounds after stop-watching = %v", got)
	}

	// 4. Bob starts sharing while Alice has AutoWatch on:
	// - Bob hears SoundSelfLive and SoundViewerWatching (Alice watches automatically).
	// - Alice hears SoundPeerSharing.
	bob.soundGate.Reset()
	alice.soundGate.Reset()
	if err := bob.SetSource(media.SourceTest); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice auto-watches bob and both hear expected chimes", 10*time.Second, func() bool {
		return member(alice, "bob").Frames > 5 &&
			slices.Equal(alice.PlayedSounds(), []media.SoundKind{media.SoundPeerSharing}) &&
			slices.Equal(bob.PlayedSounds(), []media.SoundKind{media.SoundSelfLive, media.SoundViewerWatching})
	})

	// 5. Stopping sharing and leaving play NO sounds.
	alice.soundGate.Reset()
	bob.soundGate.Reset()
	if err := bob.SetSource("none"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice sees bob stopped sharing", 5*time.Second, func() bool {
		return !member(alice, "bob").Sharing
	})
	bob.Close()
	waitFor(t, "bob gone from alice", 5*time.Second, func() bool {
		return len(alice.State().Members) == 0
	})
	if got := alice.PlayedSounds(); len(got) != 0 {
		t.Fatalf("alice PlayedSounds on peer stop-sharing + leave = %v, want empty", got)
	}

	if err := alice.SetSource("none"); err != nil {
		t.Fatal(err)
	}
	if got := alice.PlayedSounds(); len(got) != 0 {
		t.Fatalf("alice PlayedSounds on own stop-sharing = %v, want empty", got)
	}
	alice.Close()
	if n := media.LiveSoundPlayers() - beforeLive; n != 0 {
		t.Fatalf("LiveSoundPlayers after Close = %d, want 0", n)
	}
}

func TestSessionSoundBurstDebounceAndPlaySoundsToggle(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(server.New(server.Config{Log: quiet}))
	defer ts.Close()
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	ctx := context.Background()

	beforeLive := media.LiveSoundPlayers()

	// Open room (empty password avoids Argon2 per join under -race) plus a
	// wider test debounce window so heavy -race CPU contention never splits the
	// two back-to-back joins across the window (exact 2s SoundDebounce math is
	// unit-tested deterministically in internal/media.TestSoundGateDebounce).
	alice, err := Create(ctx, Config{Server: ws, Name: "alice", View: media.ViewCount, Log: quiet}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close()
	alice.soundGate.SetDebounce(10 * time.Second)

	// Burst of 2 peers joining within the debounce window plays SoundPeerJoined once.
	carol, err := Join(ctx, Config{Server: ws, Name: "carol", View: media.ViewCount, Log: quiet}, alice.sig.Welcome.Room, alice.pw)
	if err != nil {
		t.Fatal(err)
	}
	defer carol.Close()
	dave, err := Join(ctx, Config{Server: ws, Name: "dave", View: media.ViewCount, Log: quiet}, alice.sig.Welcome.Room, alice.pw)
	if err != nil {
		t.Fatal(err)
	}
	defer dave.Close()

	waitFor(t, "both carol and dave connected", 30*time.Second, func() bool {
		return member(alice, "carol").State == "connected" && member(alice, "dave").State == "connected"
	})
	if got := alice.PlayedSounds(); !slices.Equal(got, []media.SoundKind{media.SoundPeerJoined}) {
		t.Fatalf("alice burst join PlayedSounds = %v, want single [peer-joined]", got)
	}
	waitFor(t, "alice first chime buffers", 5*time.Second, func() bool {
		return alice.SoundBuffers() >= 10
	})
	bufsBefore := alice.SoundBuffers()

	// SetOutput stops the current SoundPlayer and recreates on the next chime,
	// preserving cumulative buffer delivery.
	alice.SetOutput("alt-device")
	if err := alice.SetSource(media.SourceTest); err != nil {
		t.Fatal(err)
	}
	// Both carol and dave start watching -> SoundViewerWatching plays once!
	waitFor(t, "alice SoundSelfLive + coalesced SoundViewerWatching after SetOutput", 20*time.Second, func() bool {
		return slices.Equal(alice.PlayedSounds(), []media.SoundKind{
			media.SoundPeerJoined,
			media.SoundSelfLive,
			media.SoundViewerWatching,
		}) && alice.SoundBuffers() >= bufsBefore+20
	})

	// Leaving plays no sound; closing carol and dave before eve joins also
	// avoids 4-peer x264+avdec CPU contention under -race.
	alice.soundGate.Reset()
	dave.Close()
	carol.Close()
	waitFor(t, "carol and dave gone from alice", 5*time.Second, func() bool {
		return len(alice.State().Members) == 0
	})
	if got := alice.PlayedSounds(); len(got) != 0 {
		t.Fatalf("alice PlayedSounds on carol/dave leave = %v, want empty", got)
	}

	// Turning PlaySounds off stops the SoundPlayer and silences all four chimes.
	alice.SetPlaySounds(false)
	if alice.PlaySounds() {
		t.Fatal("PlaySounds should be false after SetPlaySounds(false)")
	}
	alice.soundGate.Reset()

	off := false
	eve, err := Join(ctx, Config{Server: ws, Name: "eve", View: media.ViewCount, PlaySounds: &off, Log: quiet}, alice.sig.Welcome.Room, alice.pw)
	if err != nil {
		t.Fatal(err)
	}
	defer eve.Close()
	if eve.PlaySounds() {
		t.Fatal("eve PlaySounds should be false from Config.PlaySounds")
	}
	if err := eve.SetSource(media.SourceTest); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "eve connected and streaming to alice", 30*time.Second, func() bool {
		return member(alice, "eve").Frames > 5 && member(eve, "alice").Frames > 5
	})
	if got := alice.PlayedSounds(); len(got) != 0 {
		t.Fatalf("alice PlayedSounds while muted = %v, want empty", got)
	}
	if got := eve.PlayedSounds(); len(got) != 0 {
		t.Fatalf("eve PlayedSounds with PlaySounds=false = %v, want empty", got)
	}
	if eve.SoundBuffers() != 0 {
		t.Fatalf("eve SoundBuffers = %d, want 0", eve.SoundBuffers())
	}

	eve.Close()
	dave.Close()
	carol.Close()
	alice.Close()
	if n := media.LiveSoundPlayers() - beforeLive; n != 0 {
		t.Fatalf("LiveSoundPlayers after all closed = %d, want 0", n)
	}
}

// AutoWatch off: reproduces the bug where the sharer's cached cap holds "off"
// while the viewer now wants "high" when a brand-new PeerConnection starts or
// when signaling resumes, and verifies relay peers are not misreported as
// watching viewers.
func TestAutoWatchOffSharerReconnectAndResume(t *testing.T) {
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
		return func() { once.Do(func() { close(ch) }) }
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
	ws := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	ctx := context.Background()
	cm := &proto.ClientMeta{AppVersion: "0.2.0", Build: room.DefaultMinRelayBuild}

	autoOff := false
	p1, err := Create(ctx, Config{Server: ws, Name: "p1", View: media.ViewCount, AutoWatch: &autoOff, Client: cm, Log: quiet}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer p1.Close()

	p2, err := Join(ctx, Config{Server: ws, Name: "p2", View: media.ViewCount, AutoWatch: &autoOff, Client: cm, Log: quiet}, p1.sig.Welcome.Room, p1.pw)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()

	// Assign the lower peerID as sharer (alice) and higher as viewer (bob) so
	// reconcile offers from alice immediately upon resume without retries.
	alice, bob := p1, p2
	aliceName, bobName := "p1", "p2"
	roomCode, roomPW := p1.sig.Welcome.Room, p1.pw
	if alice.sig.Welcome.PeerID > bob.sig.Welcome.PeerID {
		alice, bob = p2, p1
		aliceName, bobName = "p2", "p1"
	}
	if err := alice.SetSource(media.SourceTest); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "bob connected and alice capped to off", 10*time.Second, func() bool {
		mb := member(alice, bobName)
		ma := member(bob, aliceName)
		return mb.State == "connected" && mb.Layer == "off" && ma.Sharing && ma.Eye == feeds.EyeClosed
	})

	// 1. Move onto relay carol while bob is STILL unwatched so alice's last
	// direct cap from bob remains "off".
	carol, err := Join(ctx, Config{Server: ws, Name: "carol", View: media.ViewCount, AutoWatch: &autoOff, Client: cm, Log: quiet}, roomCode, roomPW)
	if err != nil {
		t.Fatal(err)
	}
	defer carol.Close()
	if err := carol.Volunteer(true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice-bob direct PC pruned onto relay carol while bob unwatched", 15*time.Second, func() bool {
		return strings.HasPrefix(member(bob, aliceName).Pair, "via ") &&
			strings.HasPrefix(member(alice, bobName).Pair, "via ")
	})

	// Hold alice's signaling reconnect while bob turns the eye ON ("high") and
	// carol leaves (returning the room to direct mesh). When alice resumes, a
	// brand-new direct PeerConnection starts between alice and bob while alice
	// never saw bob's earlier MaxLayer=0 and bob now wants "high".
	releaseAlice := holdNextWS()
	defer releaseAlice()
	alice.sig.Drop()
	waitFor(t, "alice enters signaling reconnecting", 5*time.Second, func() bool {
		return alice.State().Reconnecting
	})

	aliceID := member(bob, aliceName).ID
	if err := bob.ToggleWatch(aliceID); err != nil {
		t.Fatal(err)
	}
	if err := carol.Volunteer(false); err != nil {
		t.Fatal(err)
	}
	carol.Close()
	waitFor(t, "bob sees relay cleared while alice is offline", 5*time.Second, func() bool {
		return len(bob.State().Relay) == 0 && member(bob, aliceName).Eye == feeds.EyeOpen
	})
	time.Sleep(100 * time.Millisecond)

	gateMu.Lock()
	holdCh = nil
	gateMu.Unlock()
	releaseAlice()

	waitFor(t, "alice and bob connect on brand-new direct PC", 15*time.Second, func() bool {
		ma := member(bob, aliceName)
		return !alice.State().Reconnecting && ma.State == "connected" && strings.Contains(ma.Pair, "/")
	})
	beforeNewConn := member(bob, aliceName).Frames
	waitFor(t, "video comes back on new connection without re-toggling", 8*time.Second, func() bool {
		return member(bob, aliceName).Frames > beforeNewConn+15
	})

	// 2. Signaling resume on an existing connection: bob unwatches (alice capped
	// to "off"), alice's signaling drops, bob turns the eye ON while alice is
	// offline, and alice resumes without replacing the PeerConnection.
	if err := bob.ToggleWatch(aliceID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice capped to off again", 5*time.Second, func() bool {
		return member(alice, bobName).Layer == "off"
	})
	releaseAlice2 := holdNextWS()
	defer releaseAlice2()
	alice.sig.Drop()
	waitFor(t, "alice enters second signaling reconnecting", 5*time.Second, func() bool {
		return alice.State().Reconnecting
	})
	if err := bob.ToggleWatch(aliceID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	gateMu.Lock()
	holdCh = nil
	gateMu.Unlock()
	releaseAlice2()

	beforeResume := member(bob, aliceName).Frames
	waitFor(t, "video comes back after signaling resume without re-toggling", 8*time.Second, func() bool {
		return !alice.State().Reconnecting && member(bob, aliceName).Frames > beforeResume+15
	})

	// 3. Relay watcher forwarding (option 2): relay dave itself (AutoWatch off)
	// does not trigger SoundViewerWatching, while bob watching behind dave
	// chimes once and switching back to direct mesh does not chime a second time.
	dave, err := Join(ctx, Config{Server: ws, Name: "dave", View: media.ViewCount, AutoWatch: &autoOff, Client: cm, Log: quiet}, roomCode, roomPW)
	if err != nil {
		t.Fatal(err)
	}
	defer dave.Close()
	alice.soundGate.Reset()
	if err := dave.Volunteer(true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice-bob direct PC pruned onto relay dave", 15*time.Second, func() bool {
		return strings.HasPrefix(member(bob, aliceName).Pair, "via ") &&
			strings.HasPrefix(member(alice, bobName).Pair, "via ")
	})
	framesGrow(t, "bob keeps receiving alice via relay dave", bob, aliceName)
	if got := alice.PlayedSounds(); len(got) != 0 {
		t.Fatalf("unexpected chime when already-watching bob moved onto relay dave: %v", got)
	}

	if err := bob.ToggleWatch(aliceID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice stops sending to relay dave when bob unwatches", 10*time.Second, func() bool {
		return member(alice, "dave").Layer == "off"
	})

	// Relay-path signaling resume (round 2 fix 1): alice's signaling drops while
	// capped to "off", bob turns the eye ON behind dave (dave sends "high" which
	// the server drops while dave's sentCap records "high"), and alice resumes on
	// its existing connection to dave -> video and chime must still come back.
	alice.soundGate.Reset()
	releaseAlice3 := holdNextWS()
	defer releaseAlice3()
	alice.sig.Drop()
	waitFor(t, "alice enters third signaling reconnecting (relay path)", 5*time.Second, func() bool {
		return alice.State().Reconnecting
	})
	if err := bob.ToggleWatch(aliceID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	gateMu.Lock()
	holdCh = nil
	gateMu.Unlock()
	releaseAlice3()

	beforeRelayResume := member(bob, aliceName).Frames
	waitFor(t, "bob receives alice via relay dave after sharer signaling resume", 8*time.Second, func() bool {
		return !alice.State().Reconnecting && member(bob, aliceName).Frames > beforeRelayResume+15
	})
	waitFor(t, "alice hears SoundViewerWatching once for bob behind relay dave", 5*time.Second, func() bool {
		return slices.Equal(alice.PlayedSounds(), []media.SoundKind{media.SoundViewerWatching})
	})

	alice.soundGate.Reset()
	if err := dave.Volunteer(false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alice and bob back to direct mesh without second chime", 15*time.Second, func() bool {
		ma := member(bob, aliceName)
		return ma.State == "connected" && strings.Contains(ma.Pair, "/")
	})
	framesGrow(t, "bob continues receiving alice directly", bob, aliceName)
	if got := alice.PlayedSounds(); len(got) != 0 {
		t.Fatalf("unexpected second chime when bob moved off relay dave: %v", got)
	}
}
