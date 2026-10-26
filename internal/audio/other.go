//go:build !linux && !windows

package audio

import "errors"

// ErrUnsupported: app audio capture is only implemented for Linux and Windows.
var ErrUnsupported = errors.New("app audio sharing is not available on this OS yet")

// Sharer is a stub until the Windows implementation lands.
type Sharer struct{}

func NewSharer() (*Sharer, error)        { return nil, ErrUnsupported }
func (s *Sharer) Capture() CaptureSource { return "" }
func (s *Sharer) Select(Selection) error { return ErrUnsupported }
func (s *Sharer) Close()                 {}
func ListApps() ([]App, error)           { return nil, ErrUnsupported }
func ListDevices() ([]Device, error)     { return nil, nil }
func PlaybackSink(device string) string  { return "autoaudiosink" }
