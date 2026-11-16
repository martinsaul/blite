package register

import (
	"errors"

	"golang.org/x/sys/windows/registry"
)

// Register maps our schemes to exe under HKCU\Software\Classes (no admin
// needed).
func Register(exe string) error {
	for _, s := range Schemes {
		if err := registerScheme(s, exe); err != nil {
			return err
		}
	}
	return nil
}

func registerScheme(scheme, exe string) error {
	base := `Software\Classes\` + scheme
	k, _, err := registry.CreateKey(registry.CURRENT_USER, base, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.SetStringValue("", "URL:Bellion Share"); err != nil {
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

// Unregister removes our schemes and the sign-in start entry (uninstall).
func Unregister() error {
	var first error
	for _, s := range Schemes {
		for _, k := range []string{`\shell\open\command`, `\shell\open`, `\shell`, ``} {
			if err := registry.DeleteKey(registry.CURRENT_USER, `Software\Classes\`+s+k); err != nil && !errors.Is(err, registry.ErrNotExist) && first == nil {
				first = err
			}
		}
	}
	if err := SetAutostart(false, ""); err != nil && first == nil {
		first = err
	}
	return first
}
