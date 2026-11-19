package camera

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// hashID is a camera's ID: 12 hex characters of
// HMAC-SHA256(install secret, backend NUL key). The key is the most stable
// device path there is; hashing it keeps USB serials out of logs and source
// strings and needs no escaping, and the per-install secret keeps the hash
// from being brute-forced back to a serial.
func hashID(backend, key string) string {
	m := hmac.New(sha256.New, idSecret())
	m.Write([]byte(backend + "\x00" + key))
	return hex.EncodeToString(m.Sum(nil)[:6])
}

const idKeyLen = 32

var (
	idKeyOnce sync.Once
	idKey     []byte
	// idKeyPath is where the secret lives: <config>/blite/camera-id.key,
	// next to identity.key. Tests override it.
	idKeyPath = func() (string, error) {
		d, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(d, "blite", "camera-id.key"), nil
	}
)

// idSecret is the install's ID secret, created (0600) on first use. If it
// can't be read or stored, a per-process random key is used: IDs then hold
// only for this run, which is still fine for -cameras.
func idSecret() []byte {
	idKeyOnce.Do(func() {
		if k, err := loadOrCreateIDKey(); err == nil {
			idKey = k
			return
		}
		idKey = make([]byte, idKeyLen)
		if _, err := rand.Read(idKey); err != nil {
			panic("camera: no randomness: " + err.Error())
		}
	})
	return idKey
}

func loadOrCreateIDKey() ([]byte, error) {
	p, err := idKeyPath()
	if err != nil {
		return nil, err
	}
	if k, err := readIDKey(p); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return k, err
	}
	k := make([]byte, idKeyLen)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) { // another process won the race
		return readIDKey(p)
	}
	if err != nil {
		return nil, err
	}
	_, err = f.WriteString(hex.EncodeToString(k) + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(p)
		return nil, err
	}
	return k, nil
}

func readIDKey(p string) ([]byte, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	k, err := hex.DecodeString(string(bytes.TrimSpace(b)))
	if err != nil || len(k) != idKeyLen {
		return nil, errors.New("corrupt camera ID key")
	}
	return k, nil
}

var idRE = regexp.MustCompile(`^([0-9a-f]{12}|test-[a-z0-9-]+)$`)

// SourcePrefix starts a camera source string: "camera:<id>".
const SourcePrefix = "camera:"

// ParseSource returns the ID of a "camera:<id>" source. It only checks the
// form; it doesn't look for the device.
func ParseSource(src string) (id string, ok bool) {
	id, ok = strings.CutPrefix(src, SourcePrefix)
	return id, ok && idRE.MatchString(id)
}

// Source is the source string for a camera ID.
func Source(id string) string { return SourcePrefix + id }

// byIDDir holds the persistent v4l2 names (udev): a symlink per device
// named after its USB vendor, product and serial.
const byIDDir = "/dev/v4l/by-id"

// v4l2Resolver maps a /dev/videoN path to its /dev/v4l/by-id link (the
// first by name if several point to it), or to itself when there is none
// (v4l2loopback, whose video_nr is fixed anyway). It reads dir once.
func v4l2Resolver(dir string) func(string) string {
	links := map[string]string{}
	if es, err := os.ReadDir(dir); err == nil {
		names := make([]string, 0, len(es))
		for _, e := range es {
			names = append(names, e.Name())
		}
		sort.Strings(names)
		for _, n := range names {
			p := filepath.Join(dir, n)
			if real, err := filepath.EvalSymlinks(p); err == nil {
				if _, seen := links[real]; !seen {
					links[real] = p
				}
			}
		}
	}
	return func(dev string) string {
		if dev == "" {
			return ""
		}
		real := dev
		if r, err := filepath.EvalSymlinks(dev); err == nil {
			real = r
		}
		if l, ok := links[real]; ok {
			return l
		}
		return real
	}
}

// winInstanceKey reduces a Windows device path to the device instance it
// names, so a Media Foundation path and a DirectShow moniker of the same
// camera compare equal:
//
//	\\?\usb#vid_046d&pid_082d&mi_00#7&1a2b&0&0000#{e5323777-...}\global  (MF)
//	@device:pnp:\\?\usb#vid_046d&pid_082d&mi_00#7&1a2b&0&0000#{65e8773d-...}\global  (DirectShow)
//
// both become `\\?\usb#vid_046d&pid_082d&mi_00#7&1a2b&0&0000`. Software
// (virtual) DirectShow devices ("@device:sw:...") have no instance: "".
func winInstanceKey(path string) string {
	p := strings.ToLower(strings.TrimSpace(path))
	if i := strings.Index(p, `\\?\`); i >= 0 {
		p = p[i:]
	} else {
		return ""
	}
	if i := strings.LastIndex(p, "#{"); i > 0 {
		p = p[:i]
	}
	return p
}
