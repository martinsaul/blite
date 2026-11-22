package nui

import (
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/test"

	"blite/internal/i18n"
	"blite/internal/session"
)

func TestSpeakerStates(t *testing.T) {
	i18n.SetPreference("en")
	defer i18n.SetPreference("")
	for _, c := range []struct {
		m    session.Member
		res  fyne.Resource
		glow bool
		tip  string
	}{
		{session.Member{Name: "Bob", Heard: true}, speakerWhiteRes, false, "Mute Bob"},
		{session.Member{Name: "Bob", Heard: true, Loud: true}, speakerLoudRes, true, "Mute Bob"},
		{session.Member{Name: "Bob", Heard: true, Loud: true, Muted: true}, speakerMutedRes, false, "Unmute Bob"},
		{session.Member{Name: "Bob", Muted: true}, speakerMutedDimRes, false, "Unmute Bob"},
		{session.Member{Name: "Bob"}, speakerNoneRes, false, "No audio from Bob. Click to mute them anyway."},
	} {
		res, glow := speakerIcon(c.m)
		if res != c.res || glow != c.glow || speakerTip(c.m) != c.tip {
			t.Errorf("%+v: got %s glow %v %q; want %s glow %v %q", c.m, res.Name(), glow, speakerTip(c.m), c.res.Name(), c.glow, c.tip)
		}
	}
}

// The tooltip layer takes no input: a tap where the tip shows still
// reaches the control under it.
func TestTooltipShowsWithoutTakingInput(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	tips := newTooltips()
	tapped := 0
	btn := newIconToggle(tips, musicWhiteRes, func() { tapped++ })
	btn.Set(musicWhiteRes, "Sounds off", false)
	w := test.NewWindow(container.NewStack(container.NewCenter(btn), tips.layer))
	defer w.Close()
	w.Resize(fyne.NewSize(300, 200))

	tips.place(btn, btn.meaning)
	if !tips.text.Visible() || tips.text.Text != "Sounds off" {
		t.Fatalf("tooltip visible %v text %q", tips.text.Visible(), tips.text.Text)
	}
	// Cover the whole window with the tip, then tap the button through it.
	tips.bg.Move(fyne.NewPos(0, 0))
	tips.bg.Resize(fyne.NewSize(300, 200))
	tips.text.Move(fyne.NewPos(0, 0))
	tips.text.Resize(fyne.NewSize(300, 200))
	test.TapCanvas(w.Canvas(), fyne.NewPos(150, 100))
	if tapped != 1 {
		t.Fatalf("taps reaching the button = %d, want 1", tapped)
	}
	tips.Show(btn, "")
	if tips.text.Visible() {
		t.Fatal("Show with no text should hide the tooltip")
	}
	var nilTips *tooltips
	nilTips.Show(btn, "x") // tests build views without a layer
	nilTips.Hide(btn)
}
