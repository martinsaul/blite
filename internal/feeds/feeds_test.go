package feeds

import (
	"math/rand/v2"
	"slices"
	"testing"
)

func TestStatePredicatesAndEyes(t *testing.T) {
	cases := []struct {
		st      State
		sharing bool
		watched bool
		inMain  bool
		eye     Eye
	}{
		{Gone, false, false, false, EyeGrey},
		{InMain, true, true, true, EyeOpen},
		{PoppedOut, true, true, false, EyeOpen},
		{Pinned, true, true, true, EyeOpen},
		{StoppedFading, false, false, true, EyeGrey},
		{NotWatched, true, false, false, EyeClosed},
	}
	for _, c := range cases {
		if got := c.st.Sharing(); got != c.sharing {
			t.Errorf("%s.Sharing() = %v, want %v", c.st, got, c.sharing)
		}
		if got := c.st.Watched(); got != c.watched {
			t.Errorf("%s.Watched() = %v, want %v", c.st, got, c.watched)
		}
		if got := c.st.InMainWindow(); got != c.inMain {
			t.Errorf("%s.InMainWindow() = %v, want %v", c.st, got, c.inMain)
		}
		if got := c.st.Eye(); got != c.eye {
			t.Errorf("%s.Eye() = %s, want %s", c.st, got, c.eye)
		}
	}
}

func TestStepExhaustive(t *testing.T) {
	type row struct {
		cur     State
		ev      Event
		self    bool
		auto    bool
		want    State
		started bool
		notify  bool
	}
	cases := []row{
		// Remote share start, auto-watch on.
		{Gone, ShareStart, false, true, InMain, true, false},
		{StoppedFading, ShareStart, false, true, InMain, true, false},
		{InMain, ShareStart, false, true, InMain, false, false},
		{PoppedOut, ShareStart, false, true, PoppedOut, false, false},
		{Pinned, ShareStart, false, true, Pinned, false, false},
		{NotWatched, ShareStart, false, true, NotWatched, false, false},

		// Remote share start, auto-watch off.
		{Gone, ShareStart, false, false, NotWatched, true, true},
		{StoppedFading, ShareStart, false, false, NotWatched, true, true},
		{NotWatched, ShareStart, false, false, NotWatched, false, false},
		{InMain, ShareStart, false, false, InMain, false, false},

		// Self share start ignores auto-watch=false and starts in main.
		{Gone, ShareStart, true, false, InMain, true, false},
		{NotWatched, ShareStart, true, false, NotWatched, false, false},

		// Share stop: watched remotes fade; unwatched remotes and self go straight to Gone.
		{InMain, ShareStop, false, true, StoppedFading, false, false},
		{Pinned, ShareStop, false, true, StoppedFading, false, false},
		{PoppedOut, ShareStop, false, true, StoppedFading, false, false},
		{StoppedFading, ShareStop, false, true, StoppedFading, false, false},
		{NotWatched, ShareStop, false, true, Gone, false, false},
		{Gone, ShareStop, false, true, Gone, false, false},
		{InMain, ShareStop, true, true, Gone, false, false},
		{Pinned, ShareStop, true, true, Gone, false, false},
		{PoppedOut, ShareStop, true, true, Gone, false, false},
		{NotWatched, ShareStop, true, true, Gone, false, false},

		// FadeDone only finishes StoppedFading; stale timers are no-ops.
		{StoppedFading, FadeDone, false, true, Gone, false, false},
		{InMain, FadeDone, false, true, InMain, false, false},
		{PoppedOut, FadeDone, false, true, PoppedOut, false, false},
		{Pinned, FadeDone, false, true, Pinned, false, false},
		{NotWatched, FadeDone, false, true, NotWatched, false, false},
		{Gone, FadeDone, false, true, Gone, false, false},

		// Eye toggles watching; resuming always goes to InMain.
		{InMain, EyeToggle, false, true, NotWatched, false, false},
		{Pinned, EyeToggle, false, true, NotWatched, false, false},
		{PoppedOut, EyeToggle, false, true, NotWatched, false, false},
		{NotWatched, EyeToggle, false, true, InMain, false, false},
		{StoppedFading, EyeToggle, false, true, Gone, false, false},
		{Gone, EyeToggle, false, true, Gone, false, false},

		// PopOut and PopOutClosed.
		{InMain, PopOut, false, true, PoppedOut, false, false},
		{Pinned, PopOut, false, true, PoppedOut, false, false},
		{PoppedOut, PopOut, false, true, InMain, false, false},
		{NotWatched, PopOut, false, true, NotWatched, false, false},
		{StoppedFading, PopOut, false, true, StoppedFading, false, false},
		{Gone, PopOut, false, true, Gone, false, false},

		{PoppedOut, PopOutClosed, false, true, InMain, false, false},
		{InMain, PopOutClosed, false, true, InMain, false, false},
		{Pinned, PopOutClosed, false, true, Pinned, false, false},
		{NotWatched, PopOutClosed, false, true, NotWatched, false, false},
		{StoppedFading, PopOutClosed, false, true, StoppedFading, false, false},
		{Gone, PopOutClosed, false, true, Gone, false, false},

		// Pin toggles in main and pulls PoppedOut into main as Pinned.
		{InMain, Pin, false, true, Pinned, false, false},
		{PoppedOut, Pin, false, true, Pinned, false, false},
		{Pinned, Pin, false, true, InMain, false, false},
		{NotWatched, Pin, false, true, NotWatched, false, false},
		{StoppedFading, Pin, false, true, StoppedFading, false, false},
		{Gone, Pin, false, true, Gone, false, false},
	}
	for _, c := range cases {
		got, started, notify := Step(c.cur, c.ev, c.self, c.auto)
		if got != c.want || started != c.started || notify != c.notify {
			t.Errorf("Step(%s, %s, self=%v, auto=%v) = (%s, %v, %v), want (%s, %v, %v)",
				c.cur, c.ev, c.self, c.auto, got, started, notify, c.want, c.started, c.notify)
		}
	}
	// Leave from any state goes to Gone.
	for _, st := range AllStates {
		if got, s, n := Step(st, Leave, false, true); got != Gone || s || n {
			t.Errorf("Step(%s, Leave) = (%s, %v, %v), want (Gone, false, false)", st, got, s, n)
		}
	}
}

