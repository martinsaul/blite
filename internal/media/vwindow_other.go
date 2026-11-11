//go:build !linux && !windows

package media

import "errors"

func openVideoWindow(string, int, int) (Window, error) {
	return nil, errors.New("no video window on this platform")
}
