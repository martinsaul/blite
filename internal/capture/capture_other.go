//go:build !linux && !windows

package capture

import "errors"

// List is not implemented on this OS.
func List() ([]Target, error) { return nil, errors.New("not supported on this OS") }

func windowGone(uint64) bool { return false }
