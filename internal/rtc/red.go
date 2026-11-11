package rtc

import (
	"encoding/binary"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// Audio redundancy: RED (RFC 2198) carries the previous Opus
// frame in each packet, so one lost packet costs nothing. Peers that offer
// audio/red get it; older ones get plain Opus from the same track. The
// frames inside are whatever we send (E2EE-sealed or not); a relay rewraps
// them without opening them.
const (
	mimeRED  = "audio/red"
	opusPT   = 111
	redPT    = 63
	redMaxTS = 1<<14 - 1 // timestamp offset field
	redMaxLn = 1<<10 - 1 // block length field
)

var redCap = webrtc.RTPCodecCapability{MimeType: mimeRED, ClockRate: 48000, Channels: 2, SDPFmtpLine: "111/111"}

// aframe is one Opus frame with its origin's RTP sequence and timestamp.
type aframe struct {
	seq  uint16
	ts   uint32
	data []byte
}

// audioTrack is a local audio track that sends RED to peers that
// negotiated it and plain Opus to the rest. Frames keep their origin's
// sequence numbers and timestamps, so a relay's output recovers like the
// origin's.
type audioTrack struct {
	id       string
	mu       sync.RWMutex
	bindings []aBinding
	prev     aframe // last frame written, the next packet's redundancy
	hasPrev  bool
	seq      uint16 // own track: next sequence number and timestamp
	ts       uint32
}

type aBinding struct {
	id   string
	ssrc uint32
	pt   uint8
	red  bool
	w    webrtc.TrackLocalWriter
}

func newAudioTrack(id string) *audioTrack {
	return &audioTrack{id: id, seq: uint16(rand.Uint32()), ts: rand.Uint32()}
}

func (t *audioTrack) ID() string                { return t.id }
func (t *audioTrack) RID() string               { return "" }
func (t *audioTrack) StreamID() string          { return "blite" }
func (t *audioTrack) Kind() webrtc.RTPCodecType { return webrtc.RTPCodecTypeAudio }

// Bind picks RED if the peer negotiated it, else Opus.
func (t *audioTrack) Bind(c webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	var opus, red *webrtc.RTPCodecParameters
	for _, p := range c.CodecParameters() {
		switch {
		case strings.EqualFold(p.MimeType, mimeRED) && red == nil:
			red = &p
		case strings.EqualFold(p.MimeType, webrtc.MimeTypeOpus) && opus == nil:
			opus = &p
		}
	}
	pick := red
	if pick == nil || opus == nil { // RED needs Opus inside it
		pick = opus
	}
	if pick == nil {
		return webrtc.RTPCodecParameters{}, webrtc.ErrUnsupportedCodec
	}
	t.mu.Lock()
	t.bindings = append(t.bindings, aBinding{id: c.ID(), ssrc: uint32(c.SSRC()), pt: uint8(pick.PayloadType),
		red: pick == red, w: c.WriteStream()})
	t.mu.Unlock()
	return *pick, nil
}

func (t *audioTrack) Unbind(c webrtc.TrackLocalContext) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, b := range t.bindings {
		if b.id == c.ID() {
			// a new slice: write may be ranging over the old one
			t.bindings = append(append([]aBinding(nil), t.bindings[:i]...), t.bindings[i+1:]...)
			return nil
		}
	}
	return webrtc.ErrUnbindFailed
}

// writeOwn sends one of our own frames of duration d.
func (t *audioTrack) writeOwn(data []byte, d time.Duration) {
	t.mu.Lock()
	f := aframe{seq: t.seq, ts: t.ts, data: data}
	t.seq++
	t.ts += uint32(d * 48000 / time.Second)
	t.mu.Unlock()
	t.write(f)
}

// write sends one frame to every binding.
func (t *audioTrack) write(f aframe) {
	t.mu.Lock()
	prev, has := t.prev, t.hasPrev
	t.prev, t.hasPrev = aframe{seq: f.seq, ts: f.ts, data: append([]byte(nil), f.data...)}, true
	bs := t.bindings
	t.mu.Unlock()
	var red []byte
	h := rtp.Header{Version: 2, SequenceNumber: f.seq, Timestamp: f.ts}
	for _, b := range bs {
		h.SSRC, h.PayloadType = b.ssrc, b.pt
		p := f.data
		if b.red {
			if red == nil {
				red = encodeRED(prev, has, f)
			}
			p = red
		}
		b.w.WriteRTP(&h, p)
	}
}

// encodeRED wraps cur, with prev as redundancy when it is cur's immediate
// predecessor and fits the header fields.
func encodeRED(prev aframe, has bool, cur aframe) []byte {
	off := cur.ts - prev.ts
	if !has || prev.seq != cur.seq-1 || off == 0 || off > redMaxTS || len(prev.data) > redMaxLn {
		out := make([]byte, 0, 1+len(cur.data))
		return append(append(out, opusPT), cur.data...)
	}
	out := make([]byte, 5, 5+len(prev.data)+len(cur.data))
	binary.BigEndian.PutUint32(out, 1<<31|opusPT<<24|off<<10|uint32(len(prev.data)))
	out[4] = opusPT
	out = append(out, prev.data...)
	return append(out, cur.data...)
}

// decodeRED splits a RED payload into its redundant blocks (oldest first,
// with their timestamp offsets) and the primary frame.
func decodeRED(p []byte) (blocks []aframe, primary []byte, ok bool) {
	var lens []int
	for {
		if len(p) < 1 {
			return nil, nil, false
		}
		if p[0]&0x80 == 0 {
			p = p[1:]
			break
		}
		if len(p) < 4 {
			return nil, nil, false
		}
		h := binary.BigEndian.Uint32(p)
		blocks = append(blocks, aframe{ts: h >> 10 & redMaxTS})
		lens = append(lens, int(h&redMaxLn))
		p = p[4:]
	}
	for i, n := range lens {
		if n > len(p) {
			return nil, nil, false
		}
		blocks[i].data, p = p[:n], p[n:]
	}
	return blocks, p, true
}

// audioRx turns one incoming audio stream into in-order frames: it drops
// duplicates and late packets and fills a gap from RED redundancy.
type audioRx struct {
	started bool
	seq     uint16
	ts      uint32
}

func (x *audioRx) reset() { x.started = false }

func (x *audioRx) push(p *rtp.Packet) []aframe {
	cur := aframe{seq: p.SequenceNumber, ts: p.Timestamp, data: p.Payload}
	var blocks []aframe
	if p.PayloadType == redPT {
		var ok bool
		if blocks, cur.data, ok = decodeRED(p.Payload); !ok {
			return nil
		}
	}
	d := int16(cur.seq - x.seq)
	if x.started && d <= 0 && d > -1000 {
		return nil // duplicate or too late: the decoder already concealed it
	}
	var out []aframe
	if x.started && d > 1 && d < 1000 {
		for i, b := range blocks {
			b.seq = cur.seq - uint16(len(blocks)-i)
			b.ts = cur.ts - b.ts
			if int16(b.seq-x.seq) > 0 && len(b.data) > 0 {
				out = append(out, b)
			}
		}
	}
	x.started, x.seq, x.ts = true, cur.seq, cur.ts
	return append(out, cur)
}
