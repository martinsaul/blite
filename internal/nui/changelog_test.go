package nui

import (
	"slices"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"

	"blite/internal/i18n"
	"blite/internal/update"
)

// texts lists the label and check box texts under o.
func texts(o fyne.CanvasObject) []string {
	var out []string
	var walk func(fyne.CanvasObject)
	walk = func(o fyne.CanvasObject) {
		switch w := o.(type) {
		case *widget.Label:
			out = append(out, w.Text)
		case *widget.Check:
			out = append(out, w.Text)
		case *fyne.Container:
			for _, c := range w.Objects {
				walk(c)
			}
		case fyne.Widget:
			for _, c := range test.WidgetRenderer(w).Objects() {
				walk(c)
			}
		}
	}
	walk(o)
	return out
}

func TestUpdateChanges(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	i18n.SetPreference("en")
	defer i18n.SetPreference("")

	a := test.NewApp()
	defer a.Quit()
	a.Settings().SetTheme(themeFor())
	st := demoState(false)
	st.Update = &update.Info{Result: "Updated to build 211.", UpdatedFrom: 208, UpdatedTo: 211}
	be := &fakeBackend{st: st}
	u := &UI{a: be, fa: a, win: test.NewWindow(nil)}
	defer func() {
		u.closed.Store(true)
		u.win.Close()
	}()
	u.view = container.NewStack()
	u.toasts = container.New(&cornerLayout{w: 400, pad: 16})
	u.win.SetContent(container.NewStack(u.view, u.toasts))
	u.refresh()

	// After an update: the changes since the build it replaced, not before.
	top := u.win.Canvas().Overlays().Top()
	if top == nil {
		t.Fatal("no changes panel after an update")
	}
	got := texts(top)
	for _, want := range []string{"Build 211 (2026-11-22)", "Build 210 (2026-11-21)", "Don't show change logs again"} {
		if !slices.Contains(got, want) {
			t.Errorf("panel lacks %q: %q", want, got)
		}
	}
	if slices.Contains(got, "Build 208 (2026-11-20)") {
		t.Error("panel shows the build we updated from")
	}
	u.win.Canvas().Overlays().Remove(top)
	u.refresh() // the same result again: not shown twice
	if u.win.Canvas().Overlays().Top() != nil {
		t.Error("the update result showed twice")
	}

	// A new update: a notice in the corner, once.
	be.mu.Lock()
	be.st.Update = &update.Info{Available: "0.1.0 (213)", Notes: "Faster joins."}
	be.mu.Unlock()
	u.refresh()
	u.refresh()
	if len(u.toasts.Objects) != 1 || !slices.Contains(texts(u.toasts), "Faster joins.") {
		t.Fatalf("update notice: %d objects, %q", len(u.toasts.Objects), texts(u.toasts))
	}

	// Change logs turned off: no notice for the next update.
	u.toasts.RemoveAll()
	be.mu.Lock()
	be.st.HideChangelog = true
	be.st.Update = &update.Info{Available: "0.1.0 (214)"}
	be.mu.Unlock()
	u.refresh()
	if len(u.toasts.Objects) != 0 {
		t.Error("update notice with change logs turned off")
	}
}

func TestCornerLayout(t *testing.T) {
	l := &cornerLayout{w: 400, pad: 16}
	a := widget.NewLabel("a")
	l.Layout([]fyne.CanvasObject{a}, fyne.NewSize(1000, 700))
	if p := a.Position(); p.X != 1000-16-400 || p.Y+a.Size().Height != 700-16 {
		t.Errorf("at %v size %v", p, a.Size())
	}
	l.Layout([]fyne.CanvasObject{a}, fyne.NewSize(300, 700))
	if w := a.Size().Width; w != 300-32 {
		t.Errorf("narrow window: width %v", w)
	}
}
