package e2ee

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"
	"time"
)

// au builds an Annex B access unit from NALs, mixing 3- and 4-byte start
// codes as encoders do.
func au(nals ...[]byte) []byte {
	var b []byte
	for i, n := range nals {
		if i == 0 {
			b = append(b, 0, 0, 0, 1)
		} else {
			b = append(b, 0, 0, 1)
		}
		b = append(b, n...)
	}
	return b
}

func normalize(b []byte) []byte { // to 4-byte start codes, as DecryptVideo returns
	var out []byte
	for _, n := range splitNALs(b) {
		out = append(append(out, startCode...), n...)
	}
	return out
}

func TestVideoRoundTrip(t *testing.T) {
	sk := NewSenderKey()
	ring := NewKeyring()
	ring.Add(sk.KID, sk.Key)
	big := make([]byte, 50000)
	rand.Read(big)
	// Payloads full of zero runs force emulation prevention in the
	// plaintext and, by chance, in the ciphertext.
	zeros := bytes.Repeat([]byte{0, 0, 0, 1, 0, 0, 3, 0, 0, 2}, 300)
	for _, in := range [][]byte{
		au([]byte{0x67, 0x42, 0xe0, 0x1f}, []byte{0x68, 0xce, 0x3c, 0x80}, append([]byte{0x65}, big...)),
		au(append([]byte{0x41}, zeros...)),
		au([]byte{0x09, 0xf0}),
	} {
		enc := sk.EncryptVideo(in)
		// No start code inside a NAL body: exactly one 00 00 01 per NAL.
		if got, want := bytes.Count(enc, []byte{0, 0, 1}), len(splitNALs(in)); got != want {
			t.Fatalf("%d start codes for %d NALs", got, want)
		}
		if got, want := len(splitNALs(enc)), len(splitNALs(in)); got != want {
			t.Fatalf("NAL count %d, want %d", got, want)
		}
		for i, n := range splitNALs(enc) { // header bytes in clear
			if n[0] != splitNALs(in)[i][0] {
				t.Fatalf("NAL %d header %x, want %x", i, n[0], splitNALs(in)[i][0])
			}
		}
		out, err := ring.DecryptVideo(enc)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out, normalize(in)) {
			t.Fatalf("round trip differs (%d vs %d bytes)", len(out), len(normalize(in)))
		}
		if kid, ok := FrameKID(enc, true); !ok || kid != sk.KID {
			t.Fatalf("FrameKID = %x, %v", kid, ok)
		}
	}
}

func TestVideoTamper(t *testing.T) {
	sk := NewSenderKey()
	ring := NewKeyring()
	ring.Add(sk.KID, sk.Key)
	enc := sk.EncryptVideo(au([]byte{0x41, 1, 2, 3, 4, 5}))
	// A relay changing the NAL type (here: making it look like an IDR).
	bad := bytes.Clone(enc)
	bad[4] = 0x65
	if _, err := ring.DecryptVideo(bad); !errors.Is(err, ErrBadFrame) {
		t.Fatalf("changed header: %v", err)
	}
	bad = bytes.Clone(enc)
	bad[len(bad)-5] ^= 1
	if _, err := ring.DecryptVideo(bad); !errors.Is(err, ErrBadFrame) {
		t.Fatalf("flipped bit: %v", err)
	}
	other := NewKeyring()
	if _, err := other.DecryptVideo(enc); !errors.Is(err, ErrNoKey) {
		t.Fatalf("without key: %v", err)
	}
}

func TestAudioAndNonces(t *testing.T) {
	sk := NewSenderKey()
	ring := NewKeyring()
	ring.Add(sk.KID, sk.Key)
	in := []byte("opus frame")
	a, b := sk.EncryptAudio(in), sk.EncryptAudio(in)
	if bytes.Equal(a, b) {
		t.Fatal("same ciphertext twice: nonce reused")
	}
	for _, c := range [][]byte{a, b} {
		if out, err := ring.DecryptAudio(c); err != nil || !bytes.Equal(out, in) {
			t.Fatalf("audio: %q %v", out, err)
		}
	}
	if kid, _ := FrameKID(a, false); kid != sk.KID {
		t.Fatal("audio kid")
	}
}

func TestRetire(t *testing.T) {
	sk := NewSenderKey()
	ring := NewKeyring()
	ring.Add(sk.KID, sk.Key)
	enc := sk.EncryptAudio([]byte("x"))
	ring.Retire(sk.KID, 30*time.Millisecond)
	if _, err := ring.DecryptAudio(enc); err != nil {
		t.Fatal("retired key unusable during grace")
	}
	time.Sleep(40 * time.Millisecond)
	if _, err := ring.DecryptAudio(enc); !errors.Is(err, ErrNoKey) {
		t.Fatalf("after grace: %v", err)
	}
}

func TestExchange(t *testing.T) {
	_, alicePriv, _ := ed25519.GenerateKey(rand.Reader)
	alicePub := base64.StdEncoding.EncodeToString(alicePriv.Public().(ed25519.PublicKey))
	a, b, eve := NewExchange(), NewExchange(), NewExchange()

	sig := SignExchange(alicePriv, "ROOM1", a.Public())
	if !VerifyExchange(alicePub, "ROOM1", a.Public(), sig) {
		t.Fatal("valid exchange key rejected")
	}
	if VerifyExchange(alicePub, "ROOM2", a.Public(), sig) || VerifyExchange(alicePub, "ROOM1", eve.Public(), sig) {
		t.Fatal("signature accepted for another room or key")
	}

	sk := NewSenderKey()
	sealed, err := a.SealKey(b.Public(), sk)
	if err != nil {
		t.Fatal(err)
	}
	kid, key, err := b.OpenKey(a.Public(), sealed)
	if err != nil || kid != sk.KID || key != sk.Key {
		t.Fatalf("open: %x %v", kid, err)
	}
	if _, _, err := eve.OpenKey(a.Public(), sealed); err == nil {
		t.Fatal("a third party opened the key")
	}
	if _, _, err := b.OpenKey(eve.Public(), sealed); err == nil {
		t.Fatal("opened with the wrong sender")
	}
}

func BenchmarkVideo1080pFrame(b *testing.B) {
	sk := NewSenderKey()
	ring := NewKeyring()
	ring.Add(sk.KID, sk.Key)
	p := make([]byte, 20000) // ~5 Mbit/s at 30 fps
	rand.Read(p)
	in := au(append([]byte{0x41}, p...))
	b.SetBytes(int64(len(in)))
	for b.Loop() {
		out, _ := ring.DecryptVideo(sk.EncryptVideo(in))
		_ = out
	}
}
