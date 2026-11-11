package nui

import (
	"testing"

	"fyne.io/fyne/v2/lang"

	"blite/internal/i18n"
	"blite/internal/session"
)

// TestLangFyneWords: Fyne's own words follow our language, not the system's.
func TestLangFyneWords(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("LANG", "fr_FR.UTF-8") // the system says French
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_MESSAGES", "")
	t.Setenv("LANGUAGE", "")
	defer i18n.SetPreference("")
	for _, tag := range []string{"ja", "de", "en"} {
		if err := i18n.SetPreference(tag); err != nil {
			t.Fatal(err)
		}
		setFyneLanguage()
		for _, w := range []string{"Cut", "Quit", "(Select one)"} {
			if got, want := lang.L(w), i18n.L(w); got != want {
				t.Errorf("%s: Fyne says %q for %q, we %q", tag, got, w, want)
			}
		}
	}
}

func TestLangSendText(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	i18n.SetPreference("en")
	defer i18n.SetPreference("")
	for _, c := range []struct {
		m    session.Member
		want string
	}{
		{session.Member{}, "-"},
		{session.Member{Layer: "high"}, "Full quality"},
		{session.Member{Layer: "high", Limit: "cpu"}, "Full quality (your CPU)"},
		{session.Member{Layer: "low", Auto: "low"}, "Low quality, low: small tile"},
		{session.Member{Auto: "off"}, "Paused: windows minimized"},
		{session.Member{Layer: "720p", Limit: "odd"}, "720p (odd)"},
	} {
		if got := sendText(c.m); got != c.want {
			t.Errorf("sendText(%+v) = %q, want %q", c.m, got, c.want)
		}
	}
	if got := upperFirst("ünter"); got != "Ünter" {
		t.Error(got)
	}
	if got := upperFirst("画面"); got != "画面" {
		t.Error(got)
	}
}
