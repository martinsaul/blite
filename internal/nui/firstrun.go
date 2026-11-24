package nui

import (
	"encoding/json"
	"os"
	"path/filepath"

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
