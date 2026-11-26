package redact

import (
	"regexp"
	"strings"
	"testing"
)

func TestIPs(t *testing.T) {
	code4 := regexp.MustCompile(`^203\.0\.x\.x~[0-9a-f]{4}$`)
	for _, tc := range []struct{ in, keep, gone string }{
		{"pair 203.0.113.7:5000 <-> 192.168.1.20:5000", "192.168.1.20:5000", "203.0.113.7"},
		{"public ip=203.0.113.7.", "ip=203.0.x.x~", "113.7"},
		{"ws://203.0.113.7:8443/ws", "ws://203.0.x.x~", "113.7"},
		{"lan 10.0.0.5 cgnat 100.64.1.2 lo 127.0.0.1 ll 169.254.3.4", "10.0.0.5 cgnat 100.64.1.2 lo 127.0.0.1 ll 169.254.3.4", ""},
		{"v6 [2001:db8:1234::7]:443 ula fd2c:7174:233:33c6::1 ll fe80::1%eth0", "[2001:db8:x:x~", "1234::7"},
		{"v6 full 2a02:1810:4d02:aa00:1:2:3:4 end", "2a02:1810:x:x~", "aa00"},
		{"mapped ::ffff:203.0.113.9 x", "203.0.x.x~", "113.9"},
	} {
		out := Text(tc.in)
		if tc.keep != "" && !strings.Contains(out, tc.keep) {
			t.Errorf("%q -> %q: want %q kept", tc.in, out, tc.keep)
		}
		if tc.gone != "" && strings.Contains(out, tc.gone) {
			t.Errorf("%q -> %q: %q still there", tc.in, out, tc.gone)
		}
	}
	// Not addresses: times, versions, hashes, a MAC, fingerprints.
	for _, s := range []string{
		"time=2026-11-25T12:58:42.166482145Z level=INFO",
		"gstreamer 1.28.8 build v1.2.3.4 os 10.0.26100.1 x1.2.3.4",
		"mac aa:bb:cc:dd:ee:ff cert 28:57:9D:00:11:22:33:44:55:66:FC:4D sha deadbeef",
		"ratio 1.5 at 12:00",
	} {
		if out := Text(s); out != s {
			t.Errorf("changed a line without addresses: %q -> %q", s, out)
		}
	}
	// Same address, same code within a run; different addresses differ;
	// a second pass leaves the codes alone.
	a, b := ipCode4(t, "203.0.113.7"), ipCode4(t, "203.0.113.8")
	if !code4.MatchString(a) || a != ipCode4(t, "203.0.113.7") || a == b {
		t.Errorf("codes %q %q", a, b)
	}
	once := Text("a 203.0.113.7 b 2001:db8::1")
	if twice := Text(once); twice != once {
		t.Errorf("second pass changed %q to %q", once, twice)
	}
}

func ipCode4(t *testing.T, s string) string {
	t.Helper()
	return strings.TrimPrefix(Text("x "+s), "x ")
}
