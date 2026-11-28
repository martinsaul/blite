package room

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"time"

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

// hashTimeout bounds how long derive waits for a slot in hashSem.
var hashTimeout = 10 * time.Second

type passwordHash struct {
	salt []byte
	key  []byte
}

func hashPassword(ctx context.Context, pw string) (passwordHash, error) {
	if pw == "" {
		return passwordHash{}, nil // an open room: anyone with the code joins
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	key, err := derive(ctx, pw, salt)
	if err != nil {
		return passwordHash{}, err
	}
	return passwordHash{salt: salt, key: key}, nil
}

func (h passwordHash) verify(ctx context.Context, pw string) (bool, error) {
	if h.key == nil {
		return true, nil // open room (older clients still send some password)
	}
	dk, err := derive(ctx, pw, h.salt)
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(dk, h.key) == 1, nil
}

func derive(ctx context.Context, pw string, salt []byte) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, ErrServerBusy
	}
	t := time.NewTimer(hashTimeout)
	defer t.Stop()
	select {
	case hashSem <- struct{}{}:
		defer func() { <-hashSem }()
	case <-ctx.Done():
		return nil, ErrServerBusy
	case <-t.C:
		return nil, ErrServerBusy
	}
	return argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen), nil
}

// validPassword: empty (an open room) or MinPasswordLen..MaxPasswordLen.
func validPassword(pw string) bool {
	return pw == "" || len(pw) >= MinPasswordLen && len(pw) <= MaxPasswordLen
}