// TestNoStrandedFeed verifies that no state can strand a feed:
//   - Every transition stays within AllStates.
//   - Any sharing state can reach InMain, PoppedOut, Pinned, and NotWatched
//     using local user controls alone (EyeToggle, PopOut, PopOutClosed, Pin).
//   - Any non-sharing state reaches InMain once sharing starts (plus EyeToggle
//     when autoWatch is off), and reaches Gone via FadeDone or Leave.
func TestNoStrandedFeed(t *testing.T) {
	userEvents := []Event{EyeToggle, PopOut, PopOutClosed, Pin}
	sharingTargets := []State{InMain, PoppedOut, Pinned, NotWatched}

	canReach := func(start, target State, self, auto bool, allowed []Event) bool {
		seen := map[State]bool{start: true}
		q := []State{start}
		for len(q) > 0 {
			cur := q[0]
			q = q[1:]
			if cur == target {
				return true
			}
			for _, ev := range allowed {
				nxt, _, _ := Step(cur, ev, self, auto)
				if !seen[nxt] {
					seen[nxt] = true
					q = append(q, nxt)
				}
			}
		}
		return false
	}

	for _, self := range []bool{false, true} {
		for _, auto := range []bool{false, true} {
			for _, st := range AllStates {
				for _, ev := range AllEvents {
					nxt, _, _ := Step(st, ev, self, auto)
					if !slices.Contains(AllStates, nxt) {
						t.Fatalf("invalid state %q from (%s, %s)", nxt, st, ev)
					}
				}
				if st.Sharing() {
					for _, target := range sharingTargets {
						if !canReach(st, target, self, auto, userEvents) {
							t.Errorf("stranded sharing feed: %s cannot reach %s via user controls (self=%v auto=%v)",
								st, target, self, auto)
						}
					}
				} else {
					if !canReach(st, InMain, self, auto, []Event{ShareStart, EyeToggle}) {
						t.Errorf("stranded idle/fading feed: %s cannot reach InMain (self=%v auto=%v)", st, self, auto)
					}
					if !canReach(st, Gone, self, auto, []Event{FadeDone, Leave}) {
						t.Errorf("%s cannot reach Gone (self=%v auto=%v)", st, self, auto)
					}
				}
			}
		}
	}
}

