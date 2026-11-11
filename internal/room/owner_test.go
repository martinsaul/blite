package room

import (
	"testing"
	"time"

	"blite/internal/proto"
)

func ownerOf(m *Manager, code string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rooms[code].owner
}

func lastOwnerMsg(b *box) (string, bool) {
	all := b.all()
	for i := len(all) - 1; i >= 0; i-- {
		if o, ok := all[i].(proto.Owner); ok {
			return o.PeerID, true
		}
	}
	return "", false
}

// Ownership: the creator owns the room; an owner who drops keeps it for
// OwnerReturnWindow, then it goes to the earliest-joined online member and
// a late resume doesn't get it back; transfer is owner-only; an emptied
// room goes to the next joiner.
func TestOwnership(t *testing.T) {
	m := NewManager(time.Hour)
	now := time.Now()
	m.now = func() time.Time { return now }
	ba, bb, bc := &box{}, &box{}, &box{}
	a, _ := m.Create(pw, "alice", "ka", "", 0, false, ba)
	if a.Owner != a.PeerID {
		t.Fatalf("creator not owner: %q", a.Owner)
	}
	b, _ := m.Join(a.Room, pw, "bob", "kb", "", 0, bb)
	c, _ := m.Join(a.Room, pw, "carol", "kc", "", 0, bc)
	if b.Owner != a.PeerID || c.Owner != a.PeerID {
		t.Fatal("joiners don't see alice as owner")
	}
	m.Disconnected(a.Room, a.PeerID, ba)
	if got := ownerOf(m, a.Room); got != a.PeerID {
		t.Fatalf("alice dropped: owner %q, want alice (held)", got)
	}
	if err := m.Transfer(a.Room, b.PeerID, c.PeerID); err != ErrNotOwner {
		t.Fatalf("bob moderated a held room: %v", err)
	}
	ra, err := m.Resume(a.Room, a.PeerID, a.Resume, ba)
	if err != nil || ra.Owner != a.PeerID {
		t.Fatalf("alice resumed in time: owner %q err %v, want alice", ra.Owner, err)
	}
	m.Disconnected(a.Room, a.PeerID, ba)
	now = now.Add(OwnerReturnWindow - time.Second)
	m.Sweep()
	if got := ownerOf(m, a.Room); got != a.PeerID {
		t.Fatalf("hold ended early: owner %q", got)
	}
	now = now.Add(2 * time.Second)
	m.Sweep()
	if got := ownerOf(m, a.Room); got != b.PeerID {
		t.Fatalf("after the hold: owner %q, want bob (earliest online)", got)
	}
	if o, _ := lastOwnerMsg(bc); o != b.PeerID {
		t.Fatalf("carol told %q", o)
	}
	ra, err = m.Resume(a.Room, a.PeerID, a.Resume, ba)
	if err != nil || ra.Owner != b.PeerID {
		t.Fatalf("alice resumed: owner %q err %v, want bob", ra.Owner, err)
	}
	if err := m.Transfer(a.Room, a.PeerID, a.PeerID); err != ErrNotOwner {
		t.Fatalf("non-owner transfer: %v", err)
	}
	if err := m.Transfer(a.Room, b.PeerID, c.PeerID); err != nil || ownerOf(m, a.Room) != c.PeerID {
		t.Fatalf("transfer to carol: %v", err)
	}
	m.Leave(a.Room, c.PeerID)
	if got := ownerOf(m, a.Room); got != a.PeerID {
		t.Fatalf("carol left: owner %q, want alice (joined first)", got)
	}
	m.Leave(a.Room, a.PeerID)
	m.Leave(a.Room, b.PeerID)
	d, err := m.Join(a.Room, pw, "dave", "kd", "", 0, &box{})
	if err != nil || d.Owner != d.PeerID {
		t.Fatalf("joiner of an empty room: owner %q err %v", d.Owner, err)
	}
}

