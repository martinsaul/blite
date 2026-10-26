package main

import (
	"os"
	"strings"
)

// osVersion is the distribution's PRETTY_NAME, e.g. "Linux Mint 22".
func osVersion() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "Linux"
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return "Linux"
}
