package nui

import (
	"encoding/json"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/lang"
	"github.com/jeandeaual/go-locale"

	"blite/internal/i18n"
)

// fyneWords are the texts Fyne's own widgets show (an entry's right-click
// menu, the menus' Quit, an empty drop-down).
var fyneWords = []string{"Cut", "Copy", "Paste", "Select all", "Undo", "Redo", "Quit", "(Select one)", "OK", "Cancel", "Yes", "No", "Error"}

// setFyneLanguage makes Fyne's own texts follow our language. Fyne picks
// its language from the system locale alone, with no way to choose
// another, so we hand it our words under the locales it will pick: the
// system's (as Fyne reads them), and English, its fallback.
func setFyneLanguage() {
	words := map[string]string{}
	for _, w := range fyneWords {
		words[w] = i18n.L(w)
	}
	b, _ := json.Marshal(words)
	locs, _ := locale.GetLocales()
	for _, l := range append(locs, "en") {
		if l != "" {
			lang.AddTranslationsForLocale(b, fyne.Locale(l)) // an odd locale name: Fyne keeps its own words
		}
	}
}

// setLanguage switches the app to tag ("" = the system's) and keeps the
// choice: the window, menus and tray are rebuilt in it at once.
func (u *UI) setLanguage(tag string) {
	if err := i18n.SetPreference(tag); err != nil {
		u.showErr(err)
	}
	setFyneLanguage()
	u.fa.Settings().SetTheme(themeFor()) // also redraws the text in the new fonts
	u.setTrayMenu()
	u.menuKey, u.viewKind = "", ""
	u.refresh()
}
