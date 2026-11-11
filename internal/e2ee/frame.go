// Package e2ee encrypts media end to end: each sender encrypts
// its encoded frames with its own key, which only room members get, so a
// relay without the keys forwards media it can't read.
//
// Video is encrypted per H.264 NAL unit. The NAL header byte stays in
// clear (relays look for keyframes, and Pion's packetizer groups by NAL
// type); the rest becomes
//
//	kid(4) | counter(8) | AES-128-GCM(payload) | tag(16) | 0x80
//
// with H.264 emulation prevention applied, so no start code appears inside
// it. The trailer keeps the NAL from ending in a zero byte. The header byte,
// kid and counter are authenticated. Audio frames carry the same fields
// without the header byte or the escaping.
package e2ee

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

const (
	KeySize  = 16
	hdrSize  = 4 + 8 // kid, counter
	tagSize  = 16
	trailer  = 0x80
	overhead = hdrSize + tagSize + 1
)

var (
	ErrNoKey    = errors.New("e2ee: no key for this frame")
	ErrBadFrame = errors.New("e2ee: malformed or tampered frame")
)

// SenderKey encrypts our frames. Safe for concurrent use.
type SenderKey struct {
	KID  uint32
	Key  [KeySize]byte
	aead cipher.AEAD
	ctr  atomic.Uint64
}

// NewSenderKey makes a random key. kid must not collide with another
// sender key the room's members hold; callers pick it at random.
func NewSenderKey() *SenderKey {
	var k [KeySize]byte
	rand.Read(k[:])
	var id [4]byte
	rand.Read(id[:])
	return newSenderKey(binary.BigEndian.Uint32(id[:]), k)
}

func newSenderKey(kid uint32, k [KeySize]byte) *SenderKey {
	return &SenderKey{KID: kid, Key: k, aead: newAEAD(k)}
}

func newAEAD(k [KeySize]byte) cipher.AEAD {
	b, _ := aes.NewCipher(k[:])
	a, _ := cipher.NewGCM(b)
	return a
}

// seal appends hdr(kid, counter) | ciphertext | tag of p to dst; prefix is
// authenticated too (the NAL header byte, or nothing).
func (s *SenderKey) seal(dst, prefix, p []byte) []byte {
	n := s.ctr.Add(1)
	var h [hdrSize]byte
	binary.BigEndian.PutUint32(h[:4], s.KID)
	binary.BigEndian.PutUint64(h[4:], n)
	dst = append(dst, h[:]...)
	return s.aead.Seal(dst, nonce(n), p, aad(prefix, h[:]))
}

func nonce(n uint64) []byte {
	var b [12]byte
	binary.BigEndian.PutUint64(b[4:], n)
	return b[:]
}

func aad(prefix, h []byte) []byte {
	return append(append(make([]byte, 0, len(prefix)+len(h)), prefix...), h...)
}

// Keyring holds the keys of the senders we can decrypt.
type Keyring struct {
	mu   sync.RWMutex
	keys map[uint32]*ringKey
}

type ringKey struct {
	aead    cipher.AEAD
	expires time.Time // zero = current
}

func NewKeyring() *Keyring { return &Keyring{keys: map[uint32]*ringKey{}} }

// Add installs a key.
func (r *Keyring) Add(kid uint32, k [KeySize]byte) {
	r.mu.Lock()
	r.keys[kid] = &ringKey{aead: newAEAD(k)}
	r.mu.Unlock()
}

// Retire keeps kid usable for grace (frames still in flight), then drops
// it. It also drops keys whose grace ended.
func (r *Keyring) Retire(kid uint32, grace time.Duration) {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if k := r.keys[kid]; k != nil {
		k.expires = now.Add(grace)
	}
	for id, k := range r.keys {
		if !k.expires.IsZero() && now.After(k.expires) {
			delete(r.keys, id)
		}
	}
}

// Has reports whether kid is usable.
func (r *Keyring) Has(kid uint32) bool { return r.get(kid) != nil }

func (r *Keyring) get(kid uint32) cipher.AEAD {
	r.mu.RLock()
	defer r.mu.RUnlock()
	k := r.keys[kid]
	if k == nil || (!k.expires.IsZero() && time.Now().After(k.expires)) {
		return nil
	}
	return k.aead
}

