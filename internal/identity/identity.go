// Package identity is Blite's device identity: a long-lived Ed25519 key that
// signs each call's DTLS fingerprints, so a malicious or compromised signaling
// server can't swap them and sit in the middle unnoticed. Peers trust a key on
// first use per display name and can compare a short safety code aloud.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Key is this device's identity.
type Key struct {
	Private ed25519.PrivateKey
}

// Public returns the base64 public key sent to peers as idKey.
func (k Key) Public() string {
	return base64.StdEncoding.EncodeToString(k.Private.Public().(ed25519.PublicKey))
}

func dir() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "blite"), nil
}

// LoadOrCreate reads the key from the user's config dir (0600 file), creating it
// on first run. POC: a file, not the OS keystore.
func LoadOrCreate() (Key, error) {
	d, err := dir()
	if err != nil {
		return Key{}, err
	}
	p := filepath.Join(d, "identity.key")
	if b, err := os.ReadFile(p); err == nil {
		seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(seed) != ed25519.SeedSize {
			return Key{}, fmt.Errorf("corrupt identity key %s", p)
		}
		return Key{Private: ed25519.NewKeyFromSeed(seed)}, nil
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Key{}, err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return Key{}, err
	}
	if err := os.WriteFile(p, []byte(base64.StdEncoding.EncodeToString(priv.Seed())+"\n"), 0o600); err != nil {
		return Key{}, err
	}
	return Key{Private: priv}, nil
}

// Ephemeral returns a throwaway key (headless test peers).
func Ephemeral() Key {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	return Key{Private: priv}
}

// Fingerprints extracts the a=fingerprint values from an SDP, sorted.
func Fingerprints(sdp string) []string {
	var fps []string
	for _, line := range strings.Split(sdp, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "a=fingerprint:"); ok {
			fps = append(fps, strings.ToLower(v))
		}
	}
	sort.Strings(fps)
	return fps
}

func message(room, from, to string, fps []string) []byte {
	return []byte("blite-v0|" + room + "|" + from + "|" + to + "|" + strings.Join(fps, ","))
}

// SignSDP signs the fingerprints in sdp for one direction of one peer pair.
func (k Key) SignSDP(room, from, to, sdp string) []byte {
	return ed25519.Sign(k.Private, message(room, from, to, Fingerprints(sdp)))
}

// ErrBadSignature means the SDP's fingerprints weren't signed by the sender's
// key: someone in the middle (most likely the signaling server) changed them.
var ErrBadSignature = errors.New("DTLS fingerprint not signed by this peer's key")

// VerifySDP checks sig over sdp's fingerprints against the sender's idKey.
func VerifySDP(idKey, room, from, to, sdp string, sig []byte) error {
	pub, err := base64.StdEncoding.DecodeString(idKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return errors.New("peer has no valid identity key")
	}
	fps := Fingerprints(sdp)
	if len(fps) == 0 {
		return errors.New("SDP has no DTLS fingerprint")
	}
	if !ed25519.Verify(pub, message(room, from, to, fps), sig) {
		return ErrBadSignature
	}
	return nil
}

// SafetyCode derives a code everyone in the room sees identically if nobody
// swapped keys: SHA-256 over the sorted idKeys, shown as 5 groups of 4 digits.
func SafetyCode(idKeys []string) string {
	ks := append([]string(nil), idKeys...)
	sort.Strings(ks)
	sum := sha256.Sum256([]byte("blite-safety-v0|" + strings.Join(ks, "|")))
	groups := make([]string, 5)
	for i := range groups {
		n := binary.BigEndian.Uint32(sum[i*4:]) % 10000
		groups[i] = fmt.Sprintf("%04d", n)
	}
	return strings.Join(groups, " ")
}

// Known is the trust-on-first-use store: display name -> idKey.
type Known struct {
	mu   sync.Mutex
	path string
	m    map[string]string
}

// LoadKnown reads known_peers.json from the config dir (missing = empty).
func LoadKnown() *Known {
	k := &Known{m: map[string]string{}}
	if d, err := dir(); err == nil {
		k.path = filepath.Join(d, "known_peers.json")
		if b, err := os.ReadFile(k.path); err == nil {
			json.Unmarshal(b, &k.m)
		}
	}
	return k
}

// Check records name -> idKey on first sight and reports whether a previously
// seen name now has a different key.
func (k *Known) Check(name, idKey string) (changed bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if old, ok := k.m[name]; ok {
		return old != idKey
	}
	k.m[name] = idKey
	if k.path != "" {
		if b, err := json.MarshalIndent(k.m, "", "  "); err == nil {
			os.MkdirAll(filepath.Dir(k.path), 0o700)
			os.WriteFile(k.path, b, 0o600)
		}
	}
	return false
}

// Tag is the 4 digits shown after a display name (name#1234), derived from
// the identity key, so nobody picks their own: two "Alice"s stay apart, and a
// name keeps its tag only with its key. "" for no key.
func Tag(idKey string) string {
	if idKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("blite-tag-v0|" + idKey))
	return fmt.Sprintf("%04d", binary.BigEndian.Uint32(sum[:4])%10000)
}

// Display is name#tag ("name" alone without a key).
func Display(name, idKey string) string {
	if t := Tag(idKey); t != "" {
		return name + "#" + t
	}
	return name
}

// CleanName makes a user's chosen display name valid: trimmed, no '#'
// (reserved for the tag), at most 64 bytes. "" if nothing is left.
func CleanName(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "#", ""))
	for len(s) > 64 {
		r := []rune(s)
		s = string(r[:len(r)-1])
	}
	return s
}

// LoadName returns the display name the user chose ("" = never set).
func LoadName() string {
	d, err := dir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(d, "name.txt"))
	if err != nil {
		return ""
	}
	return CleanName(string(b))
}

// SaveName stores the user's display name.
func SaveName(name string) error {
	d, err := dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(d, "name.txt"), []byte(name+"\n"), 0o600)
}
