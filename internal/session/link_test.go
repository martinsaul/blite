package session

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"blite/internal/redact"
	"blite/internal/room"
)

func TestParseLink(t *testing.T) {
	for _, s := range []string{
		"https://share.example.com/j/7K3QX9MD#se%20cret",
		"https://share.example.com/j/7K3QX9MD?e2ee=1#se%20cret",
		"bshare://join/7K3QX9MD#se%20cret",
		"bshare://join/7K3QX9MD?e2ee=1#se%20cret",
		"blite://join/7K3QX9MD#se%20cret", // links shared before the rename
		"blite://join/7K3QX9MD?server=share.example.com&e2ee=1#se%20cret",
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
	for _, c := range []struct {
		link   string
		server string
		e2ee   bool
	}{
		{"https://share.example.com/j/7K3QX9MD#pw", "wss://share.example.com/ws", false},
		{"https://share.example.com/j/7K3QX9MD?e2ee=1#pw", "wss://share.example.com/ws", true},
		{"https://Share.Example.com:8443/j/7K3QX9MD#pw", "wss://share.example.com:8443/ws", false},
		{"http://localhost:8080/j/7K3QX9MD?e2ee=1#pw", "ws://localhost:8080/ws", true},
		{"bshare://join/7K3QX9MD#pw", "", false}, // older links: the built-in default
		{"bshare://join/7K3QX9MD?e2ee=1#pw", "", true},
		{"blite://join/7K3QX9MD#pw", "", false},
		{"bshare://join/7K3QX9MD?server=share.example.com#pw", "wss://share.example.com/ws", false},
		{"bshare://join/7K3QX9MD?server=share.example.com&e2ee=1#pw", "wss://share.example.com/ws", true},
		{"blite://join/7K3QX9MD?server=share.example.com%3A8443&e2ee=1#pw", "wss://share.example.com:8443/ws", true},
		{"bshare://join/7K3QX9MD?server=ws%3A%2F%2Flocalhost%3A8080#pw", "ws://localhost:8080/ws", false},
	} {
		server, code, pw, e2ee, err := ParseLinkServer(c.link)
		if err != nil || server != c.server || code != "7K3QX9MD" || pw != "pw" || e2ee != c.e2ee {
			t.Errorf("%s: got %q %q %q e2ee=%v %v, want server %q e2ee=%v", c.link, server, code, pw, e2ee, err, c.server, c.e2ee)
		}
	}
	for _, s := range []string{"bshare://join/7K3QX9MD?server=not%20a%20host#pw", "bshare://join/7K3QX9MD?server=evil.example.com%2Fpath#pw"} {
		if _, _, _, _, err := ParseLinkServer(s); err == nil {
			t.Errorf("%s: want an error", s)
		}
	}
}

// The links a room hands out lead back to the same room, server, password and
// encryption requirement, and still parse with older clients' link logic.
func TestLinkRoundTrip(t *testing.T) {
	const def = "wss://bellion.example.com/ws"
	for _, server := range []string{def, "wss://share.example.com/ws", "wss://share.example.com:8443/ws", "ws://localhost:8080/ws"} {
		for _, pw := range []string{"", "se cret/#?"} {
			for _, e2ee := range []bool{false, true} {
				app := AppLink(server, def, "7K3QX9MD", pw, e2ee)
				want := server
				if server == def {
					want = "" // not named: older builds open it too
					if strings.Contains(app, "server=") {
						t.Errorf("%s: names the default server", app)
					}
				}
				if before, _, _ := strings.Cut(app, "#"); strings.Contains(before, "e2ee=1") != e2ee {
					t.Errorf("%s: marker mismatch for e2ee=%v", app, e2ee)
				}
				s, code, p, gotE2EE, err := ParseLinkServer(app)
				if err != nil || s != want || code != "7K3QX9MD" || p != pw || gotE2EE != e2ee {
					t.Errorf("%s: got %q %q %q e2ee=%v %v", app, s, code, p, gotE2EE, err)
				}
				if os, oc, op, oerr := legacyParseLinkServer(app); oerr != nil || os != want || oc != "7K3QX9MD" || op != pw {
					t.Errorf("legacyParseLinkServer(%s): got %q %q %q %v", app, os, oc, op, oerr)
				}

				share := ShareLink(server, "7K3QX9MD", pw, e2ee)
				if before, _, _ := strings.Cut(share, "#"); strings.Contains(before, "e2ee=1") != e2ee {
					t.Errorf("%s: marker mismatch for e2ee=%v", share, e2ee)
				}
				s, code, p, gotE2EE, err = ParseLinkServer(share)
				if server == "ws://localhost:8080/ws" {
					continue // share links are https; local test servers aren't
				}
				if err != nil || s != server || code != "7K3QX9MD" || p != pw || gotE2EE != e2ee {
					t.Errorf("%s: got %q %q %q e2ee=%v %v", share, s, code, p, gotE2EE, err)
				}
				if os, oc, op, oerr := legacyParseLinkServer(share); oerr != nil || os != server || oc != "7K3QX9MD" || op != pw {
					t.Errorf("legacyParseLinkServer(%s): got %q %q %q %v", share, os, oc, op, oerr)
				}
				if os, oc, op, olink, oerr := legacyParseJoinInput(share); oerr != nil || !olink || os != server || oc != "7K3QX9MD" || op != pw {
					t.Errorf("legacyParseJoinInput(%s): got %q %q %q %v %v", share, os, oc, op, olink, oerr)
				}
			}
		}
	}

	// Simulate the blite:// link join.js builds when /j/{code}?e2ee=1#pw opens.
	joinPageTarget := "blite://join/7K3QX9MD?server=share.example.com&e2ee=1#se%20cret"
	if s, c, p, e2ee, err := ParseLinkServer(joinPageTarget); err != nil || s != "wss://share.example.com/ws" || c != "7K3QX9MD" || p != "se cret" || !e2ee {
		t.Fatalf("join.js target parse: %q %q %q %v %v", s, c, p, e2ee, err)
	}
	if s, c, p, err := legacyParseLinkServer(joinPageTarget); err != nil || s != "wss://share.example.com/ws" || c != "7K3QX9MD" || p != "se cret" {
		t.Fatalf("legacy join.js target parse: %q %q %q %v", s, c, p, err)
	}
}

// legacyParseLink, legacyParseLinkServer and legacyParseJoinInput keep the
// pre-marker parser logic verbatim to verify backward compatibility with older
// clients.
func legacyParseLink(s string) (code, pw string, err error) {
	u, err := parseURLRedacted(strings.TrimSpace(s))
	if err != nil {
		return "", "", err
	}
	var path string
	switch u.Scheme {
	case "bshare", "blite":
		path = strings.TrimPrefix(u.Host+u.Path, "join")
	case "https", "http":
		path = strings.TrimPrefix(u.Path, "/j")
	default:
		return "", "", fmt.Errorf("unknown link scheme %q", u.Scheme)
	}
	code = strings.Trim(path, "/")
	pw = u.Fragment
	if code == "" {
		return "", "", errors.New("link needs a room code")
	}
	return code, pw, nil
}

func legacyParseLinkServer(s string) (server, code, pw string, err error) {
	code, pw, err = legacyParseLink(s)
	if err != nil {
		return "", "", "", err
	}
	u, err := parseURLRedacted(strings.TrimSpace(s))
	if err != nil {
		return "", "", "", err
	}
	switch u.Scheme {
	case "https", "http":
		server = u.Host
		if u.Scheme == "http" {
			server = "ws://" + u.Host
		}
	default:
		server = u.Query().Get("server")
	}
	if server == "" {
		return "", code, pw, nil
	}
	if server, err = NormalizeServer(server); err != nil {
		return "", "", "", errors.New("the link's server isn't valid: " + err.Error())
	}
	return server, code, pw, nil
}

func legacyParseJoinInput(s string) (server, code, pw string, isLink bool, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", "", false, errors.New("enter a share link or room code")
	}
	rawCode, frag, hasFrag := strings.Cut(s, "#")
	if strings.Contains(rawCode, "://") {
		server, code, pw, err = legacyParseLinkServer(s)
		return server, code, pw, true, err
	}
	if strings.Contains(rawCode, "/j/") {
		server, code, pw, err = legacyParseLinkServer("https://" + s)
		return server, code, pw, true, err
	}
	code = room.NormalizeCode(rawCode)
	if code == "" {
		return "", "", "", false, errors.New("enter a share link or room code")
	}
	if hasFrag {
		if unesc, uerr := url.PathUnescape(frag); uerr == nil {
			pw = unesc
		} else {
			pw = frag
		}
	}
	return "", code, pw, false, nil
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
		e2ee   bool
		isLink bool
	}{
		{"https://share.example.com/j/7K3QX9MD#se%20cret", "wss://share.example.com/ws", "7K3QX9MD", "se cret", false, true},
		{"https://share.example.com/j/7K3QX9MD?e2ee=1#se%20cret", "wss://share.example.com/ws", "7K3QX9MD", "se cret", true, true},
		{"share.example.com/j/7K3QX9MD#pw", "wss://share.example.com/ws", "7K3QX9MD", "pw", false, true},
		{"share.example.com/j/7K3QX9MD?e2ee=1#pw", "wss://share.example.com/ws", "7K3QX9MD", "pw", true, true},
		{"bshare://join/7K3QX9MD", "", "7K3QX9MD", "", false, true},
		{"bshare://join/7K3QX9MD?e2ee=1", "", "7K3QX9MD", "", true, true},
		{"bshare://join/7K3QX9MD?server=share.example.com#pw", "wss://share.example.com/ws", "7K3QX9MD", "pw", false, true},
		{"bshare://join/7K3QX9MD?server=share.example.com&e2ee=1#pw", "wss://share.example.com/ws", "7K3QX9MD", "pw", true, true},
		{"blite://join/7K3QX9MD#pw", "", "7K3QX9MD", "pw", false, true},
		{"blite://join/7K3QX9MD?server=share.example.com&e2ee=1#pw", "wss://share.example.com/ws", "7K3QX9MD", "pw", true, true},
		{"K7PQ-2M4X", "", "K7PQ2M4X", "", false, false},
		{"  k7pq 2m4x  ", "", "K7PQ2M4X", "", false, false},
		{"MAMACO", "", "MAMAC0", "", false, false},             // Crockford O -> 0, 6-char hosted code
		{"GAME", "", "GAME", "", false, false},                 // 4-char hosted code
		{"ABCDEFGHJKMN", "", "ABCDEFGHJKMN", "", false, false}, // 12-char hosted code
		{"K7PQ-2M4X#se%20cret", "", "K7PQ2M4X", "se cret", false, false},
		{"K7PQ-2M4X#pw/j/bar", "", "K7PQ2M4X", "pw/j/bar", false, false},
	}
	for _, c := range cases {
		server, code, pw, e2ee, isLink, err := ParseJoinInput(c.in)
		if err != nil || server != c.server || code != c.code || pw != c.pw || e2ee != c.e2ee || isLink != c.isLink {
			t.Errorf("%q: got (%q, %q, %q, %v, %v, %v), want (%q, %q, %q, %v, %v, nil)",
				c.in, server, code, pw, e2ee, isLink, err, c.server, c.code, c.pw, c.e2ee, c.isLink)
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
		if _, _, _, _, _, err := ParseJoinInput(bad); err == nil {
			t.Errorf("%q: expected error, got nil", bad)
		}
	}
}

func TestParseJoinInputRedacted(t *testing.T) {
	inputs := []string{
		"K7PQ-2M4X",
		"  k7pq 2m4x  ",
		"MAMACO",
		"GAME",
		"ABCDEFGHJKMN",
		"7K3QX9MD",
		"https://share.example.com/j/7K3QX9MD?e2ee=1",
		"bshare://join/7K3QX9MD?e2ee=1",
		"blite://join/7K3QX9MD?server=share.example.com&e2ee=1",
		"share.example.com/j/7K3QX9MD?e2ee=1",
	}
	for _, raw := range inputs {
		withFrag := raw + "#s3cretJoinPw"
		if _, _, pw, _, _, err := ParseJoinInput(withFrag); err != nil || pw != "s3cretJoinPw" {
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
