package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"

	"blite/internal/secret"
)

// A backup is the identity key (and display name) sealed with a passphrase,
// so it can move to another computer: the tag and hosted-room ownership come
// with the key. Argon2id stretches the passphrase; XChaCha20-Poly1305 seals
// the key, with the header as additional data so its parameters can't be
// changed unnoticed.

// BackupExt is the backup file's extension.
const BackupExt = ".bshkey"

const backupFormat = "bellion-identity-backup-v1"

// MinPassphrase is the shortest passphrase a backup accepts, in characters.
const MinPassphrase = 8

// Argon2id cost for new backups (RFC 9106's second recommended option), and
// the most an imported file may ask for, so a crafted one can't exhaust memory.
const (
	argonTime, argonMemKiB, argonThreads = 3, 64 * 1024, 4
	maxTime, maxMemKiB, maxThreads       = 10, 1024 * 1024, 16
)

type backupFile struct {
	Format  string `json:"format"`
	KDF     string `json:"kdf"`
	Time    uint32 `json:"t"`
	MemKiB  uint32 `json:"m"`
	Threads uint8  `json:"p"`
	Salt    []byte `json:"salt"`
	Nonce   []byte `json:"nonce"`
	Box     []byte `json:"box"`
}

type backupSecret struct {
	Seed []byte `json:"seed"`
	Name string `json:"name,omitempty"`
}

// ErrBackupOpen means the passphrase is wrong or the file isn't a backup.
var ErrBackupOpen = errors.New("wrong passphrase, or the file isn't an identity key backup")

// ErrShortPassphrase is returned for a passphrase under MinPassphrase characters.
var ErrShortPassphrase = errors.New("use a passphrase of at least 8 characters")

func (f backupFile) header() []byte {
	return fmt.Appendf(nil, "%s|%s|%d|%d|%d", f.Format, f.KDF, f.Time, f.MemKiB, f.Threads)
}

func (f backupFile) key(pass string) []byte {
	return argon2.IDKey([]byte(pass), f.Salt, f.Time, f.MemKiB, f.Threads, chacha20poly1305.KeySize)
}

// Backup seals k and the display name with passphrase.
func (k Key) Backup(name, passphrase string) ([]byte, error) {
	if len(k.Private) != ed25519.PrivateKeySize {
		return nil, errors.New("no identity key")
	}
	if utf8.RuneCountInString(passphrase) < MinPassphrase {
		return nil, ErrShortPassphrase
	}
	f := backupFile{Format: backupFormat, KDF: "argon2id", Time: argonTime, MemKiB: argonMemKiB, Threads: argonThreads,
		Salt: make([]byte, 16), Nonce: make([]byte, chacha20poly1305.NonceSizeX)}
	if _, err := rand.Read(f.Salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(f.Nonce); err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(f.key(passphrase))
	if err != nil {
		return nil, err
	}
	plain, err := json.Marshal(backupSecret{Seed: k.Private.Seed(), Name: name})
	if err != nil {
		return nil, err
	}
	f.Box = aead.Seal(nil, f.Nonce, plain, f.header())
	return json.MarshalIndent(f, "", "  ")
}

// OpenBackup returns the key and display name in a backup.
func OpenBackup(data []byte, passphrase string) (Key, string, error) {
	var f backupFile
	if err := json.Unmarshal(data, &f); err != nil || f.Format != backupFormat || f.KDF != "argon2id" {
		return Key{}, "", ErrBackupOpen
	}
	if f.Time < 1 || f.Time > maxTime || f.MemKiB < 8*uint32(f.Threads) || f.MemKiB > maxMemKiB ||
		f.Threads < 1 || f.Threads > maxThreads || len(f.Salt) < 16 || len(f.Nonce) != chacha20poly1305.NonceSizeX {
		return Key{}, "", ErrBackupOpen
	}
	aead, err := chacha20poly1305.NewX(f.key(passphrase))
	if err != nil {
		return Key{}, "", err
	}
	plain, err := aead.Open(nil, f.Nonce, f.Box, f.header())
	if err != nil {
		return Key{}, "", ErrBackupOpen
	}
	var s backupSecret
	if err := json.Unmarshal(plain, &s); err != nil || len(s.Seed) != ed25519.SeedSize {
		return Key{}, "", ErrBackupOpen
	}
	return Key{Private: ed25519.NewKeyFromSeed(s.Seed)}, CleanName(s.Name), nil
}

func sealOldKey(old []byte) ([]byte, error) {
	if !secret.Enabled {
		return old, nil
	}
	raw := strings.TrimSpace(string(old))
	if raw == "" || secret.Sealed(raw) {
		return old, nil
	}
	if seed, err := base64.StdEncoding.DecodeString(raw); err == nil && len(seed) == ed25519.SeedSize {
		defer clear(seed)
		return encodeSeed(seed)
	}
	s, err := secret.Seal(old)
	if err != nil {
		return nil, err
	}
	return []byte(s + "\n"), nil
}

// Install makes k this computer's identity key from the next start. The
// key it replaces is kept as identity.key.old.
func Install(k Key) error {
	if len(k.Private) != ed25519.PrivateKeySize {
		return errors.New("no identity key")
	}
	d, err := dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	seed := k.Private.Seed()
	enc, err := encodeSeed(seed)
	if err != nil {
		return err
	}
	p := filepath.Join(d, "identity.key")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, enc, 0o600); err != nil {
		return err
	}
	if secret.Enabled {
		rb, err := os.ReadFile(tmp)
		if err != nil {
			os.Remove(tmp)
			return err
		}
		got, err := secret.Open(strings.TrimSpace(string(rb)))
		if err != nil || !bytes.Equal(got, seed) {
			os.Remove(tmp)
			if err != nil {
				return err
			}
			return errors.New("installed key mismatch")
		}
		clear(got)
	}
	if old, err := os.ReadFile(p); err == nil {
		oldOut, err := sealOldKey(old)
		if err != nil {
			os.Remove(tmp)
			return err
		}
		if err := os.WriteFile(p+".old", oldOut, 0o600); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	return os.Rename(tmp, p)
}
