package nui

import (
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
)

// quitSpy is a Fyne app that counts Quit calls.
type quitSpy struct {
	fyne.App
	quits int
}

func (q *quitSpy) Quit() { q.quits++ }

// the tray's Quit must be the app's Quit. With a nil Action,
// Fyne fills in the driver's Quit, which terminates GLFW while the settings
// watcher and windows live on.
func TestQuitItemQuitsTheApp(t *testing.T) {
	spy := &quitSpy{App: test.NewApp()}
	defer spy.App.Quit()
	u := &UI{fa: spy}
	q := u.quitItem()
	if !q.IsQuit {
		t.Fatal("quit item isn't marked IsQuit")
	}
	if q.Action == nil {
		t.Fatal("quit item has no Action: Fyne would use the driver's Quit")
	}
	q.Action()
	if spy.quits != 1 {
		t.Fatalf("app Quit called %d times, want 1", spy.quits)
	}
	if !u.closed.Load() {
		t.Fatal("closed isn't set once a quit was asked for")
	}
	q.Action() // a second click while shutting down
	if spy.quits != 1 {
		t.Fatalf("app Quit called %d times after a second click, want 1", spy.quits)
	}
}

// Once a quit was asked for, nothing reaches the driver from other
// goroutines: Show, Quit and later are no-ops.
func TestNoUIWorkAfterQuit(t *testing.T) {
	spy := &quitSpy{App: test.NewApp()}
	defer spy.App.Quit()
	u := &UI{fa: spy}
	curMu.Lock()
	cur = u
	curMu.Unlock()
	defer func() {
		curMu.Lock()
		cur = nil
		curMu.Unlock()
	}()

	Quit()
	if spy.quits != 1 || !u.closed.Load() {
		t.Fatalf("Quit: %d app quits, closed %v", spy.quits, u.closed.Load())
	}
	Quit()
	if spy.quits != 1 {
		t.Fatalf("second Quit quit again (%d)", spy.quits)
	}
	ran := false
	u.later(func() { ran = true })
	Show() // u.win is nil: it would panic if it ran
	if ran {
		t.Fatal("later ran after the quit")
	}
	if ConfirmServer("example.org") {
		t.Fatal("ConfirmServer said yes after the quit")
	}
}
