package redact

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/netip"
	"strings"
)

// Public IP addresses in logs become lossy codes: the network part is kept
// (the first two IPv4 octets, the first two IPv6 groups) and the rest is
// replaced by a short tag, e.g. 203.0.113.7 -> 203.0.x.x~1f2e. The tag is a
// keyed hash with a key made at start and never stored, so the same address
// gets the same tag for the whole run (lines can still be matched up) but
// can't be turned back into the address. Private, loopback, link-local and
// carrier-grade NAT addresses say nothing about where someone is and stay
// as they are.

var ipKey = func() []byte {
	k := make([]byte, 32)
	rand.Read(k)
	return k
}()

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// keepIP reports whether a stays in the log as it is.
func keepIP(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsMulticast() || a.IsUnspecified() || (a.Is4() && cgnat.Contains(a))
}

// ipCode is the lossy code for a public address.
func ipCode(a netip.Addr) string {
	a = a.Unmap()
	m := hmac.New(sha256.New, ipKey)
	b := a.AsSlice()
	m.Write(b)
	tag := hex.EncodeToString(m.Sum(nil)[:2])
	if a.Is4() {
		return itoa(int(b[0])) + "." + itoa(int(b[1])) + ".x.x~" + tag
	}
	return hex4(b[0], b[1]) + ":" + hex4(b[2], b[3]) + ":x:x~" + tag
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	if n < 100 {
		return string([]byte{byte('0' + n/10), byte('0' + n%10)})
	}
	return string([]byte{byte('0' + n/100), byte('0' + n/10%10), byte('0' + n%10)})
}

func hex4(hi, lo byte) string {
	s := strings.TrimLeft(hex.EncodeToString([]byte{hi, lo}), "0")
	if s == "" {
		return "0"
	}
	return s
}

func isIPRunByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' || c == ':' || c == '.'
}

func isWordByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_'
}

// ipCandidate reports whether run (a maximal run of hex digits, ':' and
// '.') may be an address, cheaply and without allocating, so ordinary
// lines (times like 12:58:42.166, versions, hashes) never reach the parser.
func ipCandidate(run string) bool {
	if strings.Contains(run, ":") {
		// IPv6: "::" or exactly eight groups.
		return strings.Contains(run, "::") || strings.Count(run, ":") == 7
	}
	// IPv4: four groups of 1-3 decimal digits.
	groups, digits := 1, 0
	for i := 0; i < len(run); i++ {
		switch c := run[i]; {
		case c == '.':
			if digits == 0 {
				return false
			}
			groups, digits = groups+1, 0
		case c >= '0' && c <= '9':
			if digits++; digits > 3 {
				return false
			}
		default:
			return false
		}
	}
	return groups == 4 && digits > 0
}

// redactIPs replaces public IP addresses in s with ipCode.
func redactIPs(s string) string {
	var b strings.Builder
	last := 0
	for i := 0; i < len(s); {
		if !isIPRunByte(s[i]) {
			i++
			continue
		}
		j := i
		for j < len(s) && isIPRunByte(s[j]) {
			j++
		}
		run := s[i:j]
		start, end := i, j
		i = j
		// Not part of a longer word (v1.2.3.4, abc1.2.3.4x) or a dotted
		// name; a trailing dot ends a sentence.
		run = strings.TrimSuffix(run, ".")
		// host:port: an IPv4 address followed by a colon.
		if k := strings.IndexByte(run, ':'); k > 0 && !ipCandidate(run) && ipCandidate(run[:k]) {
			run = run[:k]
		}
		end = start + len(run)
		if start > 0 && (isWordByte(s[start-1]) || s[start-1] == '.') || end < len(s) && isWordByte(s[end]) {
			continue
		}
		if !ipCandidate(run) {
			continue
		}
		a, err := netip.ParseAddr(run)
		if err != nil || keepIP(a) {
			continue
		}
		if b.Len() == 0 {
			b.Grow(len(s))
		}
		b.WriteString(s[last:start])
		b.WriteString(ipCode(a))
		last = end
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// redactIPsGuarded is redactIPs behind the same recover() net as the other
// passes: a bug must not crash the logger or let the raw line through.
func redactIPsGuarded(s string) (out string) {
	defer func() {
		if recover() != nil {
			out = failedFor(s)
		}
	}()
	return redactIPs(s)
}
