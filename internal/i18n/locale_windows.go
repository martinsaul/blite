//go:build windows

package i18n

import (
	"unsafe"

	"github.com/jeandeaual/go-locale"
	"golang.org/x/sys/windows"
)

var (
	kernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procGetUserDefaultUILang = kernel32.NewProc("GetUserDefaultUILanguage")
	procLCIDToLocaleName     = kernel32.NewProc("LCIDToLocaleName")
)

const (
	localeNameMaxLength     = 85
	localeAllowNeutralNames = 0x08000000
)

// defaultUILanguage asks Windows for the actual display language via
// GetUserDefaultUILanguage -> LCIDToLocaleName. GetUserPreferredUILanguages
// alone can list an input/IME language (e.g. Japanese) above the display
// language.
func defaultUILanguage() string {
	id, _, _ := procGetUserDefaultUILang.Call()
	lcid := uint32(uint16(id))
	if lcid == 0 {
		return ""
	}
	var buf [localeNameMaxLength]uint16
	n, _, _ := procLCIDToLocaleName.Call(
		uintptr(lcid),
		uintptr(unsafe.Pointer(&buf[0])),
		localeNameMaxLength,
		localeAllowNeutralNames,
	)
	if int32(n) <= 0 {
		return ""
	}
	return windows.UTF16ToString(buf[:])
}

// systemLocales puts Windows's actual display language first, then the
// user's preferred UI language list, falling back to go-locale.
func systemLocales() []string {
	ui := defaultUILanguage()
	pref, err := windows.GetUserPreferredUILanguages(windows.MUI_LANGUAGE_NAME)
	if err != nil || len(pref) == 0 {
		pref, _ = locale.GetLocales()
	}
	return mergeLocales(ui, pref)
}
