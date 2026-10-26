package room

import (
	"crypto/rand"
	"strings"
)

// Crockford base32 alphabet: no I, L, O, U, so codes survive being read aloud.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// CodeLen is the number of significant characters in a room code (~40 bits).
const CodeLen = 8

// NewCode returns a random room code without separators, e.g. "7K3QX9MD".
func NewCode() string {
	b := make([]byte, CodeLen)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	for i := range b {
		b[i] = crockford[b[i]&31]
	}
	return string(b)
}

// Display formats a code as XXXX-XXXX.
func Display(code string) string {
	if len(code) != CodeLen {
		return code
	}
	return code[:4] + "-" + code[4:]
}

// NormalizeCode accepts user input ("7k3q-x9md", "7K3Q X9MD", "7K3QX9MO") and returns
// the canonical code, or "" if it can't be one. Crockford's decode rules map
// O->0, I/L->1; U is rejected.
func NormalizeCode(in string) string {
	var sb strings.Builder
	for _, r := range strings.ToUpper(in) {
		switch {
		case r == '-' || r == ' ':
			continue
		case r == 'O':
			r = '0'
		case r == 'I' || r == 'L':
			r = '1'
		}
		if r > 127 || strings.IndexByte(crockford, byte(r)) < 0 {
			return ""
		}
		sb.WriteRune(r)
	}
	if sb.Len() != CodeLen {
		return ""
	}
	return sb.String()
}
