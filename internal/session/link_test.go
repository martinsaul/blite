package session

import (
	"fmt"
	"strings"
	"testing"

	"blite/internal/redact"
)

func TestParseLink(t *testing.T) {
	for _, s := range []string{
		"https://share.example.com/j/7K3QX9MD#se%20cret",
		"bshare://join/7K3QX9MD#se%20cret",
		"blite://join/7K3QX9MD#se%20cret", // links shared before the rename
	} {
		code, pw, err := ParseLink(s)
		if err != nil || code != "7K3QX9MD" || pw != "se cret" {
			t.Errorf("%s: got %q %q %v", s, code, pw, err)
		}
	}
	for _, s := range []string{"ftp://join/7K3QX9MD#pw", "bshare://join/#pw"} { // a link without #password is an open room
		if _, _, err := ParseLink(s); err == nil {
			t.Errorf("%s: want an error", s)
		}
	}
	// Malformed URLs with a fragment must not echo the password in err.Error().
	for _, bad := range []string{
		"https://bad host/j/7K3QX9MD#topSecretPw",
		"bshare://join/7K3QX9MD%zz#topSecretPw",
	} {
		_, _, err := ParseLink(bad)
		if err == nil {
			t.Fatalf("%q: expected parse error", bad)
		}
		if strings.Contains(err.Error(), "topSecretPw") {
			t.Fatalf("%q: error leaked password: %v", bad, err)
		}
		if !strings.Contains(err.Error(), "7K3QX9MD") {
			t.Fatalf("%q: error lost room code: %v", bad, err)
		}
	}
}

func TestParseLinkServer(t *testing.T) {
	for _, c := range []struct{ link, server string }{
		{"https://share.example.com/j/7K3QX9MD#pw", "wss://share.example.com/ws"},
		{"https://Share.Example.com:8443/j/7K3QX9MD#pw", "wss://share.example.com:8443/ws"},
		{"http://localhost:8080/j/7K3QX9MD#pw", "ws://localhost:8080/ws"},
		{"bshare://join/7K3QX9MD#pw", ""}, // older links: the built-in default
		{"blite://join/7K3QX9MD#pw", ""},
		{"bshare://join/7K3QX9MD?server=share.example.com#pw", "wss://share.example.com/ws"},
		{"blite://join/7K3QX9MD?server=share.example.com%3A8443#pw", "wss://share.example.com:8443/ws"},
		{"bshare://join/7K3QX9MD?server=ws%3A%2F%2Flocalhost%3A8080#pw", "ws://localhost:8080/ws"},
	} {
		server, code, pw, err := ParseLinkServer(c.link)
		if err != nil || server != c.server || code != "7K3QX9MD" || pw != "pw" {
			t.Errorf("%s: got %q %q %q %v, want server %q", c.link, server, code, pw, err, c.server)
		}
	}
	for _, s := range []string{"bshare://join/7K3QX9MD?server=not%20a%20host#pw", "bshare://join/7K3QX9MD?server=evil.example.com%2Fpath#pw"} {
		if _, _, _, err := ParseLinkServer(s); err == nil {
			t.Errorf("%s: want an error", s)
		}
	}
}

// The links a room hands out lead back to the same room, server and
// password.
func TestLinkRoundTrip(t *testing.T) {
	const def = "wss://bellion.example.com/ws"
	for _, server := range []string{def, "wss://share.example.com/ws", "wss://share.example.com:8443/ws", "ws://localhost:8080/ws"} {
		for _, pw := range []string{"", "se cret/#?"} {
			app := AppLink(server, def, "7K3QX9MD", pw)
			want := server
			if server == def {
				want = "" // not named: older builds open it too
				if strings.Contains(app, "server=") {
					t.Errorf("%s: names the default server", app)
				}
			}
			s, code, p, err := ParseLinkServer(app)
			if err != nil || s != want || code != "7K3QX9MD" || p != pw {
				t.Errorf("%s: got %q %q %q %v", app, s, code, p, err)
			}
			share := ShareLink(server, "7K3QX9MD", pw)
			s, code, p, err = ParseLinkServer(share)
			if server == "ws://localhost:8080/ws" {
				continue // share links are https; local test servers aren't
			}
			if err != nil || s != server || code != "7K3QX9MD" || p != pw {
				t.Errorf("%s: got %q %q %q %v", share, s, code, p, err)
			}
		}
	}
}

