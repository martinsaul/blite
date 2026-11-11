//go:build !linux && !windows

package media

import "errors"

var previewSinks []string

func openPreviewWindow() (previewWindow, error) {
	return nil, errors.New("no preview window on this platform")
}
