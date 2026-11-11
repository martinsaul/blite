package rtc

import (
	"bytes"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

func TestREDRoundTrip(t *testing.T) {
	prev := aframe{seq: 9, ts: 1000, data: []byte("prev")}
	cur := aframe{seq: 10, ts: 1960, data: []byte("current")}
	blocks, primary, ok := decodeRED(encodeRED(prev, true, cur))
	if !ok || !bytes.Equal(primary, cur.data) || len(blocks) != 1 || blocks[0].ts != 960 || !bytes.Equal(blocks[0].data, prev.data) {
		t.Fatalf("got %v %q %v", blocks, primary, ok)
	}
	// not the predecessor: primary only
	blocks, primary, ok = decodeRED(encodeRED(aframe{seq: 3, ts: 1000, data: []byte("x")}, true, cur))
	if !ok || len(blocks) != 0 || !bytes.Equal(primary, cur.data) {
		t.Fatalf("got %v %q %v", blocks, primary, ok)
	}
	if _, _, ok := decodeRED([]byte{0x80 | opusPT, 0, 0}); ok {
		t.Fatal("truncated header parsed")
	}
	if _, _, ok := decodeRED([]byte{0x80 | opusPT, 0, 0, 9, opusPT, 1}); ok {
		t.Fatal("block past the end parsed")
	}
}

func TestAudioRxRecovers(t *testing.T) {
	frame := func(i int) aframe { return aframe{seq: uint16(65530 + i), ts: uint32(i * 960), data: []byte{byte(i)}} }
	red := func(i int) *rtp.Packet {
		f := frame(i)
		return &rtp.Packet{Header: rtp.Header{PayloadType: redPT, SequenceNumber: f.seq, Timestamp: f.ts},
			Payload: encodeRED(frame(i-1), i > 0, f)}
	}
	var rx audioRx
	var got []byte
	for _, i := range []int{0, 1, 3, 4, 4, 2, 7, 8} { // 2 lost (late), 5+6 lost
		for _, f := range rx.push(red(i)) {
			if w := frame(int(f.data[0])); f.seq != w.seq || f.ts != w.ts {
				t.Fatalf("frame %d has seq %d ts %d", f.data[0], f.seq, f.ts)
			}
			got = append(got, f.data[0])
		}
	}
	// 2 comes back from 3's redundancy, 6 from 7's; 5 is gone; dup 4 and late 2 dropped
	if want := []byte{0, 1, 2, 3, 4, 6, 7, 8}; !bytes.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// plain Opus passes through
	rx.reset()
	if fs := rx.push(&rtp.Packet{Header: rtp.Header{PayloadType: opusPT, SequenceNumber: 5}, Payload: []byte("o")}); len(fs) != 1 || string(fs[0].data) != "o" {
		t.Fatalf("plain: %v", fs)
	}
}

// TestAudioTrackNegotiates checks a new peer gets RED and an older one
// (Opus only) plain Opus, from the same track.
func TestAudioTrackNegotiates(t *testing.T) {
	for _, tc := range []struct {
		name string
		red  bool
	}{{"new", true}, {"old", false}} {
		t.Run(tc.name, func(t *testing.T) {
			send := newPC(t, true)
			recv := newPC(t, tc.red)
			tr := newAudioTrack(trackAudio)
			if _, err := send.AddTrack(tr); err != nil {
				t.Fatal(err)
			}
			got := make(chan *rtp.Packet, 1)
			recv.OnTrack(func(rt *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
				for {
					p, _, err := rt.ReadRTP()
					if err != nil {
						return
					}
					select {
					case got <- p:
					default:
					}
				}
			})
			connect(t, send, recv)
			deadline := time.After(10 * time.Second)
			for i := 0; ; i++ {
				tr.writeOwn([]byte{1, 2, byte(i)}, 20*time.Millisecond)
				select {
				case p := <-got:
					wantPT := uint8(opusPT)
					if tc.red {
						wantPT = redPT
					}
					if p.PayloadType != wantPT {
						t.Fatalf("payload type %d, want %d", p.PayloadType, wantPT)
					}
					var rx audioRx
					if fs := rx.push(p); len(fs) != 1 || len(fs[0].data) != 3 {
						t.Fatalf("frames %v", fs)
					}
					return
				case <-deadline:
					t.Fatal("no audio")
				case <-time.After(20 * time.Millisecond):
				}
			}
		})
	}
}

func newPC(t *testing.T, red bool) *webrtc.PeerConnection {
	me := &webrtc.MediaEngine{}
	if red {
		if err := registerCodecs(me); err != nil {
			t.Fatal(err)
		}
	} else if err := me.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1"},
		PayloadType: opusPT}, webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatal(err)
	}
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(me)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	return pc
}

func connect(t *testing.T, a, b *webrtc.PeerConnection) {
	offer, err := a.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	done := webrtc.GatheringCompletePromise(a)
	if err := a.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-done
	if err := b.SetRemoteDescription(*a.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	answer, err := b.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	done = webrtc.GatheringCompletePromise(b)
	if err := b.SetLocalDescription(answer); err != nil {
		t.Fatal(err)
	}
	<-done
	if err := a.SetRemoteDescription(*b.LocalDescription()); err != nil {
		t.Fatal(err)
	}
}