func TestNormalizeServer(t *testing.T) {
	for in, want := range map[string]string{
		"share.example.com":               "wss://share.example.com/ws",
		"  Share.Example.COM  ":           "wss://share.example.com/ws",
		"share.example.com/":              "wss://share.example.com/ws",
		"share.example.com:8443":          "wss://share.example.com:8443/ws",
		"wss://share.example.com/ws":      "wss://share.example.com/ws",
		"wss://share.example.com/ws/":     "wss://share.example.com/ws",
		"https://share.example.com":       "wss://share.example.com/ws",
		"ws://localhost:8080/ws":          "ws://localhost:8080/ws",
		"http://192.0.2.7:8080":           "ws://192.0.2.7:8080/ws",
		"[2001:db8::1]:8443":              "wss://[2001:db8::1]:8443/ws",
		"xn--bcher-kva.example.com":       "wss://xn--bcher-kva.example.com/ws",
		"wss://a-b.share.example.com/ws?": "wss://a-b.share.example.com/ws",
	} {
		if got, err := NormalizeServer(in); err != nil || got != want {
			t.Errorf("%q: got %q %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "hello", "not a host", "ftp://share.example.com", "share.example.com/other",
		"wss://user@share.example.com/ws", "share.example.com:0", "share.example.com:99999", "share.example.com:x",
		"-bad.example.com", "share..example.com", "wss://share.example.com/ws#frag", "share.example.com?x=1", "bücher.example.com"} {
		if got, err := NormalizeServer(in); err == nil {
			t.Errorf("%q: got %q, want an error", in, got)
		}
	}
}

func TestParseJoinInput(t *testing.T) {
	cases := []struct {
		in     string
		server string
		code   string
		pw     string
		isLink bool
	}{
		{"https://share.example.com/j/7K3QX9MD#se%20cret", "wss://share.example.com/ws", "7K3QX9MD", "se cret", true},
		{"share.example.com/j/7K3QX9MD#pw", "wss://share.example.com/ws", "7K3QX9MD", "pw", true},
		{"bshare://join/7K3QX9MD", "", "7K3QX9MD", "", true},
		{"bshare://join/7K3QX9MD?server=share.example.com#pw", "wss://share.example.com/ws", "7K3QX9MD", "pw", true},
		{"blite://join/7K3QX9MD#pw", "", "7K3QX9MD", "pw", true},
		{"K7PQ-2M4X", "", "K7PQ2M4X", "", false},
		{"  k7pq 2m4x  ", "", "K7PQ2M4X", "", false},
		{"MAMACO", "", "MAMAC0", "", false},             // Crockford O -> 0, 6-char hosted code
		{"GAME", "", "GAME", "", false},                 // 4-char hosted code
		{"ABCDEFGHJKMN", "", "ABCDEFGHJKMN", "", false}, // 12-char hosted code
		{"K7PQ-2M4X#se%20cret", "", "K7PQ2M4X", "se cret", false},
		{"K7PQ-2M4X#pw/j/bar", "", "K7PQ2M4X", "pw/j/bar", false},
	}
	for _, c := range cases {
		server, code, pw, isLink, err := ParseJoinInput(c.in)
		if err != nil || server != c.server || code != c.code || pw != c.pw || isLink != c.isLink {
			t.Errorf("%q: got (%q, %q, %q, %v, %v), want (%q, %q, %q, %v, nil)",
				c.in, server, code, pw, isLink, err, c.server, c.code, c.pw, c.isLink)
		}
	}
	for _, bad := range []string{
		"",
		"   ",
		"ABC",                 // shorter than MinCodeLen (4)
		"ABCDEFGHJKMNP",       // longer than MaxCodeLen (12)
		"UUUU",                // 'U' is not in Crockford alphabet
		"ftp://join/7K3QX9MD", // invalid scheme
		"bshare://join/#pw",   // empty code
		"not a valid room code!",
	} {
		if _, _, _, _, err := ParseJoinInput(bad); err == nil {
			t.Errorf("%q: expected error, got nil", bad)
		}
	}
}

func TestParseJoinInputRedacted(t *testing.T) {
	codes := []string{
		"K7PQ-2M4X",
		"  k7pq 2m4x  ",
		"MAMACO",
		"GAME",
		"ABCDEFGHJKMN",
		"7K3QX9MD",
	}
	for _, raw := range codes {
		withFrag := raw + "#s3cretJoinPw"
		if _, _, pw, _, err := ParseJoinInput(withFrag); err != nil || pw != "s3cretJoinPw" {
			t.Fatalf("ParseJoinInput(%q) = pw %q, err %v", withFrag, pw, err)
		}
		line := fmt.Sprintf("msg=%q", withFrag)
		if got := redact.Text(line); strings.Contains(got, "s3cretJoinPw") {
			t.Fatalf("redact.Text(%s) leaked password: %s", line, got)
		}
		args := redact.Args([]string{withFrag, raw + "%23pctJoinPw"})
		for _, a := range args {
			if strings.Contains(a, "s3cretJoinPw") || strings.Contains(a, "pctJoinPw") {
				t.Fatalf("redact.Args(%q) leaked password: %q", raw, args)
			}
		}
	}
}
