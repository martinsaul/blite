// Package secret protects small local secrets at rest: DPAPI on Windows, and a
// no-op elsewhere where 0600 files are normal.
package secret

import (
	"encoding/base64"
	"runtime"
	"strings"
)

// Prefix marks values protected with Seal so readers can tell them apart from
// older plaintext files.
const Prefix = "dpapi:"

// Enabled reports whether local files should be sealed on write. Tests override
// it alongside Protect and Unprotect to exercise Windows paths on other OSes.
var Enabled = runtime.GOOS == "windows"

// Protect encrypts b for the current user on Windows (identity elsewhere).
// Tests may replace it.
var Protect = protect

// Unprotect decrypts data produced by Protect (identity elsewhere).
// Tests may replace it.
var Unprotect = unprotect

// Sealed reports whether s carries the dpapi: prefix.
func Sealed(s string) bool {
	return strings.HasPrefix(s, Prefix)
}

// Seal protects b and returns Prefix + base64.
func Seal(b []byte) (string, error) {
	out, err := Protect(b)
	if err != nil {
		return "", err
	}
	return Prefix + base64.StdEncoding.EncodeToString(out), nil
}

// Open reverses Seal. A value without Prefix is returned unchanged as old
// plaintext.
func Open(s string) ([]byte, error) {
	enc, ok := strings.CutPrefix(s, Prefix)
	if !ok {
		return []byte(s), nil
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return nil, err
	}
	return Unprotect(raw)
}
