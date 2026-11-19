//go:build !linux && !windows

package media

import "errors"

var videoWindowSinks []string

func openVideoWindow(string, int, int) (Window, error) {
	return nil, errors.New("no video window on this platform")
}
