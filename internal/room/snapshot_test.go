package room

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"
)

// A restored room is the same room: members resume with their old tokens,
// the owner keeps it even if someone else is back first, and the password,
// bans, relay queue and Discord tie survive. The snapshot holds neither the
// password nor a usable resume token.
func TestSnapshotRoundTrip(t *testing.T) {
	m := NewManager(time.Minute)
	ba, bb, bc := &box{}, &box{}, &box{}
	a, err := m.Create(pw, "alice", "ka", "", 40, false, ba)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := m.Join(a.Room, pw, "bob", "kb", "", 40, bb)
	c, _ := m.Join(a.Room, pw, "carol", "kc", "", 40, bc)
	if err := m.Volunteer(a.Room, b.PeerID, true); err != nil {
		t.Fatal(err)
	}
	if err := m.Kick(a.Room, a.PeerID, c.PeerID, true); err != nil {
		t.Fatal(err)
	}
	if err := m.TieChannel(a.Room, "123456789"); err != nil {
		t.Fatal(err)
	}
	data, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatal(err)
	}
	tokens := map[string]string{a.PeerID: a.Resume, b.PeerID: b.Resume}
	for _, p := range snap.Rooms[0].Peers {
		if h := sha256.Sum256([]byte(tokens[p.ID])); !bytes.Equal(p.Resume, h[:]) {
			t.Fatalf("%s: stored resume is not the token's hash", p.Name)
		}
	}
	if k := snap.Rooms[0].Key; len(k) != 32 || bytes.Contains(k, []byte(pw)) || bytes.Contains(data, []byte(pw)) {
		t.Fatal("snapshot holds the password")
	}

	m2 := NewManager(time.Minute)
	if n, err := m2.Restore(data); err != nil || n != 1 {
		t.Fatalf("restore: %d %v", n, err)
	}
	if ch, ok := m2.Channel("123456789"); !ok || ch.Room != a.Room || ch.People != 2 {
		t.Fatalf("channel after restore: %+v %v", ch, ok)
	}
	rb, err := m2.Resume(a.Room, b.PeerID, b.Resume, &box{})
	if err != nil {
		t.Fatalf("bob resume: %v", err)
	}
	if rb.Owner != a.PeerID {
		t.Fatalf("bob back first took the room: owner %q, want alice (held)", rb.Owner)
	}
	if len(rb.RelayQueue) != 1 || rb.RelayQueue[0] != b.PeerID {
		t.Fatalf("relay queue lost: %v", rb.RelayQueue)
	}
	if _, err := m2.Resume(a.Room, a.PeerID, "wrong", &box{}); err != ErrResumeFailed {
		t.Fatalf("wrong token: %v", err)
	}
	ra, err := m2.Resume(a.Room, a.PeerID, a.Resume, &box{})
	if err != nil || ra.Owner != a.PeerID || ra.Resume != a.Resume {
		t.Fatalf("alice resume: %+v %v", ra, err)
	}
	if _, err := m2.Join(a.Room, "wrong password", "dave", "kd", "", 40, &box{}); err != ErrBadPassword {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := m2.Join(a.Room, pw, "carol", "kc", "", 40, &box{}); err != ErrBanned {
		t.Fatalf("ban lost: %v", err)
	}
	if _, err := m2.Join(a.Room, pw, "dave", "kd", "", 40, &box{}); err != nil {
		t.Fatalf("join after restore: %v", err)
	}
}

