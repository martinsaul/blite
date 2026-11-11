package room

import (
	"testing"
	"time"

	"blite/internal/proto"
)

func lastPlan(b *box) (proto.Plan, int) {
	n := 0
	var last proto.Plan
	for _, m := range b.all() {
		if p, ok := m.(proto.Plan); ok {
			last = p
			n++
		}
	}
	return last, n
}

func countHomes(p proto.Plan) map[string]int {
	n := map[string]int{}
	for id, h := range p.Home {
		if id != h {
			n[h]++
		}
	}
	return n
}

// Newer builds get plans, not topologies, and one relay serves a room of
// any size while it has capacity; more volunteers become relays when it
// runs out, and homes stay put.
func TestPlanMultiRelay(t *testing.T) {
	m := NewManager(time.Minute)
	nb := proto.MultiRelayBuild
	a := &box{}
	sa, _ := m.Create(pw, "alice", "", "", nb, false, a)
	if sa.Plan == nil || len(sa.Plan.Relays) != 0 {
		t.Fatalf("creator's plan: %+v", sa.Plan)
	}
	ids := []string{sa.PeerID}
	boxes := []*box{a}
	for i := 1; i < 4; i++ {
		b := &box{}
		s, _ := m.Join(sa.Room, pw, "p", "", "", nb, b)
		ids, boxes = append(ids, s.PeerID), append(boxes, b)
	}
	if err := m.Volunteer(sa.Room, ids[0], true); err != nil {
		t.Fatal(err)
	}
	p, _ := lastPlan(a)
	if len(p.Relays) != 1 || p.Relays[0] != ids[0] || p.Home[ids[3]] != ids[0] || p.Epoch == 0 {
		t.Fatalf("one relay: %+v", p)
	}
	for _, m := range a.all() {
		if _, ok := m.(proto.Topology); ok {
			t.Fatal("a newer build got a topology")
		}
	}
	// A second volunteer isn't needed yet (the default capacity is 4).
	m.Volunteer(sa.Room, ids[1], true)
	if p, _ := lastPlan(a); len(p.Relays) != 1 || len(p.Queue) != 2 {
		t.Fatalf("second volunteer while not needed: %+v", p)
	}
	// Grow past what one relay takes: the second volunteer relays too, and
	// the first one's members stay with it.
	for i := 0; i < 4; i++ {
		b := &box{}
		s, _ := m.Join(sa.Room, pw, "q", "", "", nb, b)
		ids, boxes = append(ids, s.PeerID), append(boxes, b)
	}
	p, _ = lastPlan(a)
	if len(p.Relays) != 2 || p.Relays[0] != ids[0] || p.Relays[1] != ids[1] {
		t.Fatalf("two relays: %+v", p)
	}
	if p.Home[ids[2]] != ids[0] || p.Home[ids[1]] != ids[1] || p.Paused {
		t.Fatalf("homes: %+v", p.Home)
	}
	if n := countHomes(p); n[ids[0]] > defaultChildren || n[ids[1]] > defaultChildren {
		t.Fatalf("over capacity: %v", n)
	}
	// The new joiner learns the plan from its seat; the plan is the same.
	b := &box{}
	s, _ := m.Join(sa.Room, pw, "late", "", "", nb, b)
	if s.Plan == nil || s.Plan.Home[s.PeerID] == "" {
		t.Fatalf("late joiner's plan: %+v", s.Plan)
	}
	p, _ = lastPlan(a)
	if p.Epoch != s.Plan.Epoch {
		t.Fatalf("epochs: %d vs %d", p.Epoch, s.Plan.Epoch)
	}
	// The first relay withdraws: everyone moves to the second.
	m.Volunteer(sa.Room, ids[0], false)
	p, _ = lastPlan(a)
	if len(p.Relays) != 1 || p.Relays[0] != ids[1] || p.Home[ids[0]] != ids[1] {
		t.Fatalf("after withdrawal: %+v", p)
	}
}

