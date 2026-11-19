//go:build !linux && !windows

package capture

import (
	"errors"
	"image"
)

// List is not implemented on this OS.
func List() ([]Target, error) { return nil, errors.New("not supported on this OS") }

func windowGone(uint64) bool { return false }

func windowPID(uint64) int { return 0 }

func windowApp(uint64) string { return "" }

func icons([]Target) map[string]image.Image { return map[string]image.Image{} }
