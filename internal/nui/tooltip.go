package nui

import (
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
)

// tipDelay is how long the pointer rests on a control before its tooltip shows.
const tipDelay = 450 * time.Millisecond

// tooltips shows one hover tooltip at a time, on a layer over the window's
// content. Fyne has none of its own. The layer holds only a rectangle and
// text, which take no input, so the control under the pointer still gets
// its clicks (a PopUp would swallow the next one). A nil *tooltips is a
// no-op (tests build views without a window layer).
type tooltips struct {
	layer *fyne.Container
	bg    *canvas.Rectangle
	text  *canvas.Text

	mu    sync.Mutex
	owner fyne.CanvasObject // the control whose tip shows or is due
	timer *time.Timer
}

func newTooltips() *tooltips {
	t := &tooltips{bg: canvas.NewRectangle(colElevated), text: canvas.NewText("", colText)}
	t.bg.CornerRadius = 6
	t.bg.StrokeColor = colCardBorder
	t.bg.StrokeWidth = 1
	t.text.TextSize = theme.CaptionTextSize()
	t.layer = container.NewWithoutLayout(t.bg, t.text)
	t.bg.Hide()
	t.text.Hide()
	return t
}

// Show puts text under (or above) obj after the delay; "" hides it.
func (t *tooltips) Show(obj fyne.CanvasObject, text string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
	t.owner = obj
	if text == "" {
		t.mu.Unlock()
		t.hideNow()
		return
	}
	if t.text.Visible() { // moving between controls: no second wait
		t.mu.Unlock()
		t.place(obj, text)
		return
	}
	t.timer = time.AfterFunc(tipDelay, func() {
		fyne.Do(func() {
			t.mu.Lock()
			current := t.owner == obj
			t.mu.Unlock()
			if current {
				t.place(obj, text)
			}
		})
	})
	t.mu.Unlock()
}

// Hide hides obj's tip, if it is the one showing or due.
func (t *tooltips) Hide(obj fyne.CanvasObject) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.owner != obj {
		t.mu.Unlock()
		return
	}
	t.owner = nil
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
	t.mu.Unlock()
	t.hideNow()
}

func (t *tooltips) hideNow() {
	t.bg.Hide()
	t.text.Hide()
	t.layer.Refresh()
}

func (t *tooltips) place(obj fyne.CanvasObject, text string) {
	app := fyne.CurrentApp()
	if app == nil {
		return
	}
	c := app.Driver().CanvasForObject(obj)
	if c == nil {
		return
	}
	t.text.Text = text
	const padX, padY, gap = 8, 4, 4
	ts := t.text.MinSize()
	box := fyne.NewSize(ts.Width+2*padX, ts.Height+2*padY)
	at := app.Driver().AbsolutePositionForObject(obj)
	sz, win := obj.Size(), c.Size()
	x := at.X + (sz.Width-box.Width)/2
	x = max(4, min(x, win.Width-box.Width-4))
	y := at.Y + sz.Height + gap
	if y+box.Height > win.Height-4 { // no room below: above
		y = at.Y - gap - box.Height
	}
	t.bg.Move(fyne.NewPos(x, y))
	t.bg.Resize(box)
	t.text.Move(fyne.NewPos(x+padX, y+padY))
	t.text.Resize(ts)
	t.bg.Show()
	t.text.Show()
	t.layer.Refresh()
}