// Kick: owner-only, disconnect lets them back, ban doesn't.
func TestKick(t *testing.T) {
	m := NewManager(time.Minute)
	ba, bb := &box{}, &box{}
	a, _ := m.Create(pw, "alice", "ka", "", 0, false, ba)
	b, _ := m.Join(a.Room, pw, "bob", "kb", "", 0, bb)
	if err := m.Kick(a.Room, b.PeerID, a.PeerID, false); err != ErrNotOwner {
		t.Fatalf("non-owner kick: %v", err)
	}
	if err := m.Kick(a.Room, a.PeerID, b.PeerID, false); err != nil {
		t.Fatal(err)
	}
	all := bb.all()
	if k, ok := all[len(all)-1].(proto.Kicked); !ok || k.By != "alice" || k.Ban {
		t.Fatalf("bob got %#v", all[len(all)-1])
	}
	if _, err := m.Resume(a.Room, b.PeerID, b.Resume, bb); err != ErrResumeFailed {
		t.Fatalf("kicked member resumed: %v", err)
	}
	b2, err := m.Join(a.Room, pw, "bob", "kb", "", 0, &box{})
	if err != nil {
		t.Fatalf("disconnected member can't rejoin: %v", err)
	}
	if err := m.Kick(a.Room, a.PeerID, b2.PeerID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Join(a.Room, pw, "bob", "kb", "", 0, &box{}); err != ErrBanned {
		t.Fatalf("banned member rejoined: %v", err)
	}
	if _, err := m.Join(a.Room, pw, "bob2", "kother", "", 0, &box{}); err != nil {
		t.Fatalf("another key: %v", err)
	}
}

// Hosted rooms: the relay owns the room it created; listed members take
// it over while any of them is online, in join order; it returns to the
// relay when they're gone; the relay can't be kicked.
func TestHostedOwnership(t *testing.T) {
	m := NewManager(time.Minute)
	code := NewCode()
	h, err := m.CreateFixed(code, pw, "relay", "khost", "", 0, false, []string{"kb", "kc"}, &box{})
	if err != nil || h.Owner != h.PeerID {
		t.Fatalf("host not owner: %q %v", h.Owner, err)
	}
	ba, bb := &box{}, &box{}
	a, _ := m.Join(code, pw, "alice", "ka", "", 0, ba)
	if ownerOf(m, code) != h.PeerID {
		t.Fatal("unlisted member took ownership")
	}
	b, _ := m.Join(code, pw, "bob", "kb", "", 0, bb)
	if b.Owner != b.PeerID {
		t.Fatalf("listed bob: owner %q", b.Owner)
	}
	if o, _ := lastOwnerMsg(ba); o != b.PeerID {
		t.Fatal("alice not told")
	}
	c, _ := m.Join(code, pw, "carol", "kc", "", 0, &box{})
	now := time.Now()
	m.now = func() time.Time { return now }
	m.Disconnected(code, b.PeerID, bb)
	if got := ownerOf(m, code); got != b.PeerID {
		t.Fatalf("bob dropped: owner %q, want bob (held)", got)
	}
	now = now.Add(OwnerReturnWindow + time.Second)
	m.Sweep()
	if got := ownerOf(m, code); got != c.PeerID {
		t.Fatalf("bob dropped: owner %q, want carol (listed)", got)
	}
	if err := m.Kick(code, c.PeerID, h.PeerID, true); err != ErrNoPeer {
		t.Fatalf("kicked the host: %v", err)
	}
	m.Leave(code, c.PeerID)
	if got := ownerOf(m, code); got != h.PeerID {
		t.Fatalf("listed members gone: owner %q, want the relay", got)
	}
	_ = a
	// The relay restarting re-joins the existing room and is its host again.
	h2, err := m.CreateFixed(code, pw, "relay", "khost", "", 0, false, []string{"ka"}, &box{})
	if err != nil {
		t.Fatal(err)
	}
	if got := ownerOf(m, code); got != a.PeerID || h2.Owner != a.PeerID {
		t.Fatalf("new list names alice: owner %q", got)
	}
}

// An owner whose resume arrives before its old connection is seen to close
// keeps ownership, and the late close of the old socket changes nothing.
func TestOwnerFastResumeKeeps(t *testing.T) {
	m := NewManager(time.Minute)
	ba, bb := &box{}, &box{}
	a, _ := m.Create(pw, "alice", "ka", "", 0, false, ba)
	b, _ := m.Join(a.Room, pw, "bob", "kb", "", 0, bb)
	if err := m.Transfer(a.Room, a.PeerID, b.PeerID); err != nil {
		t.Fatal(err)
	}
	rb, err := m.Resume(a.Room, b.PeerID, b.Resume, &box{})
	if err != nil || rb.Owner != b.PeerID {
		t.Fatalf("bob resumed: owner %q err %v, want bob", rb.Owner, err)
	}
	m.Disconnected(a.Room, b.PeerID, bb) // the old socket, late: no effect
	if ownerOf(m, a.Room) != b.PeerID {
		t.Fatal("owner changed again")
	}
}

func TestRename(t *testing.T) {
	m := NewManager(time.Minute)
	ba, bb := &box{}, &box{}
	a, _ := m.Create(pw, "alice", "ka", "", 0, false, ba)
	b, _ := m.Join(a.Room, pw, "bob", "kb", "", 0, bb)
	if err := m.Rename(a.Room, b.PeerID, "bob#0001"); err != ErrBadRequest {
		t.Fatalf("'#' accepted: %v", err)
	}
	if _, err := m.Join(a.Room, pw, "eve#1234", "ke", "", 0, &box{}); err != ErrBadRequest {
		t.Fatalf("join with '#': %v", err)
	}
	if err := m.Rename(a.Room, b.PeerID, "Robert"); err != nil {
		t.Fatal(err)
	}
	all := ba.all()
	if r, ok := all[len(all)-1].(proto.PeerRenamed); !ok || r.PeerID != b.PeerID || r.Name != "Robert" {
		t.Fatalf("alice got %#v", all[len(all)-1])
	}
	c, _ := m.Join(a.Room, pw, "carol", "kc", "", 0, &box{})
	for _, p := range c.Others {
		if p.PeerID == b.PeerID && p.Name != "Robert" {
			t.Fatalf("newcomer sees %q", p.Name)
		}
	}
}

// An owner who leaves to update keeps the room for OwnerReturnWindow:
// the same identity rejoining takes it back (and its dropped slot goes);
// another identity doesn't, a plain leave hands over at once, and after the
// window it passes on.
func TestOwnerReturnsAfterUpdate(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	m := NewManager(time.Minute)
	m.now = func() time.Time { return now }
	ba, bb := &box{}, &box{}
	a, _ := m.Create(pw, "alice", "ka", "", 0, false, ba)
	b, _ := m.Join(a.Room, pw, "bob", "kb", "", 0, bb)

	m.LeaveForUpdate(a.Room, a.PeerID)
	if got := ownerOf(m, a.Room); got != a.PeerID {
		t.Fatalf("alice left to update: owner %q, want alice (held)", got)
	}
	if c, _ := m.Join(a.Room, pw, "carol", "kc", "", 0, &box{}); c.Owner != a.PeerID {
		t.Fatalf("another identity took the room: owner %q", c.Owner)
	}
	now = now.Add(OwnerReturnWindow - time.Second)
	m.Sweep()
	a2, err := m.Join(a.Room, pw, "alice", "ka", "", 0, ba)
	if err != nil || a2.Owner != a2.PeerID {
		t.Fatalf("alice rejoined in time: owner %q err %v, want alice", a2.Owner, err)
	}
	if o, _ := lastOwnerMsg(bb); o != a2.PeerID {
		t.Fatalf("bob told owner %q, want alice", o)
	}

	// A plain leave hands over at once.
	m.Leave(a.Room, a2.PeerID)
	if got := ownerOf(m, a.Room); got != b.PeerID {
		t.Fatalf("plain leave: owner %q, want bob", got)
	}

	// Too late.
	m.LeaveForUpdate(a.Room, b.PeerID)
	now = now.Add(OwnerReturnWindow + time.Second)
	m.Sweep()
	if got := ownerOf(m, a.Room); got == b.PeerID || got == "" {
		t.Fatalf("after the window: owner %q, want carol", got)
	}
	if b2, _ := m.Join(a.Room, pw, "bob", "kb", "", 0, bb); b2.Owner == b2.PeerID {
		t.Fatal("bob reclaimed after the window")
	}
}

// An owner who crashes mid-update (connection drops, no leave) and comes
// back as a new peer with the same identity reclaims, and its stale slot
// is removed.
func TestOwnerReclaimsDroppedSlot(t *testing.T) {
	m := NewManager(time.Minute)
	ba, bb := &box{}, &box{}
	a, _ := m.Create(pw, "alice", "ka", "", 0, false, ba)
	m.Join(a.Room, pw, "bob", "kb", "", 0, bb)
	m.Disconnected(a.Room, a.PeerID, ba)
	a2, _ := m.Join(a.Room, pw, "alice", "ka", "", 0, &box{})
	if a2.Owner != a2.PeerID {
		t.Fatalf("owner %q, want the new alice", a2.Owner)
	}
	m.mu.Lock()
	_, stale := m.rooms[a.Room].peers[a.PeerID]
	m.mu.Unlock()
	if stale {
		t.Fatal("the dropped slot is still there")
	}
}
