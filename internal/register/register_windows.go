package register

import (
	"golang.org/x/sys/windows/registry"
)

// Register maps blite:// to exe under HKCU\Software\Classes (no admin needed).
func Register(exe string) error {
	base := `Software\Classes\` + Scheme
	k, _, err := registry.CreateKey(registry.CURRENT_USER, base, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.SetStringValue("", "URL:Blite"); err != nil {
		return err
	}
	if err := k.SetStringValue("URL Protocol", ""); err != nil {
		return err
	}
	cmd, _, err := registry.CreateKey(registry.CURRENT_USER, base+`\shell\open\command`, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer cmd.Close()
	return cmd.SetStringValue("", `"`+exe+`" "%1"`)
}
