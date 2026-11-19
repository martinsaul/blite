package camera

import (
	"errors"
	"strings"

	"blite/internal/gst"
)

// Reason is why a camera couldn't be opened or stopped.
type Reason int

// Reasons.
const (
	Other  Reason = iota
	Busy          // another app has it
	Denied        // privacy settings (Windows) or device permissions (Linux)
	Gone          // unplugged or no longer listed
)

func (r Reason) String() string {
	switch r {
	case Busy:
		return "busy"
	case Denied:
		return "denied"
	case Gone:
		return "gone"
	}
	return "other"
}

// Codes in error text. GStreamer's Windows sources print HRESULTs as
// 0x%x; strerror text comes from v4l2src.
var (
	busyText = []string{
		"0xc00d3704", // MF_E_HW_MFT_FAILED_START_STREAMING: the camera is in use
		"0xc00d3ea3", // MF_E_VIDEO_RECORDING_DEVICE_PREEMPTED
		"0x800700aa", // HRESULT_FROM_WIN32(ERROR_BUSY)
		"device or resource busy",
		"is busy",
	}
	deniedText = []string{
		"0x80070005", // E_ACCESSDENIED: privacy settings
		"permission denied",
		"access is denied",
	}
	goneText = []string{
		"0xc00d3ea2", // MF_E_VIDEO_RECORDING_DEVICE_INVALIDATED
		"0xc00dabe0", // MF_E_NO_CAPTURE_DEVICES_AVAILABLE
		"0x8007048f", // HRESULT_FROM_WIN32(ERROR_DEVICE_NOT_CONNECTED)
		"0x80070016", // HRESULT_FROM_WIN32(ERROR_BAD_COMMAND): device removed mid-stream
		"no such device",
		"no such file or directory",
		"device not connected",
	}
)

// Classify sorts a camera error: a *gst.BusError by its GError domain and
// code first, then any error by known codes and text. The exact strings
// mfvideosrc posts still have to be collected on Windows hardware.
func Classify(err error) Reason {
	if err == nil {
		return Other
	}
	if errors.Is(err, ErrNotFound) {
		return Gone
	}
	text := strings.ToLower(err.Error())
	var be *gst.BusError
	if errors.As(err, &be) && be.Domain == gst.DomainResource {
		switch be.Code {
		case gst.ResourceBusy:
			return Busy
		case gst.ResourceNotAuthorized:
			return Denied
		case gst.ResourceNotFound:
			return Gone
		case gst.ResourceOpenRead, gst.ResourceOpenReadWrite:
			// v4l2src: "Could not open device ... for reading and writing."
			// with the errno text in the debug string.
			if has(text, deniedText) {
				return Denied
			}
		}
	}
	switch {
	case has(text, busyText):
		return Busy
	case has(text, deniedText):
		return Denied
	case has(text, goneText):
		return Gone
	}
	return Other
}

func has(s string, subs []string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}
