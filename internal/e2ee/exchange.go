package e2ee

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
)

// Sender keys travel between members sealed to the recipient's exchange
// key: an X25519 key per session, signed by the member's Ed25519 identity
// key for this room, so the signaling server that carries them can't read
// or swap them.

// Exchange is our X25519 key for one session.
type Exchange struct{ priv *ecdh.PrivateKey }

func NewExchange() *Exchange {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return &Exchange{priv: k}
}

// Public is our exchange key, base64.
func (x *Exchange) Public() string {
	return base64.StdEncoding.EncodeToString(x.priv.PublicKey().Bytes())
}

func kxMessage(room, pub string) []byte { return []byte("bshare-kx\x00" + room + "\x00" + pub) }

// SignExchange signs pub (our exchange key) for room with our identity.
func SignExchange(id ed25519.PrivateKey, room, pub string) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(id, kxMessage(room, pub)))
}

// VerifyExchange checks that the member with identity key idKey (base64)
// signed exchange key pub for room.
func VerifyExchange(idKey, room, pub, sig string) bool {
	k, err := base64.StdEncoding.DecodeString(idKey)
	if err != nil || len(k) != ed25519.PublicKeySize {
		return false
	}
	s, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return false
	}
	return ed25519.Verify(k, kxMessage(room, pub), s)
}

var errBadSeal = errors.New("e2ee: can't open sealed key")

// pairAEAD derives the key for messages from sender to recipient (both
// exchange keys, base64) from the X25519 secret between them.
func (x *Exchange) pairAEAD(peer, from, to string) (cipher.AEAD, error) {
	pb, err := base64.StdEncoding.DecodeString(peer)
	if err != nil {
		return nil, err
	}
	pk, err := ecdh.X25519().NewPublicKey(pb)
	if err != nil {
		return nil, err
	}
	secret, err := x.priv.ECDH(pk)
	if err != nil {
		return nil, err
	}
	k, err := hkdf.Key(sha256.New, secret, nil, "bshare-sender-key\x00"+from+"\x00"+to, 16)
	if err != nil {
		return nil, err
	}
	b, _ := aes.NewCipher(k)
	return cipher.NewGCM(b)
}

// Seal encrypts p for the member with exchange key peer.
func (x *Exchange) Seal(peer string, p []byte) (string, error) {
	a, err := x.pairAEAD(peer, x.Public(), peer)
	if err != nil {
		return "", err
	}
	n := make([]byte, a.NonceSize(), a.NonceSize()+len(p)+a.Overhead())
	rand.Read(n)
	return base64.StdEncoding.EncodeToString(a.Seal(n, n, p, nil)), nil
}

// Open decrypts what the member with exchange key peer sealed for us.
func (x *Exchange) Open(peer, sealed string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return nil, errBadSeal
	}
	a, err := x.pairAEAD(peer, peer, x.Public())
	if err != nil {
		return nil, err
	}
	if len(b) < a.NonceSize()+a.Overhead() {
		return nil, errBadSeal
	}
	p, err := a.Open(nil, b[:a.NonceSize()], b[a.NonceSize():], nil)
	if err != nil {
		return nil, errBadSeal
	}
	return p, nil
}

// SealKey and OpenKey carry a sender key: kid(4) | key(16).
func (x *Exchange) SealKey(peer string, k *SenderKey) (string, error) {
	b := make([]byte, 4+KeySize)
	b[0], b[1], b[2], b[3] = byte(k.KID>>24), byte(k.KID>>16), byte(k.KID>>8), byte(k.KID)
	copy(b[4:], k.Key[:])
	return x.Seal(peer, b)
}

func (x *Exchange) OpenKey(peer, sealed string) (kid uint32, key [KeySize]byte, err error) {
	b, err := x.Open(peer, sealed)
	if err != nil {
		return 0, key, err
	}
	if len(b) != 4+KeySize {
		return 0, key, errBadSeal
	}
	copy(key[:], b[4:])
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]), key, nil
}
