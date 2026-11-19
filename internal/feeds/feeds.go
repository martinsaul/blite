// Package feeds models the per-feed viewing state machine for a call.
// Every remote member and our own preview ("self") is a feed; the machine
// decides what shows in the main streaming window, what is popped out into
// its own window, which tile is pinned, and whether media is received.
package feeds

import "slices"

// Self is the feed ID of our own share preview.
const Self = "self"

// State is where one feed currently lives.
type State string

const (
	// Gone: not sharing (haven't started, fade finished, peer left, or call ended).
	Gone State = "gone"
	// InMain: sharing and watched as a normal tile in the main streaming window.
	InMain State = "in_main"
	// PoppedOut: sharing and watched in its own pop-out window.
	PoppedOut State = "popped_out"
	// Pinned: sharing and taking the primary area of the main streaming window.
	Pinned State = "pinned"
	// StoppedFading: remote share just ended; tile says "NAME stopped sharing"
	// in the main window and fades out before becoming Gone.
	StoppedFading State = "stopped_fading"
	// NotWatched: sharing, stopped locally by the eye (or auto-watch off):
	// no video, no audio, no window.
	NotWatched State = "not_watched"
)

// AllStates lists every valid feed State (for tests and invariants).
var AllStates = []State{Gone, InMain, PoppedOut, Pinned, StoppedFading, NotWatched}

// Sharing reports whether the feed's source is currently live.
func (s State) Sharing() bool {
	return s == InMain || s == PoppedOut || s == Pinned || s == NotWatched
}

// Watched reports whether we want media (video and audio) for this feed.
func (s State) Watched() bool {
	return s == InMain || s == PoppedOut || s == Pinned
}

// InMainWindow reports whether this feed has a tile in the main streaming window.
func (s State) InMainWindow() bool {
	return s == InMain || s == Pinned || s == StoppedFading
}

// Eye is the three-state eye indicator shown in the people list and tile overlay.
type Eye string

const (
	EyeOpen   Eye = "open"   // white open eye: sharing and watching
	EyeClosed Eye = "closed" // white closed eye: sharing and not watching
	EyeGrey   Eye = "grey"   // grey eye: not sharing
)

// Eye returns the eye icon state for s.
func (s State) Eye() Eye {
	switch {
	case s.Watched():
		return EyeOpen
	case s == NotWatched:
		return EyeClosed
	default:
		return EyeGrey
	}
}

// Event is an input to the per-feed state machine.
type Event string

const (
	ShareStart   Event = "share_start"
	ShareStop    Event = "share_stop"
	FadeDone     Event = "fade_done"
	EyeToggle    Event = "eye"
	PopOut       Event = "pop_out"
	PopOutClosed Event = "pop_out_closed"
	Pin          Event = "pin"
	Leave        Event = "leave"
)

// AllEvents lists every valid Event (for tests and invariants).
var AllEvents = []Event{ShareStart, ShareStop, FadeDone, EyeToggle, PopOut, PopOutClosed, Pin, Leave}

// Step computes the next state for a single feed given cur and ev.
// self is true for our own preview feed (Self); autoWatch is the
// "Start watching new streams automatically" setting.
func Step(cur State, ev Event, self, autoWatch bool) (next State, started, notifyUnwatched bool) {
	if cur == "" {
		cur = Gone
	}
	switch ev {
	case Leave:
		return Gone, false, false

	case ShareStart:
		if cur.Sharing() {
			return cur, false, false
		}
		if self || autoWatch {
			return InMain, true, false
		}
		return NotWatched, true, true

	case ShareStop:
		if self {
			return Gone, false, false
		}
		if cur.Watched() || cur == StoppedFading {
			return StoppedFading, false, false
		}
		return Gone, false, false

	case FadeDone:
		if cur == StoppedFading {
			return Gone, false, false
		}
		return cur, false, false

	case EyeToggle:
		switch {
		case cur.Watched():
			return NotWatched, false, false
		case cur == NotWatched:
			return InMain, false, false
		case cur == StoppedFading:
			return Gone, false, false
		default:
			return Gone, false, false
		}

	case PopOut:
		switch cur {
		case InMain, Pinned:
			return PoppedOut, false, false
		case PoppedOut:
			return InMain, false, false
		default:
			return cur, false, false
		}

	case PopOutClosed:
		if cur == PoppedOut {
			return InMain, false, false
		}
		return cur, false, false

	case Pin:
		switch cur {
		case InMain, PoppedOut:
			return Pinned, false, false
		case Pinned:
			return InMain, false, false
		default:
			return cur, false, false
		}
	}
	return cur, false, false
}