// open checks and decrypts hdr | ciphertext | tag; prefix as in seal.
func (r *Keyring) open(dst, prefix, b []byte) ([]byte, error) {
	if len(b) < hdrSize+tagSize {
		return nil, ErrBadFrame
	}
	kid := binary.BigEndian.Uint32(b[:4])
	a := r.get(kid)
	if a == nil {
		return nil, ErrNoKey
	}
	out, err := a.Open(dst, nonce(binary.BigEndian.Uint64(b[4:hdrSize])), b[hdrSize:], aad(prefix, b[:hdrSize]))
	if err != nil {
		return nil, ErrBadFrame
	}
	return out, nil
}

// FrameKID is the key id an encrypted audio frame or video access unit
// was sealed with (0, false if it carries none).
func FrameKID(b []byte, video bool) (uint32, bool) {
	if video {
		nals := splitNALs(b)
		if len(nals) == 0 || len(nals[0]) < 1+4 {
			return 0, false
		}
		var h [4]byte
		if n := copy(h[:], unescape(nals[0][1:min(len(nals[0]), 32)])); n < 4 {
			return 0, false
		}
		return binary.BigEndian.Uint32(h[:]), true
	}
	if len(b) < 4 {
		return 0, false
	}
	return binary.BigEndian.Uint32(b[:4]), true
}

// EncryptAudio seals one Opus frame.
func (s *SenderKey) EncryptAudio(p []byte) []byte {
	return s.seal(make([]byte, 0, len(p)+hdrSize+tagSize), nil, p)
}

// DecryptAudio opens one Opus frame.
func (r *Keyring) DecryptAudio(b []byte) ([]byte, error) {
	return r.open(nil, nil, b)
}

var startCode = []byte{0, 0, 0, 1}

// EncryptVideo seals an Annex B access unit NAL by NAL.
func (s *SenderKey) EncryptVideo(au []byte) []byte {
	nals := splitNALs(au)
	out := make([]byte, 0, len(au)+len(nals)*(overhead+8)+len(au)/64)
	for _, n := range nals {
		if len(n) == 0 {
			continue
		}
		body := s.seal(make([]byte, 0, len(n)+overhead), n[:1], n[1:])
		body = append(body, trailer)
		out = append(out, startCode...)
		out = append(out, n[0])
		out = escape(out, body)
	}
	return out
}

// DecryptVideo opens an access unit sealed by EncryptVideo. Any NAL that
// fails fails the whole unit.
func (r *Keyring) DecryptVideo(au []byte) ([]byte, error) {
	nals := splitNALs(au)
	if len(nals) == 0 {
		return nil, ErrBadFrame
	}
	out := make([]byte, 0, len(au))
	for _, n := range nals {
		if len(n) < 2 {
			return nil, ErrBadFrame
		}
		body := unescape(n[1:])
		if len(body) == 0 || body[len(body)-1] != trailer {
			return nil, ErrBadFrame
		}
		out = append(out, startCode...)
		out = append(out, n[0])
		var err error
		if out, err = r.open(out, n[:1], body[:len(body)-1]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// splitNALs returns the NAL units of an Annex B stream (3- or 4-byte start
// codes), without their start codes.
func splitNALs(b []byte) [][]byte {
	var out [][]byte
	i := bytes.Index(b, sc3)
	for i >= 0 {
		start := i + 3
		j := bytes.Index(b[start:], sc3)
		if j < 0 {
			return append(out, b[start:])
		}
		end := start + j
		if end > start && b[end-1] == 0 { // 4-byte start code
			end--
		}
		out = append(out, b[start:end])
		i = start + j
	}
	return out
}

var sc3 = []byte{0, 0, 1}

// escape appends p to dst with emulation prevention: a 0x03 after any two
// zero bytes that are followed by a byte <= 3.
func escape(dst, p []byte) []byte {
	for {
		i := bytes.Index(p, zz)
		if i < 0 || i+2 >= len(p) {
			return append(dst, p...)
		}
		dst = append(dst, p[:i+2]...)
		p = p[i+2:]
		if p[0] <= 3 {
			dst = append(dst, 3)
		}
	}
}

// unescape removes emulation prevention bytes.
func unescape(p []byte) []byte {
	if bytes.Index(p, zz3) < 0 {
		return p
	}
	out := make([]byte, 0, len(p))
	for {
		i := bytes.Index(p, zz3)
		if i < 0 {
			return append(out, p...)
		}
		out = append(out, p[:i+2]...)
		p = p[i+3:]
	}
}

var (
	zz  = []byte{0, 0}
	zz3 = []byte{0, 0, 3}
)