func TestPopOutNeverLosesStream(t *testing.T) {
	m := New(true)
	m.Apply("bob", ShareStart)
	if m.State("bob") != InMain || !m.MainOpen() {
		t.Fatalf("after ShareStart: state=%s mainOpen=%v", m.State("bob"), m.MainOpen())
	}

	// Pop out the only stream: main closes automatically, pop-out opens.
	m.Apply("bob", PopOut)
	if m.State("bob") != PoppedOut || m.MainOpen() || !slices.Equal(m.PopOuts(), []string{"bob"}) {
		t.Fatalf("after PopOut: state=%s mainOpen=%v popOuts=%v", m.State("bob"), m.MainOpen(), m.PopOuts())
	}

	// Closing the pop-out window returns Bob to the main window (reopening it).
	m.Apply("bob", PopOutClosed)
	if m.State("bob") != InMain || !m.MainOpen() || len(m.PopOuts()) != 0 {
		t.Fatalf("after PopOutClosed: state=%s mainOpen=%v popOuts=%v", m.State("bob"), m.MainOpen(), m.PopOuts())
	}

	// Clicking eye on a popped-out stream closes pop-out and stops watching;
	// turning eye back on puts Bob in main (never back in a pop-out).
	m.Apply("bob", PopOut)
	m.Apply("bob", EyeToggle)
	if m.State("bob") != NotWatched || m.Watched("bob") || m.Eye("bob") != EyeClosed || len(m.PopOuts()) != 0 {
		t.Fatalf("after EyeToggle on pop-out: state=%s eye=%s popOuts=%v", m.State("bob"), m.Eye("bob"), m.PopOuts())
	}
	// Late window-close callback after EyeToggle must not resurrect Bob.
	m.Apply("bob", PopOutClosed)
	if m.State("bob") != NotWatched {
		t.Fatalf("late PopOutClosed resurrected NotWatched feed to %s", m.State("bob"))
	}
	m.Apply("bob", EyeToggle)
	if m.State("bob") != InMain || m.Eye("bob") != EyeOpen || len(m.PopOuts()) != 0 {
		t.Fatalf("re-watching after pop-out should open in main: state=%s popOuts=%v", m.State("bob"), m.PopOuts())
	}
}

func TestPinExclusivity(t *testing.T) {
	m := New(true)
	m.Apply("alice", ShareStart)
	m.Apply("bob", ShareStart)
	m.Apply("carol", ShareStart)
	m.Apply("carol", PopOut)

	tr := m.Apply("alice", Pin)
	if tr.Next != Pinned || tr.Unpinned != "" || m.PinnedFeed() != "alice" {
		t.Fatalf("pin alice: %+v pinned=%s", tr, m.PinnedFeed())
	}

	// Pinning Bob unpins Alice back to InMain.
	tr = m.Apply("bob", Pin)
	if tr.Next != Pinned || tr.Unpinned != "alice" || m.State("alice") != InMain || m.PinnedFeed() != "bob" {
		t.Fatalf("pin bob: %+v alice=%s pinned=%s", tr, m.State("alice"), m.PinnedFeed())
	}

	// Pinning Carol from PoppedOut brings Carol into main as Pinned and unpins Bob.
	tr = m.Apply("carol", Pin)
	if tr.Next != Pinned || tr.Unpinned != "bob" || m.State("bob") != InMain || m.PinnedFeed() != "carol" || len(m.PopOuts()) != 0 {
		t.Fatalf("pin carol from pop-out: %+v bob=%s pinned=%s popOuts=%v", tr, m.State("bob"), m.PinnedFeed(), m.PopOuts())
	}

	// Attempting to pin a Gone or NotWatched feed must not unpin Carol.
	m.Apply("alice", EyeToggle) // NotWatched
	if tr := m.Apply("alice", Pin); tr.Next != NotWatched || m.PinnedFeed() != "carol" {
		t.Fatalf("pin NotWatched feed broke Carol's pin: %+v pinned=%s", tr, m.PinnedFeed())
	}
	if tr := m.Apply("nobody", Pin); tr.Next != Gone || m.PinnedFeed() != "carol" {
		t.Fatalf("pin Gone feed broke Carol's pin: %+v pinned=%s", tr, m.PinnedFeed())
	}

	// Clicking Pin again on Carol unpins her back to InMain.
	m.Apply("carol", Pin)
	if m.State("carol") != InMain || m.PinnedFeed() != "" {
		t.Fatalf("unpin carol: state=%s pinned=%s", m.State("carol"), m.PinnedFeed())
	}
}

