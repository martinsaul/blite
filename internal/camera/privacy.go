package camera

import "strings"

// Privacy is the Windows camera privacy state ("Camera access", "Let apps
// access your camera", "Let desktop apps access your camera"). Each
// switch is "Allow", "Deny" or "" (not set: allowed).
type Privacy struct {
	Checked bool // false where there are no such settings (Linux)

	Device        string // HKLM ...\ConsentStore\webcam: camera access for this device
	DeviceDesktop string // HKLM ...\webcam\NonPackaged
	User          string // HKCU ...\webcam: "Let apps access your camera"
	UserDesktop   string // HKCU ...\webcam\NonPackaged: "Let desktop apps access your camera"
}

// Blocked reports whether a switch denies desktop apps the camera.
func (p Privacy) Blocked() bool {
	for _, v := range []string{p.Device, p.DeviceDesktop, p.User, p.UserDesktop} {
		if strings.EqualFold(v, "Deny") {
			return true
		}
	}
	return false
}

// SettingsURI opens the Windows camera privacy page.
const SettingsURI = "ms-settings:privacy-webcam"