// Reported upload decides capacity and which volunteer relays first;
// TURN-only volunteers never relay.
func TestPlanCaps(t *testing.T) {
	m := NewManager(time.Minute)
	nb := proto.MultiRelayBuild
	a := &box{}
	sa, _ := m.Create(pw, "alice", "", "", nb, false, a)
	sb, _ := m.Join(sa.Room, pw, "bob", "", "", nb, &box{})
	sc, _ := m.Join(sa.Room, pw, "carol", "", "", nb, &box{})
	for i := 0; i < 6; i++ {
		m.Join(sa.Room, pw, "x", "", "", nb, &box{})
	}
	m.SetCaps(sa.Room, sb.PeerID, proto.Caps{UpKbps: 100_000, UpSource: "declared", Reach: "public"})
	m.SetCaps(sa.Room, sc.PeerID, proto.Caps{UpKbps: 500_000, Reach: "turn"})
	m.Volunteer(sa.Room, sc.PeerID, true)
	if p, _ := lastPlan(a); len(p.Relays) != 0 {
		t.Fatalf("TURN-only relay: %+v", p)
	}
	m.Volunteer(sa.Room, sa.PeerID, true)
	m.Volunteer(sa.Room, sb.PeerID, true)
	// alice (default capacity 4) relays first: she volunteered first and
	// bob alone wasn't there yet. With bob in, his capacity covers the room
	// but alice stays (sticky).
	p, _ := lastPlan(a)
	if len(p.Relays) != 2 || p.Relays[0] != sa.PeerID || p.Relays[1] != sb.PeerID {
		t.Fatalf("relays: %+v", p)
	}
	if err := m.SetCaps(sa.Room, sa.PeerID, proto.Caps{Reach: "bogus"}); err != ErrBadRequest {
		t.Fatalf("bad caps: %v", err)
	}
}

// With an older build in the room the plan is the legacy one: the queue's
// head relays for everyone, paused above proto.RelayMaxPeers; the older
// member gets topologies.
func TestPlanLegacyRoom(t *testing.T) {
	m := NewManager(time.Minute)
	nb := proto.MultiRelayBuild
	a, old := &box{}, &box{}
	sa, _ := m.Create(pw, "alice", "", "", nb, false, a)
	sb, _ := m.Join(sa.Room, pw, "bob", "", "", nb, &box{})
	m.Join(sa.Room, pw, "old", "", "", nb-1, old)
	m.Volunteer(sa.Room, sb.PeerID, true)
	m.Volunteer(sa.Room, sa.PeerID, true)
	p, _ := lastPlan(a)
	if len(p.Relays) != 1 || p.Relays[0] != sb.PeerID || p.Home[sa.PeerID] != sb.PeerID {
		t.Fatalf("legacy plan: %+v", p)
	}
	if _, n := lastPlan(old); n != 0 {
		t.Fatal("older build got a plan")
	}
	if tp, _ := lastTopology(old); tp.Relay() != sb.PeerID {
		t.Fatalf("older build's topology: %+v", tp)
	}
	for i := 0; i < 3; i++ {
		m.Join(sa.Room, pw, "x", "", "", nb, &box{})
	}
	if p, _ := lastPlan(a); !p.Paused || len(p.Relays) != 0 {
		t.Fatalf("legacy room of 6: %+v", p)
	}
	if tp, _ := lastTopology(old); !tp.Paused {
		t.Fatalf("older build not told of the pause: %+v", tp)
	}
}

func TestCapacity(t *testing.T) {
	for _, c := range []struct {
		caps proto.Caps
		want int
	}{
		{proto.Caps{}, defaultChildren},
		{proto.Caps{UpKbps: 1000, UpSource: "declared"}, minChildren},
		{proto.Caps{UpKbps: 1000, UpSource: "passive"}, defaultChildren},
		{proto.Caps{UpKbps: 50_000, UpSource: "passive"}, 7},
		{proto.Caps{UpKbps: 1_000_000, UpSource: "declared"}, maxChildren},
	} {
		if got := (&Peer{caps: c.caps}).capacity(); got != c.want {
			t.Errorf("%+v: %d, want %d", c.caps, got, c.want)
		}
	}
}
