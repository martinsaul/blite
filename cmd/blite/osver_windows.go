package main

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// osVersion is e.g. "Windows 11 (26100)". Windows 11 still reports 10.0;
// builds from 22000 on are 11.
func osVersion() string {
	v := windows.RtlGetVersion()
	name := fmt.Sprintf("Windows %d.%d", v.MajorVersion, v.MinorVersion)
	if v.MajorVersion == 10 {
		name = "Windows 10"
		if v.BuildNumber >= 22000 {
			name = "Windows 11"
		}
	}
	return fmt.Sprintf("%s (%d)", name, v.BuildNumber)
}
