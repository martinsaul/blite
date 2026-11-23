package nui

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"

	"blite/internal/i18n"
)

// Updating is the small window shown while an update installs at start-up,
// before the main window. Skip gives up on the update and starts as usual.
type Updating struct {
	u    *UI
	win  fyne.Window
	text *widget.Label
	bar  *widget.ProgressBar
}

// ShowUpdating opens the window (after Run's OnReady). onSkip runs once, on
// the UI goroutine, when the user clicks Skip or closes the window. nil: the
// UI is gone.
func ShowUpdating(onSkip func()) *Updating {
	u := get()
	if u == nil {
		return nil
	}
	w := &Updating{u: u}
	fyne.DoAndWait(func() {
		w.win = u.fa.NewWindow("Bellion Share")
		w.win.SetIcon(u.icon)
		w.win.SetFixedSize(true)
		w.text = widget.NewLabel(i18n.L("Updating Bellion Share..."))
		w.bar = widget.NewProgressBar()
		w.bar.Max = 100
		skipped := false
		skip := func() {
			if !skipped {
				skipped = true
				onSkip()
			}
		}
		w.win.SetCloseIntercept(skip)
		btn := widget.NewButton(i18n.L("Skip"), skip)
		w.win.SetContent(container.NewPadded(container.NewVBox(w.text, w.bar, container.NewCenter(btn))))
		w.win.Resize(fyne.NewSize(360, 0))
		w.win.CenterOnScreen()
		w.win.Show()
	})
	return w
}

// Progress shows how much of the update is downloaded.
func (w *Updating) Progress(pct int) {
	if w == nil {
		return
	}
	w.u.later(func() { w.bar.SetValue(float64(pct)) })
}

// Close closes the window.
func (w *Updating) Close() {
	if w == nil {
		return
	}
	w.u.later(w.win.Close)
}
