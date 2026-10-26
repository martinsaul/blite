// Package client is the Molt update client: it fetches a signed release
// manifest, verifies it against baked-in Ed25519 keys, downloads and verifies
// the payload, and hands it to molt-swap. It is stdlib-only on purpose so apps
// can vendor it by copying the directory.
package client

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// SchemaVersion is the manifest payload schema this client understands.
const SchemaVersion = 1

// Payload is the signed body of a manifest. The server signs the exact bytes
// it serializes; clients never re-serialize before verifying.
type Payload struct {
	Schema            int       `json:"schema"`
	Package           string    `json:"package"`
	Channel           string    `json:"channel"` // "" for a build-pinned release manifest
	Platform          string    `json:"platform"`
	Version           string    `json:"version"`
	Build             int64     `json:"build"`
	Kind              string    `json:"kind"` // zip | deb | exe | bin
	Filename          string    `json:"filename"`
	SHA256            string    `json:"sha256"`
	Size              int64     `json:"size"`
	URL               string    `json:"url"`
	ReleasedAt        time.Time `json:"releasedAt"`
	Notes             string    `json:"notes,omitempty"`
	MinSupportedBuild int64     `json:"minSupportedBuild"`
	IssuedAt          time.Time `json:"issuedAt"`
	ExpiresAt         time.Time `json:"expiresAt"`
}

// Signature is one Ed25519 signature over the raw payload bytes.
type Signature struct {
	KeyID string `json:"keyId"`
	Sig   string `json:"sig"`
}

// Envelope is the wire format of a manifest.
type Envelope struct {
	Payload    string      `json:"payload"` // base64 (std) of the UTF-8 JSON payload bytes
	Signatures []Signature `json:"signatures"`
}

// KeyID is the first 8 bytes (hex) of sha256 over the raw 32-byte public key.
func KeyID(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:8])
}

// ParsePublicKey decodes a base64 (std or url, padded or not) 32-byte Ed25519 public key.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := decodeB64(s)
	if err != nil {
		return nil, fmt.Errorf("molt: public key: %w", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("molt: public key: want %d bytes, got %d", ed25519.PublicKeySize, len(b))
	}
	return ed25519.PublicKey(b), nil
}

// Sign builds an envelope over payloadBytes, signed by every key given.
// Used by the server; exported so tests and tools share one implementation.
func Sign(payloadBytes []byte, keys ...ed25519.PrivateKey) Envelope {
	env := Envelope{Payload: base64.StdEncoding.EncodeToString(payloadBytes)}
	for _, k := range keys {
		pub := k.Public().(ed25519.PublicKey)
		env.Signatures = append(env.Signatures, Signature{
			KeyID: KeyID(pub),
			Sig:   base64.StdEncoding.EncodeToString(ed25519.Sign(k, payloadBytes)),
		})
	}
	return env
}

// Verify checks that at least one signature in env verifies against at least
// one trusted key, then decodes the payload. It does NOT check expiry or
// identity fields; see Payload.Check.
func Verify(env Envelope, trusted []ed25519.PublicKey) (Payload, error) {
	var p Payload
	raw, err := base64.StdEncoding.DecodeString(env.Payload)
	if err != nil {
		return p, fmt.Errorf("%w: payload encoding: %v", ErrBadSignature, err)
	}
	if !verifyAny(raw, env.Signatures, trusted) {
		return p, fmt.Errorf("%w: no signature verified against a trusted key", ErrBadSignature)
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, fmt.Errorf("%w: payload json: %v", ErrBadSignature, err)
	}
	if p.Schema != SchemaVersion {
		return p, fmt.Errorf("%w: unsupported schema %d", ErrCouldNotCheck, p.Schema)
	}
	return p, nil
}

func verifyAny(raw []byte, sigs []Signature, trusted []ed25519.PublicKey) bool {
	for _, s := range sigs {
		sig, err := base64.StdEncoding.DecodeString(s.Sig)
		if err != nil || len(sig) != ed25519.SignatureSize {
			continue
		}
		for _, k := range trusted {
			if len(k) == ed25519.PublicKeySize && ed25519.Verify(k, raw, sig) {
				return true
			}
		}
	}
	return false
}

// Check enforces the mix-and-match guard (the signed identity must equal what
// was asked for) and the freshness window. channel "" means build-pinned.
func (p Payload) Check(pkg, channel, platform string, now time.Time) error {
	if p.Package != pkg || p.Channel != channel || p.Platform != platform {
		return fmt.Errorf("%w: manifest is for %s/%s/%s, asked for %s/%s/%s",
			ErrBadSignature, p.Package, p.Channel, p.Platform, pkg, channel, platform)
	}
	if p.ExpiresAt.IsZero() || now.After(p.ExpiresAt) {
		return fmt.Errorf("%w: manifest expired at %s", ErrCouldNotCheck, p.ExpiresAt.Format(time.RFC3339))
	}
	if len(p.SHA256) != 64 || p.Size <= 0 {
		return fmt.Errorf("%w: manifest missing sha256/size", ErrBadSignature)
	}
	return nil
}

func decodeB64(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("not base64")
}
