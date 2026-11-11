package room

import (
	"slices"
	"sync"
	"testing"
	"time"

	"blite/internal/proto"
)

type box struct {
	mu   sync.Mutex
	msgs []any
}

func (b *box) Send(m any) bool { b.mu.Lock(); b.msgs = append(b.msgs, m); b.mu.Unlock(); return true }
func (b *box) all() []any      { b.mu.Lock(); defer b.mu.Unlock(); return append([]any(nil), b.msgs...) }

const pw = "correct horse"

func TestCodeRoundTrip(t *testing.T) {
	for range 100 {
		c := NewCode()
		if got := NormalizeCode(Display(c)); got != c {
			t.Fatalf("NormalizeCode(Display(%q)) = %q", c, got)
		}
	}
	cases := map[string]string{
		"7k3q-x9md": "7K3QX9MD",
		"7K3Q X9MD": "7K3QX9MD",
		"OOOOIIII":  "00001111",
		"LLLL0000":  "11110000",
		"7K3QX9MU":  "", // U is not Crockford
		"7K3QX9M":   "", // too short
		"7K3QX9MDD": "", // too long
		"7K3QX9Mé":  "",
	}
	for in, want := range cases {
		if got := NormalizeCode(in); got != want {
			t.Errorf("NormalizeCode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCreateJoinErrors(t *testing.T) {
	m := NewManager(time.Minute)
	if _, err := m.Create("abc", "a", "", "", 0, false, &box{}); err != ErrBadRequest {
		t.Fatalf("short password: %v", err)
	}
	if _, err := m.Create(pw, "", "", "", 0, false, &box{}); err != ErrBadRequest {
		t.Fatalf("empty name: %v", err)
	}
	s, err := m.Create(pw, "alice", "k1", "", 0, false, &box{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Join("ZZZZZZZZ", pw, "bob", "", "", 0, &box{}); err != ErrNoRoom {
		t.Fatalf("no room: %v", err)
	}
	if _, err := m.Join(s.Room, "wrong password", "bob", "", "", 0, &box{}); err != ErrBadPassword {
		t.Fatalf("bad password: %v", err)
	}
	// No cap by default; the operator's cap counts everyone.
	for i := 1; i < 12; i++ {
		if _, err := m.Join(s.Room, pw, "p", "", "", 0, &box{}); err != nil {
			t.Fatalf("join %d: %v", i, err)
		}
	}
	m.MaxPeers = 12
	if _, err := m.Join(s.Room, pw, "thirteenth", "", "", 0, &box{}); err != ErrRoomFull {
		t.Fatalf("full: %v", err)
	}
}

// lastTopology is the newest Topology b received.
func lastTopology(b *box) (proto.Topology, bool) {
	msgs := b.all()
	for i := len(msgs) - 1; i >= 0; i-- {
		if t, ok := msgs[i].(proto.Topology); ok {
			return t, true
		}
	}
	return proto.Topology{}, false
}

// Above proto.RelayMaxPeers relay mode pauses: members are told an empty
// queue (mesh) with the volunteers waiting, and it resumes when the room
// shrinks back.
func TestRelayPausesInBigRooms(t *testing.T) {
	m := NewManager(time.Minute)
	m.MinRelayBuild = 0
	a := &box{}
	sa, _ := m.Create(pw, "alice", "ka", "", 0, false, a)
	var last Seat
	for i := 1; i < proto.RelayMaxPeers; i++ {
		last, _ = m.Join(sa.Room, pw, "p", "", "", 0, &box{})
	}
	if err := m.Volunteer(sa.Room, sa.PeerID, true); err != nil {
		t.Fatal(err)
	}
	if tp, _ := lastTopology(a); tp.Relay() != sa.PeerID || tp.Paused {
		t.Fatalf("relay at %d members: %+v", proto.RelayMaxPeers, tp)
	}
	big, err := m.Join(sa.Room, pw, "sixth", "", "", 0, &box{})
	if err != nil {
		t.Fatal(err)
	}
	if tp, _ := lastTopology(a); tp.Relay() != "" || !tp.Paused || len(tp.Waiting) != 1 || tp.Waiting[0] != sa.PeerID {
		t.Fatalf("paused: %+v", tp)
	}
	if big.RelayQueue != nil || !big.RelayPaused || len(big.RelayWaiting) != 1 {
		t.Fatalf("joiner's seat: %+v", big)
	}
	m.Leave(sa.Room, last.PeerID)
	if tp, _ := lastTopology(a); tp.Relay() != sa.PeerID || tp.Paused {
		t.Fatalf("resumed: %+v", tp)
	}
}

func TestJoinAnnouncesAndListsPeers(t *testing.T) {
	m := NewManager(time.Minute)
	a := &box{}
	sa, _ := m.Create(pw, "alice", "ka", "", 0, false, a)
	sb, err := m.Join(sa.Room, pw, "bob", "kb", "", 0, &box{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sb.Others) != 1 || sb.Others[0].PeerID != sa.PeerID || sb.Others[0].IDKey != "ka" {
		t.Fatalf("bob sees %+v", sb.Others)
	}
	msgs := a.all()
	if len(msgs) != 1 {
		t.Fatalf("alice got %d msgs", len(msgs))
	}
	if pj, ok := msgs[0].(proto.PeerJoined); !ok || pj.PeerID != sb.PeerID || pj.Name != "bob" {
		t.Fatalf("alice got %+v", msgs[0])
	}
}

func TestRelayIsolation(t *testing.T) {
	m := NewManager(time.Minute)
	a, b, x := &box{}, &box{}, &box{}
	sa, _ := m.Create(pw, "alice", "", "", 0, false, a)
	sb, _ := m.Join(sa.Room, pw, "bob", "", "", 0, b)
	sx, _ := m.Create(pw, "mallory", "", "", 0, false, x)

	if !m.Relay(sa.Room, sa.PeerID, sb.PeerID, "hi") {
		t.Fatal("same-room relay failed")
	}
	// Mallory, in another room, names bob's peer ID: must not reach him,
	// whether she claims her own room or bob's.
	if m.Relay(sx.Room, sx.PeerID, sb.PeerID, "evil") || m.Relay(sa.Room, sx.PeerID, sb.PeerID, "evil") {
		t.Fatal("cross-room relay succeeded")
	}
	for _, msg := range b.all() {
		if msg == "evil" {
			t.Fatal("bob received cross-room message")
		}
	}
}

func TestEmptyRoomGrace(t *testing.T) {
	m := NewManager(time.Minute)
	now := time.Unix(1_000_000, 0)
	m.now = func() time.Time { return now }
	sa, _ := m.Create(pw, "alice", "", "", 0, false, &box{})
	sb, _ := m.Join(sa.Room, pw, "bob", "", "", 0, &box{})
	m.Leave(sa.Room, sa.PeerID)
	m.Leave(sb.Room, sb.PeerID)

	// Empty but inside the grace period: the link still works.
	now = now.Add(DefaultEmptyGrace - time.Second)
	m.Sweep()
	sc, err := m.Join(sa.Room, pw, "carol", "", "", 0, &box{})
	if err != nil {
		t.Fatalf("join inside empty grace: %v", err)
	}
	// Rejoining resets the clock; leaving starts it again.
	m.Leave(sc.Room, sc.PeerID)
	now = now.Add(DefaultEmptyGrace - time.Second)
	m.Sweep()
	if m.Count() != 1 {
		t.Fatal("room ended inside its empty grace period")
	}
	now = now.Add(2 * time.Second)
	m.Sweep()
	if m.Count() != 0 {
		t.Fatal("room survived its empty grace period")
	}
	if _, err := m.Join(sa.Room, pw, "dave", "", "", 0, &box{}); err != ErrNoRoom {
		t.Fatalf("join ended room: %v", err)
	}
}

func TestResumeAndSweep(t *testing.T) {
	m := NewManager(time.Minute)
	now := time.Unix(1_000_000, 0)
	m.now = func() time.Time { return now }

	a1, b := &box{}, &box{}
	sa, _ := m.Create(pw, "alice", "", "", 0, false, a1)
	sb, _ := m.Join(sa.Room, pw, "bob", "", "", 0, b)

	m.Disconnected(sa.Room, sa.PeerID, a1)
	if _, err := m.Resume(sa.Room, sa.PeerID, "wrong", &box{}); err != ErrResumeFailed {
		t.Fatalf("bad token: %v", err)
	}
	a2 := &box{}
	rs, err := m.Resume(sa.Room, sa.PeerID, sa.Resume, a2)
	if err != nil || len(rs.Others) != 1 || rs.Others[0].PeerID != sb.PeerID {
		t.Fatalf("resume: %v %+v", err, rs)
	}
	// The old socket closing late must not detach the resumed one.
	m.Disconnected(sa.Room, sa.PeerID, a1)
	if !m.Relay(sb.Room, sb.PeerID, sa.PeerID, "after") {
		t.Fatal("resumed peer unreachable after stale disconnect")
	}

	// Both drop; after the grace period the room ends.
	m.Disconnected(sa.Room, sa.PeerID, a2)
	m.Disconnected(sb.Room, sb.PeerID, b)
	now = now.Add(59 * time.Second)
	m.Sweep()
	if m.Count() != 1 {
		t.Fatal("swept inside grace window")
	}
	now = now.Add(2 * time.Second)
	m.Sweep() // peers expire; the empty room is kept for EmptyGrace
	now = now.Add(DefaultEmptyGrace + time.Second)
	m.Sweep()
	if m.Count() != 0 {
		t.Fatal("room with only expired peers survived")
	}
}

// topologies returns the relay queues a box was sent, in order.
func topologies(b *box) [][]string {
	var qs [][]string
	for _, msg := range b.all() {
		if t, ok := msg.(proto.Topology); ok {
			qs = append(qs, t.Queue)
		}
	}
	return qs
}

func TestRelayQueue(t *testing.T) {
	m := NewManager(time.Minute)
	b := DefaultMinRelayBuild
	a, bb, c := &box{}, &box{}, &box{}
	sa, _ := m.Create(pw, "alice", "", "", b, false, a)
	sb, _ := m.Join(sa.Room, pw, "bob", "", "", b, bb)
	sc, _ := m.Join(sa.Room, pw, "carol", "", "", b, c)

	if err := m.Volunteer(sa.Room, sb.PeerID, true); err != nil {
		t.Fatal(err)
	}
	m.Volunteer(sa.Room, sb.PeerID, true) // no-op: already queued
	m.Volunteer(sa.Room, sc.PeerID, true)
	want := [][]string{{sb.PeerID}, {sb.PeerID, sc.PeerID}}
	if got := topologies(a); !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("alice saw %v, want %v", got, want)
	}

	// A late joiner learns the queue from its seat.
	sd, err := m.Join(sa.Room, pw, "dave", "", "", b, &box{})
	if err != nil || !slices.Equal(sd.RelayQueue, []string{sb.PeerID, sc.PeerID}) {
		t.Fatalf("dave: %v %v", err, sd.RelayQueue)
	}

	// The active relay withdraws: carol takes over. Then carol leaves the
	// room: back to mesh.
	m.Volunteer(sa.Room, sb.PeerID, false)
	m.Leave(sa.Room, sc.PeerID)
	got := topologies(bb)
	if n := len(got); n < 2 || !slices.Equal(got[n-2], []string{sc.PeerID}) || len(got[n-1]) != 0 {
		t.Fatalf("bob saw %v", got)
	}
	// Withdrawing when not queued sends nothing.
	before := len(topologies(a))
	m.Volunteer(sa.Room, sb.PeerID, false)
	if len(topologies(a)) != before {
		t.Fatal("no-op withdraw broadcast a topology")
	}
}

func TestRelayBuildGate(t *testing.T) {
	m := NewManager(time.Minute)
	b := DefaultMinRelayBuild
	sa, _ := m.Create(pw, "alice", "", "", b, false, &box{})
	sOld, _ := m.Join(sa.Room, pw, "old", "", "", b-1, &box{})
	if err := m.Volunteer(sa.Room, sa.PeerID, true); err != ErrRelayUnsupported {
		t.Fatalf("volunteer with an old member: %v", err)
	}
	m.Leave(sa.Room, sOld.PeerID)
	if err := m.Volunteer(sa.Room, sa.PeerID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Join(sa.Room, pw, "old", "", "", b-1, &box{}); err != ErrUpdateRequired {
		t.Fatalf("old join during relay: %v", err)
	}
	m.Volunteer(sa.Room, sa.PeerID, false)
	if _, err := m.Join(sa.Room, pw, "old", "", "", b-1, &box{}); err != nil {
		t.Fatalf("old join in mesh: %v", err)
	}
}

func TestE2EERoomRefusesOldBuilds(t *testing.T) {
	m := NewManager(time.Minute)
	if _, err := m.Create(pw, "old", "k0", "", proto.E2EEBuild-1, true, &box{}); err != ErrBadRequest {
		t.Fatalf("old build created an encrypted room: %v", err)
	}
	a, err := m.Create(pw, "alice", "ka", "", proto.E2EEBuild, true, &box{})
	if err != nil || !a.E2EE {
		t.Fatalf("create: %+v %v", a, err)
	}
	if _, err := m.Join(a.Room, pw, "bob", "kb", "", proto.E2EEBuild-1, &box{}); err != ErrUpdateRequired {
		t.Fatalf("old build joined: %v", err)
	}
	b, err := m.Join(a.Room, pw, "carol", "kc", "", proto.E2EEBuild, &box{})
	if err != nil || !b.E2EE {
		t.Fatalf("join: %+v %v", b, err)
	}
	// Rooms without encryption still take old builds.
	c, _ := m.Create(pw, "dave", "kd", "", proto.E2EEBuild, false, &box{})
	if _, err := m.Join(c.Room, pw, "erin", "ke", "", 30, &box{}); err != nil {
		t.Fatalf("old build refused from a plain room: %v", err)
	}
}

func TestOpenRoom(t *testing.T) {
	m := NewManager(time.Minute)
	s, err := m.Create("", "alice", "k1", "", 0, false, &box{})
	if err != nil {
		t.Fatal(err)
	}
	// No password, or any password an older client sends, gets in.
	for _, p := range []string{"", "whatever"} {
		if _, err := m.Join(s.Room, p, "bob", "", "", 0, &box{}); err != nil {
			t.Fatalf("join with %q: %v", p, err)
		}
	}
}

func TestChannelTie(t *testing.T) {
	m := NewManager(time.Minute)
	if _, ok := m.Channel("123456789"); ok {
		t.Fatal("no room yet")
	}
	s, _ := m.Create("", "alice", "k1", "", 0, false, &box{})
	if err := m.TieChannel(s.Room, "123456789"); err != nil {
		t.Fatal(err)
	}
	cr, ok := m.Channel("123456789")
	if !ok || cr.Room != s.Room || !cr.Open || cr.People != 1 {
		t.Fatalf("Channel = %+v %v", cr, ok)
	}
	s2, _ := m.Create(pw, "bob", "k2", "", 0, false, &box{})
	if err := m.TieChannel(s2.Room, "123456789"); err != ErrChannelBusy {
		t.Fatalf("second tie: %v", err)
	}
	if err := m.TieChannel(s2.Room, "not-a-snowflake"); err != ErrBadRequest {
		t.Fatalf("bad id: %v", err)
	}
}
