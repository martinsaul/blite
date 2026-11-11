//go:build windows

package i18n

import (
	"github.com/jeandeaual/go-locale"
	"golang.org/x/sys/windows"
)

// systemLocales are the user's display languages in order of preference
// (Settings > Time & language > Language); the regional format's only
// if Windows doesn't say (Fyne's lang goes by the regional format alone).
func systemLocales() []string {
	if l, err := windows.GetUserPreferredUILanguages(windows.MUI_LANGUAGE_NAME); err == nil && len(l) > 0 {
		return l
	}
	l, _ := locale.GetLocales()
	return l
}
