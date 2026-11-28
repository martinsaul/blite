package room

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
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
	a, err := m.Create(bg, pw, "alice", "ka", "", 40, false, ba)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := m.Join(bg, a.Room, pw, "bob", "kb", "", 40, bb)
	c, _ := m.Join(bg, a.Room, pw, "carol", "kc", "", 40, bc)
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
	if _, err := m2.Join(bg, a.Room, "wrong password", "dave", "kd", "", 40, &box{}); err != ErrBadPassword {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := m2.Join(bg, a.Room, pw, "carol", "kc", "", 40, &box{}); err != ErrBanned {
		t.Fatalf("ban lost: %v", err)
	}
	if _, err := m2.Join(bg, a.Room, pw, "dave", "kd", "", 40, &box{}); err != nil {
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
	a, _ := m.Create(bg, pw, "alice", "ka", "", 0, false, ba)
	b, _ := m.Join(bg, a.Room, pw, "bob", "kb", "", 0, bb)
	c, _ := m.Join(bg, a.Room, pw, "carol", "kc", "", 0, bc)
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
	a, _ := src.Create(bg, pw, "alice", "ka", "", 0, false, &box{})
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
		a, _ := m.Create(bg, "", "alice", "ka", "", 0, false, ba)
		m.TieChannel(a.Room, "123456789")
		m.Leave(a.Room, a.PeerID) // room A is empty but alive
		b, _ := m.Create(bg, "", "bob", "kb", "", 0, false, &box{})
		if err := m.TieChannel(b.Room, "123456789"); err != nil {
			t.Fatal(err)
		}
		m.Join(bg, a.Room, "", "carol", "kc", "", 0, &box{}) // A has people again
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
	a, _ := m.Create(bg, pw, "alice", "ka", "", 40, false, &box{})
	m.mu.Lock()
	m.rooms[a.Room].pw = passwordHash{} // an open room: no Argon2 per join
	m.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 200 {
			s, err := m.Join(bg, a.Room, "", "bob", string(rune('a'+i%26))+"k", "", 40, &box{})
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
	a, _ := m.Create(bg, pw, "alice", "ka", "", 0, false, &box{})
	b, _ := m.Join(bg, a.Room, pw, "bob", "kb", "", 0, &box{})
	data, _ := m.Snapshot()

	m2 := NewManager(time.Minute)
	m2.now = func() time.Time { return now }
	m2.Restore(data)
	m2.Resume(a.Room, b.PeerID, b.Resume, &box{})
	now = now.Add(90 * time.Second) // alice's slot is gone, her hold isn't
	m2.Sweep()
	a2, err := m2.Join(bg, a.Room, pw, "alice", "ka", "", 0, &box{})
	if err != nil || a2.Owner != a2.PeerID {
		t.Fatalf("alice rejoined: owner %q err %v, want her new ID %q", a2.Owner, err, a2.PeerID)
	}
}

// Only a file that isn't a snapshot fails Restore.
func TestSnapshotBad(t *testing.T) {
	m := NewManager(time.Minute)
	for _, in := range []string{"", "{", `{"v":2,"rooms":[]}`, `[]`} {
		if _, err := m.Restore([]byte(in)); err != ErrSnapshot {
			t.Errorf("Restore(%q) = %v, want ErrSnapshot", in, err)
		}
	}
}

// A bad room is skipped or repaired, never the whole file: a server that
// starts empty fails every resume.
func TestSnapshotBadRooms(t *testing.T) {
	const peer = `{"id":"p1","name":"a","resume":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`
	for _, tc := range []struct {
		in     string
		loaded int
		check  func(r *Room) string
	}{
		{`{"v":1,"rooms":[{"code":"bad code"}]}`, 0, nil},
		{`{"v":1,"rooms":[{"code":"7K3QX9MD","peers":[]},{"code":"7K3QX9MD","peers":[]}]}`, 1, nil},
		{`{"v":1,"rooms":[{"code":"7K3QX9MD","salt":"AAAA","peers":[]}]}`, 0, nil}, // would be an open room
		{`{"v":1,"rooms":[{"code":"7K3QX9MD","peers":[{"id":"p","resume":"AAAA"},` + peer + `]}]}`, 1, func(r *Room) string {
			if len(r.peers) != 1 || r.peers["p1"] == nil {
				return "unreadable member not dropped alone"
			}
			return ""
		}},
		{`{"v":1,"rooms":[{"code":"7K3QX9MD","owner":"ghost","host":"ghost","peers":[` + peer + `]}]}`, 1, func(r *Room) string {
			if r.host != "" || r.owner != "" {
				return "dangling owner/host not cleared" // the first member back gets the room
			}
			return ""
		}},
		{`{"v":1,"rooms":[{"code":"7K3QX9MD","plan":{"epoch":4,"relays":["ghost"],"home":{"ghost":"ghost"}},"relayQ":["ghost","p1"],"peers":[` + peer + `]}]}`, 1, func(r *Room) string {
			if len(r.plan.Relays) != 0 || len(r.plan.Home) != 0 || r.plan.Epoch != 4 || len(r.relayQ) != 1 || r.relayQ[0] != "p1" {
				return fmt.Sprintf("plan/queue not repaired: %+v %v", r.plan, r.relayQ)
			}
			return ""
		}},
	} {
		m := NewManager(time.Minute)
		n, problems, err := m.RestoreReport([]byte(tc.in))
		if err != nil || n != tc.loaded || len(problems) == 0 {
			t.Errorf("Restore(%s) = %d %v %v, want %d rooms and a problem", tc.in, n, problems, err, tc.loaded)
			continue
		}
		if tc.check != nil {
			if msg := tc.check(m.rooms["7K3QX9MD"]); msg != "" {
				t.Errorf("Restore(%s): %s", tc.in, msg)
			}
		}
	}
}

// restoreAll snapshots m and restores it into a new Manager with the same
// clock, failing on any problem.
func restoreAll(t *testing.T, m *Manager) *Manager {
	t.Helper()
	data, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	m2 := NewManager(m.ResumeGrace)
	m2.now = m.now
	n, problems, err := m2.RestoreReport(data)
	if err != nil || len(problems) > 0 || n != m.Count() {
		t.Fatalf("restore: %d of %d rooms, %v %v", n, m.Count(), problems, err)
	}
	return m2
}

// An owner whose slot ran out during its hold leaves owner naming a gone
// slot. That snapshot restores as it is, and the owner's identity still
// reclaims the room in the new server.
func TestSnapshotDuringOwnerHold(t *testing.T) {
	now := time.Now()
	m := NewManager(time.Minute)
	m.now = func() time.Time { return now }
	ba := &box{}
	a, _ := m.Create(bg, pw, "alice", "ka", "", 0, false, ba)
	b, _ := m.Join(bg, a.Room, pw, "bob", "kb", "", 0, &box{})
	m.Disconnected(a.Room, a.PeerID, ba)
	now = now.Add(90 * time.Second) // her slot is gone, her hold isn't
	m.Sweep()
	if got := ownerOf(m, a.Room); got != a.PeerID {
		t.Fatalf("owner %q, want alice's gone slot (held)", got)
	}

	m2 := restoreAll(t, m)
	if _, err := m2.Resume(a.Room, b.PeerID, b.Resume, &box{}); err != nil {
		t.Fatalf("bob resume: %v", err)
	}
	if got := ownerOf(m2, a.Room); got != a.PeerID {
		t.Fatalf("restored owner %q, want alice (still held)", got)
	}
	a2, err := m2.Join(bg, a.Room, pw, "alice", "ka", "", 0, &box{})
	if err != nil || a2.Owner != a2.PeerID {
		t.Fatalf("alice rejoined: owner %q err %v", a2.Owner, err)
	}

	// An owner without an identity can only resume its slot: once that is
	// gone, the hold ends and the room passes on at once.
	m3 := NewManager(time.Minute)
	m3.now = m.now
	bc := &box{}
	c, _ := m3.Create(bg, pw, "carol", "", "", 0, false, bc)
	d, _ := m3.Join(bg, c.Room, pw, "dave", "kd", "", 0, &box{})
	m3.Disconnected(c.Room, c.PeerID, bc)
	now = now.Add(61 * time.Second)
	m3.Sweep()
	if got := ownerOf(m3, c.Room); got != d.PeerID {
		t.Fatalf("anonymous owner's slot gone: owner %q, want dave", got)
	}
	restoreAll(t, m3)
}

// A hosted room whose host left forgets it as host, and the snapshot
// restores (host stayed set and the restore dropped every room).
func TestSnapshotAfterHostLeft(t *testing.T) {
	m := NewManager(time.Minute)
	h, err := m.CreateFixed(bg, "7K3QX9MD", pw, "relay", "kh", "", 40, false, []string{"ka"}, &box{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := m.Join(bg, h.Room, pw, "bob", "kb", "", 40, &box{})
	m.Leave(h.Room, h.PeerID)
	m.mu.Lock()
	host := m.rooms[h.Room].host
	m.mu.Unlock()
	if host != "" {
		t.Fatalf("host %q after the host left", host)
	}
	if got := ownerOf(m, h.Room); got != b.PeerID {
		t.Fatalf("owner %q, want bob", got)
	}
	m2 := restoreAll(t, m)
	h2, err := m2.CreateFixed(bg, "7K3QX9MD", pw, "relay", "kh", "", 40, false, []string{"ka"}, &box{})
	if err != nil {
		t.Fatal(err)
	}
	m2.mu.Lock()
	host = m2.rooms[h.Room].host
	m2.mu.Unlock()
	if host != h2.PeerID {
		t.Fatalf("the returning relay is not the host: %q", host)
	}
}

// One bad room in a file costs only itself: the other rooms load and their
// members resume.
func TestSnapshotMixed(t *testing.T) {
	m := NewManager(time.Minute)
	a, _ := m.Create(bg, pw, "alice", "ka", "", 0, false, &box{})
	b, _ := m.Create(bg, pw, "bob", "kb", "", 0, false, &box{})
	c, _ := m.Create(bg, pw, "carol", "kc", "", 0, false, &box{})
	data, _ := m.Snapshot()
	var snap map[string]any
	json.Unmarshal(data, &snap)
	rooms := snap["rooms"].([]any)
	for _, r := range rooms {
		r := r.(map[string]any)
		switch r["code"] {
		case b.Room:
			r["relayQ"] = []string{"ghost"} // repairable
		case c.Room:
			r["salt"] = nil // unusable: skipped
		}
	}
	rooms = append(rooms, map[string]any{"code": "not a code"})
	snap["rooms"] = rooms
	data, _ = json.Marshal(snap)

	m2 := NewManager(time.Minute)
	n, problems, err := m2.RestoreReport(data)
	if err != nil || n != 2 || len(problems) != 3 {
		t.Fatalf("restore: %d rooms, %q, %v; want 2 rooms, 3 problems", n, problems, err)
	}
	for _, s := range []Seat{a, b} {
		if _, err := m2.Resume(s.Room, s.PeerID, s.Resume, &box{}); err != nil {
			t.Fatalf("resume in %s: %v", s.Room, err)
		}
	}
	if _, err := m2.Resume(c.Room, c.PeerID, c.Resume, &box{}); err != ErrResumeFailed {
		t.Fatalf("skipped room: %v", err)
	}
}

// A canary room (creator named CanaryPrefix...) is never in a snapshot,
// isn't Served, and is deleted as soon as its last peer leaves.
func TestCanaryRoomEphemeral(t *testing.T) {
	m := NewManager(time.Minute)
	c, err := m.Create(bg, "", CanaryPrefix+"-a", "", "", 0, false, &box{})
	if err != nil {
		t.Fatal(err)
	}
	j, err := m.Join(bg, c.Room, "", CanaryPrefix+"-b", "", "", 0, &box{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(bg, "", "alice", "", "", 0, false, &box{}); err != nil {
		t.Fatal(err)
	}
	data, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if n, err := NewManager(time.Minute).Restore(data); err != nil || n != 1 {
		t.Fatalf("snapshot holds %d rooms (%v), want only alice's", n, err)
	}
	if m.Served() != 1 {
		t.Fatalf("served %d, want 1 (the canary's doesn't count)", m.Served())
	}
	m.Leave(c.Room, c.PeerID)
	m.Leave(j.Room, j.PeerID)
	if m.Count() != 1 {
		t.Fatalf("rooms %d after the canary left, want 1", m.Count())
	}
}

// Reset drops every room and hands back the live connections.
func TestReset(t *testing.T) {
	m := NewManager(time.Minute)
	s, _ := m.Create(bg, "", "alice", "", "", 0, false, &box{})
	m.Join(bg, s.Room, "", "bob", "", "", 0, &box{})
	b, _ := m.Create(bg, "", "carol", "", "", 0, false, &box{})
	m.Disconnected(b.Room, b.PeerID, nil) // no-op: out doesn't match
	rooms, outs := m.Reset()
	if rooms != 2 || len(outs) != 3 || m.Count() != 0 {
		t.Fatalf("reset: %d rooms, %d outs, %d left", rooms, len(outs), m.Count())
	}
}
