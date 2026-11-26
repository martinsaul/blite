package redact

import (
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Anonymizer removes who and where from log text for an anonymous report:
// display names, room codes, server and Discord names, window titles, the
// home folder, the user name and the computer's name. Each distinct value
// becomes a stable placeholder (name1, room1, ...) so the log still reads
// as one story. It runs on top of Text, which has already removed secrets.
type Anonymizer struct {
	seen  map[string]string // value -> placeholder
	count map[string]int    // kind -> placeholders so far
	known []string          // free-text values to replace, longest first
}

// anonKeys are the log attributes whose values identify someone or
// something they used, and the placeholder each becomes.
var anonKeys = map[string]string{
	"peer": "name", "from": "name", "to": "name", "origin": "name", "viewer": "name", "provider": "name",
	"owner": "name", "offerer": "name", "name": "name", "member": "name", "members": "name", "user": "user",
	"guild": "guild", "channel": "channel", "channelId": "channel", "room": "room", "code": "room",
	"server": "server", "host": "server", "window": "window", "title": "window", "source": "window",
}

// attrRE matches key=value and key="quoted value" in slog's text format;
// jsonAttrRE matches "key":"value" in its JSON format.
var (
	attrRE     = regexp.MustCompile(`\b([A-Za-z]+)=("(?:[^"\\]|\\.)*"|[^\s"]+)`)
	jsonAttrRE = regexp.MustCompile(`"([A-Za-z]+)":"((?:[^"\\]|\\.)*)"`)
)

// peerID is a member's random per-session ID (not identifying, and needed
// to follow a connection through the log).
var peerID = regexp.MustCompile(`^[A-Za-z0-9_-]{12}$`)

// NewAnonymizer returns an Anonymizer that also replaces the given names
// (ours, members', Discord's...) wherever they appear.
func NewAnonymizer(names ...string) *Anonymizer {
	a := &Anonymizer{seen: map[string]string{}, count: map[string]int{}}
	for _, n := range names {
		if n = strings.TrimSpace(n); len(n) >= 3 {
			a.known = append(a.known, n)
			a.placeholder("name", n)
		}
	}
	if h, err := os.UserHomeDir(); err == nil && len(h) > 3 {
		a.known = append(a.known, h, filepath.ToSlash(h))
		a.seen[h], a.seen[filepath.ToSlash(h)] = "~", "~"
	}
	if u, err := user.Current(); err == nil {
		for _, n := range []string{u.Username, filepath.Base(u.Username), u.Name} {
			if len(n) >= 3 {
				a.known = append(a.known, n)
				a.placeholder("user", n)
			}
		}
	}
	if h, err := os.Hostname(); err == nil && len(h) >= 3 {
		a.known = append(a.known, h)
		a.seen[h] = "computer"
	}
	sort.Slice(a.known, func(i, j int) bool { return len(a.known[i]) > len(a.known[j]) })
	return a
}

func (a *Anonymizer) placeholder(kind, v string) string {
	if p, ok := a.seen[v]; ok {
		return p
	}
	a.count[kind]++
	p := kind + strconv.Itoa(a.count[kind])
	a.seen[v] = p
	return p
}

// value returns the anonymized form of attribute k's value raw, and
// whether it changed.
func (a *Anonymizer) value(k, raw string) (string, bool) {
	kind, ok := anonKeys[k]
	if !ok || raw == "" || peerID.MatchString(raw) || raw == "[cut]" {
		return "", false
	}
	if _, err := strconv.ParseFloat(raw, 64); err == nil {
		return "", false // counts, sizes
	}
	// "name#TAG": the tag comes from the key and stays; the name goes.
	if n, tag, ok := strings.Cut(raw, "#"); ok && n != "" {
		return a.placeholder(kind, n) + "#" + tag, true
	}
	return a.placeholder(kind, raw), true
}

// Text anonymizes s.
func (a *Anonymizer) Text(s string) string {
	s = attrRE.ReplaceAllStringFunc(s, func(m string) string {
		k, v, _ := strings.Cut(m, "=")
		if r, ok := a.value(k, strings.Trim(v, `"`)); ok {
			return k + "=" + r
		}
		return m
	})
	s = jsonAttrRE.ReplaceAllStringFunc(s, func(m string) string {
		sm := jsonAttrRE.FindStringSubmatch(m)
		if r, ok := a.value(sm[1], sm[2]); ok {
			return `"` + sm[1] + `":"` + r + `"`
		}
		return m
	})
	for _, n := range a.known {
		if strings.Contains(s, n) {
			s = strings.ReplaceAll(s, n, a.seen[n])
		}
	}
	return s
}