func TestSelfPreviewAndMainWindowAutoClose(t *testing.T) {
	m := New(false) // even with auto-watch off, our own share preview starts on
	tr := m.Apply(Self, ShareStart)
	if tr.Next != InMain || !tr.StartedSharing || tr.NotifyUnwatched {
		t.Fatalf("self ShareStart: %+v", tr)
	}
	if !m.MainOpen() || !slices.Equal(m.MainFeeds(), []string{Self}) {
		t.Fatalf("main should stay open with self preview: open=%v feeds=%v", m.MainOpen(), m.MainFeeds())
	}

	// Remote share with auto-watch off stays NotWatched and asks for a control-window note.
	tr = m.Apply("bob", ShareStart)
	if tr.Next != NotWatched || !tr.StartedSharing || !tr.NotifyUnwatched || m.Eye("bob") != EyeClosed {
		t.Fatalf("bob ShareStart with autoWatch=false: %+v eye=%s", tr, m.Eye("bob"))
	}
	// Duplicate ShareStart (e.g. resolution change) does not re-trigger notification.
	if tr = m.Apply("bob", ShareStart); tr.StartedSharing || tr.NotifyUnwatched {
		t.Fatalf("duplicate ShareStart triggered start/notify: %+v", tr)
	}

	// Hiding our own preview leaves nothing watched: main window auto-closes.
	m.Apply(Self, EyeToggle)
	if m.State(Self) != NotWatched || m.MainOpen() || m.Eye(Self) != EyeClosed {
		t.Fatalf("after hiding self preview: state=%s open=%v eye=%s", m.State(Self), m.MainOpen(), m.Eye(Self))
	}

	// Stopping our own share goes straight to Gone (no stopped-fading tile).
	m.Apply(Self, EyeToggle) // back to InMain
	m.Apply(Self, ShareStop)
	if m.State(Self) != Gone || m.MainOpen() || m.Eye(Self) != EyeGrey {
		t.Fatalf("after self ShareStop: state=%s open=%v eye=%s", m.State(Self), m.MainOpen(), m.Eye(Self))
	}

	// Unwatched Bob stopping also goes straight to Gone without opening main for a fade.
	m.Apply("bob", ShareStop)
	if m.State("bob") != Gone || m.MainOpen() || m.Eye("bob") != EyeGrey {
		t.Fatalf("unwatched ShareStop: state=%s open=%v eye=%s", m.State("bob"), m.MainOpen(), m.Eye("bob"))
	}
}

func TestStoppedFadingAndReshareRace(t *testing.T) {
	m := New(true)
	m.Apply("bob", ShareStart)
	m.Apply("bob", Pin)
	m.Apply("bob", ShareStop)

	// Stopping unpins Bob into StoppedFading; main stays open while fading, media is off.
	if m.State("bob") != StoppedFading || m.PinnedFeed() != "" || !m.MainOpen() || m.Watched("bob") || m.Eye("bob") != EyeGrey {
		t.Fatalf("StoppedFading state mismatch: state=%s pinned=%s open=%v watched=%v eye=%s",
			m.State("bob"), m.PinnedFeed(), m.MainOpen(), m.Watched("bob"), m.Eye("bob"))
	}

	// Bob shares again before FadeDone fires: returns to InMain, and stale FadeDone is ignored.
	tr := m.Apply("bob", ShareStart)
	if tr.Next != InMain || !tr.StartedSharing {
		t.Fatalf("reshare during fade: %+v", tr)
	}
	m.Apply("bob", FadeDone)
	if m.State("bob") != InMain {
		t.Fatalf("stale FadeDone clobbered live share: %s", m.State("bob"))
	}

	// Normal fade completion drops tile and closes main window.
	m.Apply("bob", ShareStop)
	m.Apply("bob", FadeDone)
	if m.State("bob") != Gone || m.MainOpen() {
		t.Fatalf("after FadeDone: state=%s open=%v", m.State("bob"), m.MainOpen())
	}

	// Peer leaving during StoppedFading drops tile immediately.
	m.Apply("bob", ShareStart)
	m.Apply("bob", ShareStop)
	m.Apply("bob", Leave)
	if m.State("bob") != Gone || m.MainOpen() {
		t.Fatalf("after Leave during fade: state=%s open=%v", m.State("bob"), m.MainOpen())
	}
}

func TestRandomMultiFeedInvariants(t *testing.T) {
	rng := rand.New(rand.NewPCG(42, 7))
	m := New(true)
	ids := []string{Self, "a", "b", "c", "d"}

	for step := range 10000 {
		if rng.IntN(25) == 0 {
			m.SetAutoWatch(!m.AutoWatch())
		}
		if rng.IntN(200) == 0 {
			m.LeaveAll()
			if m.MainOpen() || len(m.PopOuts()) != 0 || m.PinnedFeed() != "" || len(m.states) != 0 {
				t.Fatalf("step %d: LeaveAll left state %+v", step, m.states)
			}
			continue
		}
		id := ids[rng.IntN(len(ids))]
		ev := AllEvents[rng.IntN(len(AllEvents))]
		m.Apply(id, ev)

		pinned := 0
		for k, st := range m.states {
			if st == Gone {
				t.Fatalf("step %d: Gone stored in map for %s", step, k)
			}
			if !slices.Contains(AllStates, st) {
				t.Fatalf("step %d: invalid state %q for %s", step, st, k)
			}
			if st == Pinned {
				pinned++
			}
		}
		if pinned > 1 {
			t.Fatalf("step %d: %d feeds pinned: %+v", step, pinned, m.states)
		}
		main := m.MainFeeds()
		if m.MainOpen() != (len(main) > 0) {
			t.Fatalf("step %d: MainOpen=%v main=%v", step, m.MainOpen(), main)
		}
		for _, p := range m.PopOuts() {
			if slices.Contains(main, p) {
				t.Fatalf("step %d: feed %s in both MainFeeds and PopOuts", step, p)
			}
		}
	}
}
