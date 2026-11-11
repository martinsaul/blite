package register

import (
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
