//go:build !windows

package i18n

import "github.com/jeandeaual/go-locale"

// systemLocales are the user's locales: LANGUAGE (a list), LC_ALL,
// LC_MESSAGES or LANG on Linux.
func systemLocales() []string {
	l, _ := locale.GetLocales()
	return l
}
