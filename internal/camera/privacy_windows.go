package camera

import "golang.org/x/sys/windows/registry"

const consentStore = `Software\Microsoft\Windows\CurrentVersion\CapabilityAccessManager\ConsentStore\webcam`

// CheckPrivacy reads the camera privacy switches from the registry.
func CheckPrivacy() Privacy {
	return Privacy{
		Checked:       true,
		Device:        consent(registry.LOCAL_MACHINE, consentStore),
		DeviceDesktop: consent(registry.LOCAL_MACHINE, consentStore+`\NonPackaged`),
		User:          consent(registry.CURRENT_USER, consentStore),
		UserDesktop:   consent(registry.CURRENT_USER, consentStore+`\NonPackaged`),
	}
}

func consent(root registry.Key, path string) string {
	k, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetStringValue("Value")
	if err != nil {
		return ""
	}
	return v
}
