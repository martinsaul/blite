package rtc

import (
	"testing"

	"github.com/pion/rtp"
)

// A 4K keyframe of 1000 packets (1.2 MB) comes out whole: the old 512-packet
// limit dropped its start, so the key gate never saw the IDR.
func TestVideoSampleBuilderBigKeyframe(t *testing.T) {
	sb := newVideoSampleBuilder(90000)
	const n, size = 1000, 1188
	push := func(seq uint16, ts uint32, payload []byte, marker bool) {
		sb.Push(&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts, Marker: marker, PayloadType: 96}, Payload: payload})
	}
	// FU-A fragments of one IDR NAL (type 5), then a small P frame to close it.
	for i := range n {
		fu := make([]byte, 2+size)
		fu[0] = 0x7c // NRI 3, type 28 (FU-A)
		fu[1] = 5
		if i == 0 {
			fu[1] |= 0x80 // start
		}
		if i == n-1 {
			fu[1] |= 0x40 // end
		}
		push(uint16(1000+i), 3000, fu, i == n-1)
	}
	push(uint16(1000+n), 6000, []byte{0x41, 0x9a, 0x00}, true)
	s := sb.Pop()
	if s == nil {
		t.Fatal("the keyframe never came out")
	}
	if !HasIDR(s.Data) {
		t.Fatal("no IDR in the frame")
	}
	if want := n * size; len(s.Data) < want {
		t.Fatalf("frame of %d bytes, want at least %d", len(s.Data), want)
	}
}
