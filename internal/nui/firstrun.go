package nui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"blite/internal/i18n"
)

// firstRun remembers the one-time questions the app asked.
type firstRun struct {
	AutostartAsked bool `json:"autostartAsked"`
}

func firstRunFile() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "blite", "firstrun.json"), nil
}

func loadFirstRun() (firstRun, string) {
	var f firstRun
	p, err := firstRunFile()
	if err != nil {
		return f, ""
	}
	if b, err := os.ReadFile(p); err == nil {
		json.Unmarshal(b, &f)
	}
	return f, p
}

// askName offers, once, to keep or change the random name a new user
// starts with (identity.FriendlyName). Either answer saves a name, so it
// isn't asked again.
func (u *UI) askName() {
	if u.nameAsked || u.st.NameChosen || u.st.Name == "" {
		return
	}
	u.nameAsked = true
	name := widget.NewEntry()
	name.SetText(u.st.Name)
	given := u.st.Name
	d := dialog.NewForm(i18n.L("Your name"), i18n.L("Save"), i18n.L("Cancel"), []*widget.FormItem{
		hinted(widget.NewFormItem(i18n.L("Name"), name), i18n.L("Others see it with your tag (#{{.Tag}}), which comes from your identity key.", i18n.D{"Tag": u.st.Tag})),
	}, func(ok bool) {
		n := given
		if t := strings.TrimSpace(name.Text); ok && t != "" {
			n = t
		}
		u.do(func() error { return u.a.SetName(n) })
	}, u.win)
	d.Resize(fyne.NewSize(460, 0))
	d.Show()
	u.win.Canvas().Focus(name)
}

// askAutostart asks once whether to start at sign-in, where the OS supports
// it and it isn't on already. The answer stays changeable in the Help menu.
func (u *UI) askAutostart() {
	f, p := loadFirstRun()
	if p == "" || f.AutostartAsked || u.st.Autostart == nil {
		return
	}
	f.AutostartAsked = true
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err == nil {
		b, _ := json.Marshal(f)
		os.WriteFile(p, b, 0o644)
	}
	if *u.st.Autostart {
		return
	}
	u.confirm(i18n.L("Start with your computer?"),
		i18n.L("Start Bellion Share in the background when you sign in? You can change this later in Settings."),
		i18n.L("Start automatically"), i18n.L("Not now"), widget.HighImportance,
		func() error { return u.a.SetAutostart(true) })
}
