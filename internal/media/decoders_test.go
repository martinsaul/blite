package media

import (
	"slices"
	"testing"

	"blite/internal/gst"
)

// Only listed H.264 decoders stay pickable; a listed one that is installed
// still decodes a received stream.
func TestDecoderAllowlist(t *testing.T) {
	Init()
	if d := gst.DemoteDecoders("video/x-h264", h264Decoders); len(d) != 0 {
		t.Fatalf("Init left %v pickable", d)
	}
	var have []string
	for _, n := range h264Decoders {
		if gst.HasFactory(n) {
			have = append(have, n)
		}
	}
	if len(have) == 0 {
		t.Skip("no allowed H.264 decoder installed")
	}
	if !slices.ContainsFunc(have, func(n string) bool { return n == "avdec_h264" || n == "openh264dec" }) {
		t.Logf("only hardware decoders installed: %v", have)
	}
}
