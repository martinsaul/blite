package room

import (
	"crypto/rand"
	"crypto/subtle"

	"golang.org/x/crypto/argon2"
)

// MinPasswordLen is the shortest room password the server accepts.
const MinPasswordLen = 4

// MaxPasswordLen bounds hashing work per request.
const MaxPasswordLen = 128

// Argon2id parameters (RFC 9106 second recommended option, scaled down for a
// 5-person room service). Hashing is also bounded by hashSem below.
const (
	argonTime    = 2
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 1
	argonKeyLen  = 32
	saltLen      = 16
)

// hashSem caps concurrent Argon2 computations so a burst of joins can't
// exhaust memory (each one allocates argonMemory).
var hashSem = make(chan struct{}, 4)

type passwordHash struct {
	salt []byte
	key  []byte
}

func hashPassword(pw string) passwordHash {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return passwordHash{salt: salt, key: derive(pw, salt)}
}

func (h passwordHash) verify(pw string) bool {
	return subtle.ConstantTimeCompare(derive(pw, h.salt), h.key) == 1
}

func derive(pw string, salt []byte) []byte {
	hashSem <- struct{}{}
	defer func() { <-hashSem }()
	return argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
}