// After a restore, members who were connected get a full ResumeGrace from
// the restore; one that had already dropped keeps its old deadline. An
// owner who never comes back keeps the room for OwnerReturnWindow from the
// restore, then it hands over.
func TestSnapshotGrace(t *testing.T) {
	m := NewManager(time.Minute)
	now := time.Now()
	m.now = func() time.Time { return now }
	ba, bb, bc := &box{}, &box{}, &box{}
	a, _ := m.Create(pw, "alice", "ka", "", 0, false, ba)
	b, _ := m.Join(a.Room, pw, "bob", "kb", "", 0, bb)
	c, _ := m.Join(a.Room, pw, "carol", "kc", "", 0, bc)
	now = now.Add(-30 * time.Second)
	m.Disconnected(a.Room, c.PeerID, bc) // carol dropped 30 s before the snapshot
	now = now.Add(30 * time.Second)
	data, _ := m.Snapshot()

	m2 := NewManager(time.Minute)
	now = now.Add(10 * time.Second) // the restart took 10 s
	m2.now = func() time.Time { return now }
	if _, err := m2.Restore(data); err != nil {
		t.Fatal(err)
	}
	if _, err := m2.Resume(a.Room, b.PeerID, b.Resume, &box{}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(25 * time.Second)
	m2.Sweep()
	if _, err := m2.Resume(a.Room, c.PeerID, c.Resume, &box{}); err != ErrResumeFailed {
		t.Fatalf("carol outlived her old deadline: %v", err)
	}
	now = now.Add(30 * time.Second) // 55 s after the restore
	m2.Sweep()
	if got := ownerOf(m2, a.Room); got != a.PeerID {
		t.Fatalf("owner %q before the hold ran out, want alice", got)
	}
	now = now.Add(10 * time.Second) // 65 s: alice's grace is over
	m2.Sweep()
	if _, err := m2.Resume(a.Room, a.PeerID, a.Resume, &box{}); err != ErrResumeFailed {
		t.Fatalf("alice resumed after her grace: %v", err)
	}
	if got := ownerOf(m2, a.Room); got != a.PeerID {
		t.Fatalf("owner %q, want alice (held for her identity until OwnerReturnWindow)", got)
	}
	now = now.Add(OwnerReturnWindow - 65*time.Second + time.Second)
	m2.Sweep()
	if got := ownerOf(m2, a.Room); got != b.PeerID {
		t.Fatalf("owner %q after OwnerReturnWindow, want bob", got)
	}
}

// Restore replaces a room only if it came from an earlier Restore and
// nobody has joined or resumed it since. One this server has served, even if
// everyone has dropped again, is newer than the file: a reload must not undo
// a kick.
func TestSnapshotRestoreMerge(t *testing.T) {
	src := NewManager(time.Minute)
	a, _ := src.Create(pw, "alice", "ka", "", 0, false, &box{})
	data, _ := src.Snapshot()
	src.Rename(a.Room, a.PeerID, "alice2")
	data2, _ := src.Snapshot()

	dst := NewManager(time.Minute)
	if _, err := dst.Restore(data); err != nil {
		t.Fatal(err)
	}
	if n, err := dst.Restore(data2); err != nil || n != 1 {
		t.Fatalf("reload over an untouched room: %d %v", n, err)
	}
	if name := dst.rooms[a.Room].peers[a.PeerID].Name; name != "alice2" {
		t.Fatalf("untouched room not replaced: %q", name)
	}
	ob := &box{}
	if _, err := dst.Resume(a.Room, a.PeerID, a.Resume, ob); err != nil {
		t.Fatal(err)
	}
	dst.Rename(a.Room, a.PeerID, "alice3")
	dst.Disconnected(a.Room, a.PeerID, ob)
	if n, err := dst.Restore(data2); err != nil || n != 0 {
		t.Fatalf("reload over a served room: %d %v", n, err)
	}
	if name := dst.rooms[a.Room].peers[a.PeerID].Name; name != "alice3" {
		t.Fatalf("served room overwritten: %q", name)
	}
}

// A Discord channel that a newer room took over stays with the newer room
// after a restore, even when the old room has people again.
func TestSnapshotChannelTakeover(t *testing.T) {
	for range 20 { // Restore walks rooms in map order
		m := NewManager(time.Minute)
		ba := &box{}
		a, _ := m.Create("", "alice", "ka", "", 0, false, ba)
		m.TieChannel(a.Room, "123456789")
		m.Leave(a.Room, a.PeerID) // room A is empty but alive
		b, _ := m.Create("", "bob", "kb", "", 0, false, &box{})
		if err := m.TieChannel(b.Room, "123456789"); err != nil {
			t.Fatal(err)
		}
		m.Join(a.Room, "", "carol", "kc", "", 0, &box{}) // A has people again
		data, _ := m.Snapshot()
		m2 := NewManager(time.Minute)
		if _, err := m2.Restore(data); err != nil {
			t.Fatal(err)
		}
		if ch, ok := m2.Channel("123456789"); !ok || ch.Room != b.Room {
			t.Fatalf("channel went to %q, want the newer room %q", ch.Room, b.Room)
		}
	}
}

// Saving while members join, get kicked and banned, and volunteer must not
// race (run with -race).
func TestSnapshotDuringTraffic(t *testing.T) {
	m := NewManager(time.Minute)
	a, _ := m.Create(pw, "alice", "ka", "", 40, false, &box{})
	m.mu.Lock()
	m.rooms[a.Room].pw = passwordHash{} // an open room: no Argon2 per join
	m.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 200 {
			s, err := m.Join(a.Room, "", "bob", string(rune('a'+i%26))+"k", "", 40, &box{})
			if err != nil {
				continue
			}
			m.Volunteer(a.Room, s.PeerID, true)
			m.Kick(a.Room, a.PeerID, s.PeerID, i%2 == 0)
		}
	}()
	for {
		select {
		case <-done:
			return
		default:
			if _, err := m.Snapshot(); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// The owner's hold survives a restore with its identity key: the owner,
// back on a new install after its slot ran out, gets the room back.
func TestSnapshotOwnerReclaims(t *testing.T) {
	m := NewManager(time.Minute)
	now := time.Now()
	m.now = func() time.Time { return now }
	a, _ := m.Create(pw, "alice", "ka", "", 0, false, &box{})
	b, _ := m.Join(a.Room, pw, "bob", "kb", "", 0, &box{})
	data, _ := m.Snapshot()

	m2 := NewManager(time.Minute)
	m2.now = func() time.Time { return now }
	m2.Restore(data)
	m2.Resume(a.Room, b.PeerID, b.Resume, &box{})
	now = now.Add(90 * time.Second) // alice's slot is gone, her hold isn't
	m2.Sweep()
	a2, err := m2.Join(a.Room, pw, "alice", "ka", "", 0, &box{})
	if err != nil || a2.Owner != a2.PeerID {
		t.Fatalf("alice rejoined: owner %q err %v, want her new ID %q", a2.Owner, err, a2.PeerID)
	}
}

func TestSnapshotBad(t *testing.T) {
	m := NewManager(time.Minute)
	for _, in := range []string{"", "{", `{"v":2,"rooms":[]}`, `{"v":1,"rooms":[{"code":"bad code"}]}`, `{"v":1,"rooms":[{"code":"7K3QX9MD","peers":[{"id":"p","resume":"AAAA"}]}]}`,
		`{"v":1,"rooms":[{"code":"7K3QX9MD","peers":[]},{"code":"7K3QX9MD","peers":[]}]}`,
		`{"v":1,"rooms":[{"code":"7K3QX9MD","owner":"ghost","peers":[]}]}`,
		`{"v":1,"rooms":[{"code":"7K3QX9MD","plan":{"home":{"ghost":"ghost"}},"peers":[]}]}`,
		`{"v":1,"rooms":[{"code":"7K3QX9MD","relayQ":["ghost"],"peers":[]}]}`} {
		if _, err := m.Restore([]byte(in)); err != ErrSnapshot {
			t.Errorf("Restore(%q) = %v, want ErrSnapshot", in, err)
		}
	}
}
