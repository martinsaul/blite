package session

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// A link names the signaling server its room is on: https://HOST/j/CODE is
// on wss://HOST/ws, and bshare://join/CODE?server=HOST on wss://HOST/ws.
// bshare:// links without a server (all links before custom servers) are on
// the built-in default server.

// ParseLinkServer is ParseLink plus the link's signaling server, normalized
// (wss://host/ws); "" = the link doesn't name one (the built-in default).
func ParseLinkServer(s string) (server, code, pw string, err error) {
	code, pw, err = ParseLink(s)
	if err != nil {
		return "", "", "", err
	}
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return "", "", "", err
	}
	switch u.Scheme {
	case "https", "http":
		server = u.Host
		if u.Scheme == "http" { // local test servers
			server = "ws://" + u.Host
		}
	default: // bshare, blite
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

// AppLink builds the bshare:// link that opens the app on a room. It names
// the server unless that is def, the built-in default, so older builds
// (which ignore the server) still open links to the default server.
func AppLink(server, def, room, pw string) string {
	l := "bshare://join/" + url.PathEscape(room)
	if h := ServerHost(server); h != "" && !SameServer(server, def) {
		if strings.HasPrefix(server, "ws://") {
			h = "ws://" + h // local test servers
		}
		l += "?server=" + url.QueryEscape(h)
	}
	if pw != "" {
		l += "#" + url.PathEscape(pw)
	}
	return l
}

// NormalizeServer turns what a user typed or a link carries (a host like
// share.example.com, host:port, or a full wss://host/ws URL) into a
// signaling URL, wss://host/ws. ws:// stays ws:// (local test servers);
// https:// and http:// are taken as wss:// and ws://.
func NormalizeServer(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("enter a server")
	}
	if !strings.Contains(s, "://") {
		s = "wss://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", errors.New("not a server address")
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "wss", "https":
		scheme = "wss"
	case "ws", "http":
		scheme = "ws"
	default:
		return "", errors.New("use a host name like share.example.com or a wss:// address")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", errors.New("use a host name like share.example.com or a wss:// address")
	}
	if p := strings.TrimSuffix(u.Path, "/"); p != "" && p != "/ws" {
		return "", errors.New("the signaling address must end in /ws")
	}
	host := strings.ToLower(u.Host)
	if !validHost(host) {
		return "", errors.New("not a valid host name: " + u.Host)
	}
	return scheme + "://" + host + "/ws", nil
}

// validHost accepts a DNS name with a dot (or localhost) or an IP address,
// with an optional port.
func validHost(h string) bool {
	name, port := h, ""
	if hh, p, err := net.SplitHostPort(h); err == nil {
		name, port = hh, p
	} else if strings.Count(h, ":") == 1 || strings.HasPrefix(h, "[") {
		return false // a bad port, or a bracketed address without one
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	if net.ParseIP(name) != nil {
		return true
	}
	if name == "localhost" {
		return true
	}
	if len(name) > 253 || !strings.Contains(name, ".") {
		return false
	}
	for _, l := range strings.Split(name, ".") {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, c := range l {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// ServerHost is the host (and port) of a signaling URL; "" if it has none.
func ServerHost(server string) string {
	u, err := url.Parse(server)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

// SameServer reports whether two signaling URLs are on the same host.
func SameServer(a, b string) bool {
	h := ServerHost(a)
	return h != "" && h == ServerHost(b)
}