// Transition describes the result of Machine.Apply.
type Transition struct {
	Prev            State
	Next            State
	Unpinned        string // another feed demoted from Pinned to InMain, if any
	StartedSharing  bool   // transitioned from non-sharing to sharing
	NotifyUnwatched bool   // remote share started unwatched because autoWatch is off
}

// Machine tracks all feeds in one session and maintains cross-feed invariants
// (such as having at most one Pinned feed at a time). Callers synchronize access.
type Machine struct {
	autoWatch bool
	states    map[string]State
}

// New creates a Machine with the initial autoWatch preference.
func New(autoWatch bool) *Machine {
	return &Machine{
		autoWatch: autoWatch,
		states:    map[string]State{},
	}
}

// AutoWatch reports whether new remote shares start watched automatically.
func (m *Machine) AutoWatch() bool { return m.autoWatch }

// SetAutoWatch updates the auto-watch setting for subsequent remote shares.
func (m *Machine) SetAutoWatch(on bool) { m.autoWatch = on }

// State returns feed id's state (Gone when unknown).
func (m *Machine) State(id string) State {
	if st, ok := m.states[id]; ok {
		return st
	}
	return Gone
}

// Eye returns feed id's eye indicator state.
func (m *Machine) Eye(id string) Eye { return m.State(id).Eye() }

// Watched reports whether feed id is currently watched.
func (m *Machine) Watched(id string) bool { return m.State(id).Watched() }

// Sharing reports whether feed id is currently sharing.
func (m *Machine) Sharing(id string) bool { return m.State(id).Sharing() }

// Apply delivers ev to feed id and returns the resulting Transition.
func (m *Machine) Apply(id string, ev Event) Transition {
	if id == "" {
		return Transition{Prev: Gone, Next: Gone}
	}
	prev := m.State(id)
	next, started, notify := Step(prev, ev, id == Self, m.autoWatch)
	tr := Transition{
		Prev:            prev,
		Next:            next,
		StartedSharing:  started,
		NotifyUnwatched: notify,
	}
	if next == Pinned && prev != Pinned {
		for other, st := range m.states {
			if other != id && st == Pinned {
				m.states[other] = InMain
				tr.Unpinned = other
			}
		}
	}
	if next == Gone {
		delete(m.states, id)
	} else {
		m.states[id] = next
	}
	return tr
}

// LeaveAll resets every feed to Gone (when leaving the session).
func (m *Machine) LeaveAll() {
	clear(m.states)
}

// MainOpen reports whether the main streaming window has at least one tile to show.
func (m *Machine) MainOpen() bool {
	for _, st := range m.states {
		if st.InMainWindow() {
			return true
		}
	}
	return false
}

// MainFeeds returns the sorted IDs of feeds shown in the main streaming window.
func (m *Machine) MainFeeds() []string {
	var out []string
	for id, st := range m.states {
		if st.InMainWindow() {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// PinnedFeed returns the ID of the pinned feed, or "" when none is pinned.
func (m *Machine) PinnedFeed() string {
	for id, st := range m.states {
		if st == Pinned {
			return id
		}
	}
	return ""
}

// PopOuts returns the sorted IDs of feeds currently in pop-out windows.
func (m *Machine) PopOuts() []string {
	var out []string
	for id, st := range m.states {
		if st == PoppedOut {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}
