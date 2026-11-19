// Package redact strips room passwords, share-link fragments, URL credentials,
// and secret log fields from CLI arguments and diagnostic/report/disk log text.
package redact

import (
	"bytes"
	"io"
	"log/slog"
	"math/bits"
	"net"
	"strconv"
	"strings"
	"sync"
)

const (
	crockford  = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	minCodeLen = 4
	maxCodeLen = 12
	maxLineBuf = 64 << 10
)

type keyKind uint8

const (
	keyNone    keyKind = iota
	keyStrict          // always cut (strings, numbers, booleans, hex, etc.)
	keyKeyOnly         // "key": keep only true/false and <6-digit decimal ints
)

// classifyKey classifies an identifier or flag name (case-insensitively):
//   - exact: pw, pass, password, passwd, token, secret, apikey, api_key, api-key, host_key, hostkey -> keyStrict
//   - suffix: *password(s), *passwd(s), *passphrase(s), *token(s), *secret(s), *apikey, *api_key, *api-key -> keyStrict
//   - exact: key -> keyKeyOnly
//   - dotted keys (e.g. room.password, room.pw, auth.key): classified by final segment
func classifyKey(raw string) keyKind {
	k := strings.TrimLeft(strings.TrimSpace(raw), "-")
	if k == "" {
		return keyNone
	}
	if dot := strings.LastIndexByte(k, '.'); dot >= 0 {
		return classifyKey(k[dot+1:])
	}
	if strings.EqualFold(k, "key") {
		return keyKeyOnly
	}
	switch len(k) {
	case 2:
		if strings.EqualFold(k, "pw") {
			return keyStrict
		}
	case 4:
		if strings.EqualFold(k, "pass") {
			return keyStrict
		}
	case 5:
		if strings.EqualFold(k, "token") {
			return keyStrict
		}
	case 6:
		if strings.EqualFold(k, "passwd") || strings.EqualFold(k, "secret") || strings.EqualFold(k, "apikey") {
			return keyStrict
		}
	case 7:
		if strings.EqualFold(k, "api_key") || strings.EqualFold(k, "api-key") || strings.EqualFold(k, "hostkey") {
			return keyStrict
		}
	case 8:
		if strings.EqualFold(k, "password") || strings.EqualFold(k, "host_key") || strings.EqualFold(k, "host-key") {
			return keyStrict
		}
	}
	for _, suf := range []string{
		"password", "passwords",
		"passwd", "passwds",
		"passphrase", "passphrases",
		"token", "tokens",
		"secret", "secrets",
		"apikey", "api_key", "api-key",
	} {
		if hasSuffixFold(k, suf) {
			return keyStrict
		}
	}
	return keyNone
}

func hasSuffixFold(s, lowerSuf string) bool {
	if len(s) < len(lowerSuf) {
		return false
	}
	off := len(s) - len(lowerSuf)
	for i := 0; i < len(lowerSuf); i++ {
		c := s[off+i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lowerSuf[i] {
			return false
		}
	}
	return true
}

// IsSecretKey reports whether k (case-insensitively) names a secret log field.
func IsSecretKey(k string) bool {
	return classifyKey(k) != keyNone
}

// isAllowedKeyNonSecretLiteral reports whether an unquoted value for "key" is
// a non-secret boolean ("true"/"false") or a decimal integer with fewer than
// 6 digits. Hex (0x...), floats/Inf/NaN, underscored numbers (1_000), and
// >=6-digit integers are rejected (and therefore redacted).
func isAllowedKeyNonSecretLiteral(v string) bool {
	if strings.EqualFold(v, "true") || strings.EqualFold(v, "false") {
		return true
	}
	if v != "" && v[0] == '-' {
		v = v[1:]
	}
	if len(v) == 0 || len(v) >= 6 {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return false
		}
	}
	return true
}

// ReplaceAttr is a slog.HandlerOptions.ReplaceAttr callback that redacts
// secret-keyed attributes (case-insensitively, including suffixed keys like
// joinPassword/passwords/room.password) at log write time while preserving
// host/code on "link" attributes and small decimal ints/booleans on "key".
func ReplaceAttr(_ []string, a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	if strings.EqualFold(strings.TrimSpace(a.Key), "link") {
		if v.Kind() == slog.KindString {
			return slog.String(a.Key, URL(v.String()))
		}
		return a
	}
	switch classifyKey(a.Key) {
	case keyStrict:
		return slog.String(a.Key, "[cut]")
	case keyKeyOnly:
		switch v.Kind() {
		case slog.KindBool:
			return a
		case slog.KindInt64:
			if n := v.Int64(); n > -100000 && n < 100000 {
				return a
			}
		case slog.KindUint64:
			if v.Uint64() < 100000 {
				return a
			}
		}
		return slog.String(a.Key, "[cut]")
	}
	return a
}

// Writer wraps an io.Writer and runs Text on complete lines so secrets never
// reach log files on disk, even when pipe reads split a line across writes.
// Lines exceeding maxLineBuf drop their remainder up to the next newline with a
// "[line truncated]" marker rather than flushing unredacted partial tokens.
type Writer struct {
	mu       sync.Mutex
	w        io.Writer
	buf      []byte
	dropping bool
}

// NewWriter returns a line-buffered redacting Writer around w.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w}
}

// Write implements io.Writer.
func (rw *Writer) Write(p []byte) (n int, err error) {
	total := len(p)
	if total == 0 {
		return 0, nil
	}
	endsLine := p[total-1] == '\n'
	rw.mu.Lock()
	defer rw.mu.Unlock()
	// Text already recovers from redaction panics; this also covers the
	// truncation path. On a panic, drop whatever is buffered (it may hold an
	// unredacted partial line) and the rest of the current line.
	defer func() {
		if recover() != nil {
			rw.buf = rw.buf[:0]
			rw.dropping = !endsLine
			_, err = io.WriteString(rw.w, redactionFailed+"\n")
			n = total
			if err != nil {
				n = 0
			}
		}
	}()

	if rw.dropping {
		nl := bytes.IndexByte(p, '\n')
		if nl < 0 {
			return total, nil
		}
		rw.dropping = false
		p = p[nl+1:]
		if len(p) == 0 {
			return total, nil
		}
	}

	if len(rw.buf) == 0 && len(p) <= maxLineBuf && p[len(p)-1] == '\n' {
		// Redact line by line, so a redaction failure (Text's recover net)
		// replaces only the line that caused it.
		for len(p) > 0 {
			nl := bytes.IndexByte(p, '\n')
			if _, err := io.WriteString(rw.w, Text(string(p[:nl+1]))); err != nil {
				return 0, err
			}
			p = p[nl+1:]
		}
		return total, nil
	}

	rw.buf = append(rw.buf, p...)
	off := 0
	for off < len(rw.buf) {
		rem := rw.buf[off:]
		if nl := bytes.IndexByte(rem, '\n'); nl >= 0 {
			if nl > maxLineBuf {
				if err := rw.writeTruncatedPrefixLocked(rem[:maxLineBuf]); err != nil {
					rw.buf = append(rw.buf[:0], rw.buf[off:]...)
					return 0, err
				}
			} else {
				if _, err := io.WriteString(rw.w, Text(string(rem[:nl+1]))); err != nil {
					rw.buf = append(rw.buf[:0], rw.buf[off:]...)
					return 0, err
				}
			}
			off += nl + 1
			continue
		}
		if len(rem) > maxLineBuf {
			if err := rw.writeTruncatedPrefixLocked(rem[:maxLineBuf]); err != nil {
				rw.buf = append(rw.buf[:0], rw.buf[off:]...)
				return 0, err
			}
			off = len(rw.buf)
			rw.dropping = true
		}
		break
	}
	if off > 0 {
		rw.buf = append(rw.buf[:0], rw.buf[off:]...)
	}
	return total, nil
}

type openQuote struct {
	idx int  // index of opening quote (including leading backslashes if escaped)
	bs  int  // backslash count before quote (0 for unescaped, 2^d-1 for escaped)
	ch  byte // '"' or '\''
}

func isQuoteEscapeDepth(bs int) bool {
	return bs > 0 && (bs&(bs+1)) == 0
}

// closesQuoteAtDepth reports whether a quote preceded by cbs backslashes closes
// (or terminates via an outer quote) a string opened with openBS backslashes.
// quoteBSUsed is how many of the cbs backslashes belong to the closing quote token.
func closesQuoteAtDepth(cbs, openBS int) (quoteBSUsed int, closes bool) {
	if openBS == 0 {
		if cbs%2 == 0 {
			return 0, true
		}
		return 0, false
	}
	if cbs < openBS && (cbs == 0 || isQuoteEscapeDepth(cbs)) {
		return cbs, true
	}
	if cbs >= openBS && (cbs-openBS)%(2*(openBS+1)) == 0 {
		return openBS, true
	}
	return 0, false
}

// lineCursor tracks quote, escape, brace, and bracket nesting incrementally in
// a single left-to-right forward pass so multi-match lines stay strictly O(n).
type lineCursor struct {
	s        string
	pos      int
	bsRun    int
	quotes   int
	braces   int
	brackets int
	stackLen int
	stackBuf [32]openQuote
}

func (c *lineCursor) init(s string) {
	*c = lineCursor{s: s}
}

func (c *lineCursor) resetLine() {
	c.bsRun = 0
	c.quotes = 0
	c.braces = 0
	c.brackets = 0
	c.stackLen = 0
}

func (c *lineCursor) openQuotes() []openQuote {
	return c.stackBuf[:c.stackLen]
}

func (c *lineCursor) advanceTo(target int) {
	if target > len(c.s) {
		target = len(c.s)
	}
	if target < c.pos {
		lineStart := strings.LastIndexAny(c.s[:target], "\r\n") + 1
		c.pos = lineStart
		c.resetLine()
	}
	s := c.s
	for c.pos < target {
		b := s[c.pos]
		switch b {
		case '\r', '\n':
			c.resetLine()
		case '\\':
			c.bsRun++
		case '"', '\'':
			bs := c.bsRun
			c.bsRun = 0
			if b == '"' && bs%2 == 0 {
				c.quotes++
			}
			if bs%2 == 1 {
				qIdx := c.pos - bs
				popped := false
				for k := c.stackLen - 1; k >= 0; k-- {
					if c.stackBuf[k].bs > 0 && c.stackBuf[k].ch == b {
						if _, ok := closesQuoteAtDepth(bs, c.stackBuf[k].bs); ok {
							c.stackLen = k
							popped = true
							break
						}
					}
				}
				if !popped && isQuoteEscapeDepth(bs) && (c.stackLen == 0 || c.stackBuf[c.stackLen-1].bs < bs) && (b == '"' || c.stackLen == 0) && c.stackLen < len(c.stackBuf) {
					c.stackBuf[c.stackLen] = openQuote{idx: qIdx, bs: bs, ch: b}
					c.stackLen++
				}
			} else if b == '"' || (b == '\'' && c.stackLen == 0 && (c.pos == 0 || !isKeyIdentByte(s[c.pos-1]))) {
				// Same rules as unclosedQuotes: an unescaped single quote only
				// opens (after a non-identifier byte) and never closes.
				if c.stackLen > 0 && c.stackBuf[0].bs == 0 && c.stackBuf[0].ch == b {
					c.stackLen = 0
				} else if c.stackLen == 0 {
					c.stackBuf[0] = openQuote{idx: c.pos, bs: 0, ch: b}
					c.stackLen = 1
				}
			}
		case '{':
			c.bsRun = 0
			c.braces++
		case '}':
			c.bsRun = 0
			if c.braces > 0 {
				c.braces--
			}
		case '[':
			c.bsRun = 0
			c.brackets++
		case ']':
			c.bsRun = 0
			if c.brackets > 0 {
				c.brackets--
			}
		default:
			c.bsRun = 0
		}
		c.pos++
	}
}

func (c *lineCursor) inOuterDoubleQuote() bool {
	return c.quotes%2 == 1
}

func (c *lineCursor) activeEscapedQuoteBS() int {
	for i := c.stackLen - 1; i >= 0; i-- {
		if c.stackBuf[i].ch == '"' && c.stackBuf[i].bs > 0 {
			return c.stackBuf[i].bs
		}
	}
	return 0
}

func (c *lineCursor) inUnclosedBrace() bool {
	return c.braces > 0
}

func (c *lineCursor) inUnclosedBraceOrBracket() bool {
	return c.braces > 0 || c.brackets > 0
}

// secretKeyBeforeQuote checks whether s[:q.idx] ends right after a secret key
// assignment (e.g. `pw=`, `password: `, `-password `, or JSON `"password":`)
// and returns (isSecret, isJSON).
func secretKeyBeforeQuote(s string, q openQuote) (bool, bool) {
	p := strings.TrimRight(s[:q.idx], " \t")
	if p == "" {
		return false, false
	}
	// JSON or escaped JSON: `"password":` or `\"password\":`
	if strings.HasSuffix(p, ":") {
		beforeColon := strings.TrimRight(p[:len(p)-1], " \t")
		closeSeq := strings.Repeat(`\`, q.bs) + string(q.ch)
		if q.ch == '"' && strings.HasSuffix(beforeColon, closeSeq) {
			rem := beforeColon[:len(beforeColon)-len(closeSeq)]
			kEnd := len(rem)
			kStart := kEnd
			for kStart > 0 && isKeyIdentByte(rem[kStart-1]) {
				kStart--
			}
			if kStart < kEnd && strings.HasSuffix(rem[:kStart], closeSeq) {
				if classifyKey(rem[kStart:kEnd]) != keyNone {
					return true, true
				}
			}
		}
	}
	// Key= or Key:
	if strings.HasSuffix(p, "=") || strings.HasSuffix(p, ":") {
		beforeSep := strings.TrimRight(p[:len(p)-1], " \t")
		kEnd := len(beforeSep)
		kStart := kEnd
		for kStart > 0 && isKeyIdentByte(beforeSep[kStart-1]) {
			kStart--
		}
		if kStart < kEnd && classifyKey(beforeSep[kStart:kEnd]) != keyNone {
			return true, false
		}
	}
	// CLI flag with space: `-password "`
	if len(p) < q.idx { // had trailing space before quote
		kEnd := len(p)
		kStart := kEnd
		for kStart > 0 && isKeyIdentByte(p[kStart-1]) {
			kStart--
		}
		if kStart < kEnd && p[kStart] == '-' && classifyKey(p[kStart:kEnd]) != keyNone {
			return true, false
		}
	}
	return false, false
}

// unclosedQuotes returns the quotes (unescaped, or escaped at depth 2^d-1)
// still open at the end of s. Unlike lineCursor it deliberately does NOT reset
// on '\r' (or '\n'): a truncated head is one logical line, and a raw '\r'
// inside a quoted secret value (`pw="sec\rret …`) must not end the value.
// It is O(len(s)·depth), and depth is at most log2(len(s)) because nested
// escaped quotes need strictly increasing 2^d-1 backslash runs.
func unclosedQuotes(s string) []openQuote {
	var stack []openQuote
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' {
			j := i
			for j < len(s) && s[j] == '\\' {
				j++
			}
			bs := j - i
			if j < len(s) && (s[j] == '"' || s[j] == '\'') && bs%2 == 1 {
				ch := s[j]
				popped := false
				for k := len(stack) - 1; k >= 0; k-- {
					if stack[k].bs > 0 && stack[k].ch == ch {
						if _, ok := closesQuoteAtDepth(bs, stack[k].bs); ok {
							stack = stack[:k]
							popped = true
							break
						}
					}
				}
				if !popped && isQuoteEscapeDepth(bs) && (len(stack) == 0 || stack[len(stack)-1].bs < bs) && (ch == '"' || len(stack) == 0) {
					stack = append(stack, openQuote{idx: i, bs: bs, ch: ch})
				}
				i = j
				continue
			}
			i = j - 1
			continue
		}
		if s[i] == '"' || (s[i] == '\'' && len(stack) == 0 && (i == 0 || !isKeyIdentByte(s[i-1]))) {
			ch := s[i]
			if len(stack) > 0 && stack[0].bs == 0 && stack[0].ch == ch {
				stack = stack[:0]
			} else if len(stack) == 0 {
				stack = append(stack, openQuote{idx: i, bs: 0, ch: ch})
			}
		}
	}
	return stack
}

// writeTruncatedPrefixLocked safely truncates a giant (>64 KiB) newline-free
// prefix. If the cut lands inside a quoted value following a secret key (or an
// unquoted secret assignment), it drops back to right after the key and writes
// "[cut] [line truncated]\n".
func (rw *Writer) writeTruncatedPrefixLocked(head []byte) error {
	s := string(head)
	stack := unclosedQuotes(s)
	for idx := len(stack) - 1; idx >= 0; idx-- {
		q := stack[idx]
		if sec, isJSON := secretKeyBeforeQuote(s, q); sec {
			prefix := Text(s[:q.idx])
			if isJSON {
				qMark := strings.Repeat(`\`, q.bs) + `"`
				_, err := io.WriteString(rw.w, prefix+qMark+"[cut]"+qMark+" [line truncated]\n")
				return err
			}
			_, err := io.WriteString(rw.w, prefix+"[cut] [line truncated]\n")
			return err
		}
	}

	cutAt := bytes.LastIndexAny(head, " \t")
	if cutAt <= 0 {
		_, err := io.WriteString(rw.w, "[line truncated]\n")
		return err
	}

	// If the trailing unquoted token s[cutAt+1:] itself starts with `<secretKey>=`
	// or `<secretKey>:`, drop back to just after the key separator: `pw=[cut] [line truncated]`.
	tail := s[cutAt+1:]
	tkEnd := 0
	for tkEnd < len(tail) && isKeyIdentByte(tail[tkEnd]) {
		tkEnd++
	}
	if tkEnd > 0 && tkEnd < len(tail) && (tail[tkEnd] == '=' || tail[tkEnd] == ':') && classifyKey(tail[:tkEnd]) != keyNone {
		keepLen := cutAt + 1 + tkEnd + 1
		_, err := io.WriteString(rw.w, Text(s[:keepLen])+"[cut] [line truncated]\n")
		return err
	}

	// If head[:cutAt] ends with a dangling secret key + separator (e.g. `pw = `)
	// or a CLI secret flag (`-password `), emit `[cut]` for its truncated value.
	trimmed := strings.TrimRight(s[:cutAt], " \t")
	if strings.HasSuffix(trimmed, "=") || strings.HasSuffix(trimmed, ":") {
		beforeSep := strings.TrimRight(trimmed[:len(trimmed)-1], " \t")
		kStart := len(beforeSep)
		for kStart > 0 && isKeyIdentByte(beforeSep[kStart-1]) {
			kStart--
		}
		if kStart < len(beforeSep) && classifyKey(beforeSep[kStart:]) != keyNone {
			_, err := io.WriteString(rw.w, Text(s[:cutAt])+" [cut] [line truncated]\n")
			return err
		}
	} else {
		kStart := len(trimmed)
		for kStart > 0 && isKeyIdentByte(trimmed[kStart-1]) {
			kStart--
		}
		if kStart < len(trimmed) && trimmed[kStart] == '-' && classifyKey(trimmed[kStart:]) != keyNone {
			_, err := io.WriteString(rw.w, Text(s[:cutAt])+" [cut] [line truncated]\n")
			return err
		}
	}
	_, err := io.WriteString(rw.w, Text(s[:cutAt])+" [line truncated]\n")
	return err
}

// Flush redacts and writes any buffered trailing bytes without a newline.
func (rw *Writer) Flush() error {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if rw.dropping {
		rw.dropping = false
		rw.buf = nil
		return nil
	}
	if len(rw.buf) == 0 {
		return nil
	}
	chunk := Text(string(rw.buf))
	rw.buf = nil
	_, err := io.WriteString(rw.w, chunk)
	return err
}

type prefilterFlags struct {
	hasHash    bool // '#'
	hasPct23   bool // '%23' or '%2523'
	hasAt      bool // '@'
	hasScheme  bool // '://'
	hasKeyword bool // pass, pw, token, key, secret (case-insensitive)
	hasBearer  bool // bearer (case-insensitive)
}

func (f prefilterFlags) any() bool {
	return f.hasHash || f.hasPct23 || f.hasAt || f.hasScheme || f.hasKeyword || f.hasBearer
}

// scanPrefilter performs a single zero-allocation byte pass over s so ordinary
// log lines skip all redaction passes in under 1 µs with 0 allocs.
func scanPrefilter(s string) (f prefilterFlags) {
	n := len(s)
	for i := 0; i < n; i++ {
		c := s[i]
		if c < ':' {
			if c == '#' {
				f.hasHash = true
			} else if c == '%' && i+2 < n && s[i+1] == '2' {
				if s[i+2] == '3' || (s[i+2] == '5' && i+4 < n && s[i+3] == '2' && s[i+4] == '3') {
					f.hasPct23 = true
				}
			}
			continue
		}
		if c <= '@' {
			if c == ':' && i+2 < n && s[i+1] == '/' && s[i+2] == '/' {
				f.hasScheme = true
			} else if c == '@' {
				f.hasAt = true
			}
			continue
		}
		lo := c | 0x20
		if lo < 'b' || lo > 't' {
			continue
		}
		switch lo {
		case 'p':
			if !f.hasKeyword && i+1 < n {
				n1 := s[i+1] | 0x20
				if n1 == 'w' || (n1 == 'a' && i+3 < n && (s[i+2]|0x20) == 's' && (s[i+3]|0x20) == 's') {
					f.hasKeyword = true
				}
			}
		case 't':
			if !f.hasKeyword && i+4 < n && (s[i+1]|0x20) == 'o' && (s[i+2]|0x20) == 'k' && (s[i+3]|0x20) == 'e' && (s[i+4]|0x20) == 'n' {
				f.hasKeyword = true
			}
		case 'k':
			if !f.hasKeyword && i+2 < n && (s[i+1]|0x20) == 'e' && (s[i+2]|0x20) == 'y' {
				f.hasKeyword = true
			}
		case 's':
			if !f.hasKeyword && i+5 < n && (s[i+1]|0x20) == 'e' && (s[i+2]|0x20) == 'c' && (s[i+3]|0x20) == 'r' && (s[i+4]|0x20) == 'e' && (s[i+5]|0x20) == 't' {
				f.hasKeyword = true
			}
		case 'b':
			if !f.hasBearer && i+5 < n && (s[i+1]|0x20) == 'e' && (s[i+2]|0x20) == 'a' && (s[i+3]|0x20) == 'r' && (s[i+4]|0x20) == 'e' && (s[i+5]|0x20) == 'r' {
				f.hasBearer = true
			}
		}
	}
	return f
}

func hasFoldAt(s string, idx int, lowerWord string) bool {
	if idx+len(lowerWord) > len(s) {
		return false
	}
	for j := 0; j < len(lowerWord); j++ {
		if (s[idx+j] | 0x20) != lowerWord[j] {
			return false
		}
	}
	return true
}

// Text removes secret fields, Bearer tokens, URL credentials, secret query
// parameters, share-link fragments (# and %23 on any path), and bare CODE#pw
// join inputs from log text.
func Text(s string) string {
	if s == "" {
		return ""
	}
	pf := scanPrefilter(s)
	if !pf.any() {
		return s
	}
	return redactTextGuarded(s, pf)
}

// redactionFailed replaces a line (or value) whose redaction panicked. The
// raw input is never returned in that case.
const redactionFailed = "[redaction failed]"

// failedFor returns the redactionFailed marker, keeping a trailing newline so
// a line-oriented log stays line-oriented.
func failedFor(s string) string {
	if strings.HasSuffix(s, "\n") {
		return redactionFailed + "\n"
	}
	return redactionFailed
}

// redactTextGuarded runs the redaction passes behind a recover() safety net:
// a bug in a pass must neither crash the process (Writer runs this under its
// mutex, inside the logger) nor fall back to the unredacted line.
func redactTextGuarded(s string, pf prefilterFlags) (out string) {
	defer func() {
		if recover() != nil {
			out = failedFor(s)
		}
	}()
	return redactText(s, pf)
}

// testPassHook, when set (tests only), runs before the redaction passes so a
// test can force a panic and check the recover() safety nets.
var testPassHook func(string)

func redactText(s string, pf prefilterFlags) string {
	if testPassHook != nil {
		testPassHook(s)
	}
	if pf.hasKeyword {
		s = redactQueryParams(s)
		s = redactSecretFields(s)
	}
	if pf.hasBearer {
		s = redactBearerTokens(s)
	}
	if pf.hasAt {
		s = redactUserinfo(s)
	}
	if pf.hasHash || pf.hasPct23 {
		s = redactFragments(s, pf.hasPct23)
	}
	return s
}

func isKeyIdentByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') || c == '_' || c == '.' || c == '-'
}

func isHexByte(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func parseHexUint(s string) (uint32, bool) {
	var v uint32
	for i := 0; i < len(s); i++ {
		c := s[i]
		v <<= 4
		switch {
		case c >= '0' && c <= '9':
			v |= uint32(c - '0')
		case c >= 'a' && c <= 'f':
			v |= uint32(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v |= uint32(c-'A') + 10
		default:
			return 0, false
		}
	}
	return v, true
}

func isUnicodeSpaceCodepoint(cp uint32) bool {
	if cp <= 0x20 || cp == 0x7f || cp == 0x85 || cp == 0x00a0 {
		return true
	}
	switch cp {
	case 0x1680, 0x200b, 0x200c, 0x200d, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return cp >= 0x2000 && cp <= 0x200a
}

// consumeEscape parses a backslash escape sequence starting at s[i] == '\\' at
// ANY backslash depth (e.g. \n, \\n, \\\n, \t, \u000a, \\u000a, \x0a, \", \\\").
func consumeEscape(s string, i int) (end int, isWs bool, isQuote bool, quoteBS int, quoteCh byte, isBoundary bool) {
	if i >= len(s) || s[i] != '\\' {
		return i, false, false, 0, 0, false
	}
	j := i
	for j < len(s) && s[j] == '\\' {
		j++
	}
	bs := j - i
	if j >= len(s) {
		return j, false, false, 0, 0, true
	}
	c := s[j]
	switch c {
	case 'n', 'r', 't', 'v', 'f', '0':
		return j + 1, true, false, 0, 0, true
	case '"', '\'':
		if bs%2 == 1 {
			return j + 1, false, true, bs, c, true
		}
		// Even backslashes before quote: the backslashes themselves end at j,
		// leaving the unescaped quote at j to be handled by the caller.
		return j, false, false, 0, 0, true
	case 'x', 'X':
		if j+3 <= len(s) {
			if cp, ok := parseHexUint(s[j+1 : j+3]); ok {
				b := byte(cp)
				return j + 3, isUnicodeSpaceCodepoint(cp), false, 0, 0, !isKeyIdentByte(b)
			}
		}
	case 'u':
		if j+5 <= len(s) {
			if cp, ok := parseHexUint(s[j+1 : j+5]); ok {
				b := byte(cp)
				return j + 5, isUnicodeSpaceCodepoint(cp), false, 0, 0, cp >= 0x80 || !isKeyIdentByte(b)
			}
		}
	case 'U':
		if j+9 <= len(s) {
			if cp, ok := parseHexUint(s[j+1 : j+9]); ok {
				b := byte(cp)
				return j + 9, isUnicodeSpaceCodepoint(cp), false, 0, 0, cp >= 0x80 || !isKeyIdentByte(b)
			}
		}
	}
	return j, false, false, 0, 0, true
}

// isHardWhitespaceAt reports whether s[i:] starts with an ASCII control/space
// byte, a UTF-8 non-ASCII space/separator rune (NBSP, ZWSP, U+2028, etc.), or a
// literal escape sequence representing whitespace (\n, \\n, \u000a, \x0a, etc.).
func isHardWhitespaceAt(s string, i int) (int, bool) {
	if i >= len(s) {
		return i, false
	}
	c := s[i]
	if c <= 0x20 || c == 0x7f {
		return i + 1, true
	}
	if c == '\\' {
		end, isWs, _, _, _, _ := consumeEscape(s, i)
		if isWs {
			return end, true
		}
		return i, false
	}
	if c >= 0x80 {
		// Check 2-byte UTF-8 NBSP (C2 A0) and NEL (C2 85).
		if c == 0xc2 && i+1 < len(s) && (s[i+1] == 0xa0 || s[i+1] == 0x85) {
			return i + 2, true
		}
		// Check 3-byte UTF-8 spaces: U+2000..U+200D, U+2028, U+2029, U+202F, U+205F, U+3000, U+FEFF.
		if i+2 < len(s) {
			b1, b2 := s[i+1], s[i+2]
			switch c {
			case 0xe2:
				if b1 == 0x80 && ((b2 >= 0x80 && b2 <= 0x8d) || b2 == 0xa8 || b2 == 0xa9 || b2 == 0xaf) {
					return i + 3, true
				}
				if b1 == 0x81 && b2 == 0x9f {
					return i + 3, true
				}
			case 0xe3:
				if b1 == 0x80 && b2 == 0x80 {
					return i + 3, true
				}
			case 0xef:
				if b1 == 0xbb && b2 == 0xbf {
					return i + 3, true
				}
			}
		}
	}
	return i, false
}

// scanQuotedEnd scans a quoted string whose content begins at contentStart and
// whose opening quote was quoteCh preceded by openBS backslashes.
func scanQuotedEnd(s string, contentStart int, quoteCh byte, openBS int) (contentEnd int, afterClose int, closed bool) {
	for i := contentStart; i < len(s); i++ {
		if s[i] == '\r' || s[i] == '\n' {
			return i, i, false
		}
		if s[i] == quoteCh {
			cbs := countBackslashesBefore(s, i)
			if usedBS, ok := closesQuoteAtDepth(cbs, openBS); ok {
				if openBS > 0 && cbs < openBS {
					return i - usedBS, i - usedBS, true
				}
				return i - usedBS, i + 1, true
			}
		}
	}
	return len(s), len(s), false
}

// redactSecretFields scans s left-to-right on structural non-[A-Za-z0-9_.-]
// and escape boundaries, redacting JSON ("k":"v", "k":123, \"k\":4821),
// key=val / key = val, key:val / key : val, and CLI -flag val / "-flag" "val".
func redactSecretFields(s string) string {
	var b strings.Builder
	var cur lineCursor
	cur.init(s)
	pos := 0
	i := 0
	forcedBoundary := true

	for i < len(s) {
		if s[i] == '\\' {
			escEnd, _, isQ, qBS, qCh, isB := consumeEscape(s, i)
			// Check escaped JSON or escaped CLI flag pair starting with \"key\"...
			if isQ && qCh == '"' && escEnd < len(s) && isKeyIdentByte(s[escEnd]) {
				if nextIdx, repl, ok := tryQuotedKeyForm(s, i, escEnd, qBS); ok {
					if pos == 0 {
						b.Grow(len(s))
					}
					b.WriteString(s[pos:i])
					b.WriteString(repl)
					pos = nextIdx
					i = nextIdx
					forcedBoundary = true
					continue
				}
			}
			i = escEnd
			forcedBoundary = isB
			continue
		}

		atBoundary := forcedBoundary || i == 0 || !isKeyIdentByte(s[i-1])
		forcedBoundary = false

		if atBoundary && isKeyIdentByte(s[i]) {
			// Check unescaped JSON `"key":...` or Go %q `"-flag" "val"` when preceded by `"`.
			if i >= 1 && s[i-1] == '"' && countBackslashesBefore(s, i-1)%2 == 0 {
				if nextIdx, repl, ok := tryQuotedKeyForm(s, i-1, i, 0); ok {
					if pos == 0 {
						b.Grow(len(s))
					}
					if i-1 >= pos {
						b.WriteString(s[pos : i-1])
						b.WriteString(repl)
					} else {
						// The opening quote was already emitted as the closing
						// quote of the previous replacement (`"pw":""pw":1`).
						b.WriteString(repl[pos-(i-1):])
					}
					pos = nextIdx
					i = nextIdx
					forcedBoundary = true
					continue
				}
			}

			kEnd := i
			for kEnd < len(s) && isKeyIdentByte(s[kEnd]) {
				kEnd++
			}
			// Skip URL query parameters (?k=v, &k=v, ;k=v), which redactQueryParams
			// already redacted up to '&'/';'/'#'/closing quote.
			if i >= 1 && (s[i-1] == '?' || s[i-1] == '&' || s[i-1] == ';') && kEnd < len(s) && s[kEnd] == '=' {
				i = kEnd
				continue
			}
			rawKey := s[i:kEnd]
			if kind := classifyKey(rawKey); kind != keyNone {
				if valStart, valEnd, ok := matchKeyValueAfter(s, i, kEnd, rawKey, kind, &cur); ok {
					if pos == 0 {
						b.Grow(len(s) + len(s)/4)
					}
					b.WriteString(s[pos:valStart])
					b.WriteString("[cut]")
					pos = valEnd
					i = valEnd
					forcedBoundary = true
					continue
				}
			}
			i = kEnd
			continue
		}
		i++
	}

	if pos == 0 {
		return s
	}
	b.WriteString(s[pos:])
	return b.String()
}

// tryQuotedKeyForm handles `"key": val` / `\"key\": val` (JSON strings and
// primitives) and `"-flag" "val"` / `\"-flag\" \"val\"`.
func tryQuotedKeyForm(s string, openQuoteIdx, keyStart, openBS int) (nextIdx int, repl string, ok bool) {
	kEnd := keyStart
	for kEnd < len(s) && isKeyIdentByte(s[kEnd]) {
		kEnd++
	}
	if kEnd == keyStart {
		return 0, "", false
	}
	rawKey := s[keyStart:kEnd]
	kind := classifyKey(rawKey)
	if kind == keyNone {
		return 0, "", false
	}
	qMark := strings.Repeat(`\`, openBS) + `"`
	if !strings.HasPrefix(s[kEnd:], qMark) {
		return 0, "", false
	}
	afterKey := kEnd + len(qMark)

	// Form S2: `"-password" "secret"` or `\"-password\" \"secret\"`.
	if rawKey[0] == '-' && strings.TrimLeft(rawKey, "-") != "" {
		sp := afterKey
		for sp < len(s) && (s[sp] == ' ' || s[sp] == '\t') {
			sp++
		}
		if sp > afterKey && strings.HasPrefix(s[sp:], qMark) {
			valContentStart := sp + len(qMark)
			_, afterVal, _ := scanQuotedEnd(s, valContentStart, '"', openBS)
			return afterVal, s[openQuoteIdx:valContentStart] + "[cut]" + qMark, true
		}
	}

	// Form S1: JSON `"key" : "val"` or `"key" : 123`.
	sp := afterKey
	for sp < len(s) && (s[sp] == ' ' || s[sp] == '\t') {
		sp++
	}
	if sp >= len(s) || s[sp] != ':' {
		return 0, "", false
	}
	valStart := sp + 1
	for valStart < len(s) && (s[valStart] == ' ' || s[valStart] == '\t') {
		valStart++
	}
	if valStart >= len(s) {
		return 0, "", false
	}
	if strings.HasPrefix(s[valStart:], qMark) {
		valContentStart := valStart + len(qMark)
		_, afterVal, _ := scanQuotedEnd(s, valContentStart, '"', openBS)
		return afterVal, s[openQuoteIdx:valContentStart] + "[cut]" + qMark, true
	}
	// JSON primitive (number, boolean, etc.).
	litEnd := valStart
	for litEnd < len(s) && (isKeyIdentByte(s[litEnd]) || s[litEnd] == '+') {
		litEnd++
	}
	if litEnd == valStart {
		return 0, "", false
	}
	lit := s[valStart:litEnd]
	if lit == "null" {
		return 0, "", false
	}
	if kind == keyKeyOnly && isAllowedKeyNonSecretLiteral(lit) {
		return 0, "", false
	}
	return litEnd, s[openQuoteIdx:valStart] + qMark + "[cut]" + qMark, true
}

// matchKeyValueAfter matches `= val`, `: val` (with optional spaces around `=`/`:`),
// and CLI `-flag val` after rawKey at s[keyStart:keyEnd].
func matchKeyValueAfter(s string, keyStart, keyEnd int, rawKey string, kind keyKind, cur *lineCursor) (valStart, valEnd int, ok bool) {
	sp := keyEnd
	for sp < len(s) && (s[sp] == ' ' || s[sp] == '\t') {
		sp++
	}
	hasSep := false
	var sep byte
	if sp < len(s) && (s[sp] == '=' || s[sp] == ':') {
		// Don't treat URL schemes like `secret://...` as `secret: //`.
		if s[sp] == ':' && strings.HasPrefix(s[sp:], "://") {
			return 0, 0, false
		}
		hasSep = true
		sep = s[sp]
		valStart = sp + 1
		for valStart < len(s) && (s[valStart] == ' ' || s[valStart] == '\t') {
			valStart++
		}
	} else if rawKey[0] == '-' && strings.TrimLeft(rawKey, "-") != "" && sp > keyEnd {
		// Plain-text CLI flag with space: `-password pw`.
		valStart = sp
	} else {
		return 0, 0, false
	}

	if valStart >= len(s) {
		return 0, 0, false
	}
	if _, ws := isHardWhitespaceAt(s, valStart); ws {
		return 0, 0, false
	}

	// Quoted value: `"..."`, `\"...\"`, or `'...'`.
	if s[valStart] == '"' {
		_, afterClose, _ := scanQuotedEnd(s, valStart+1, '"', 0)
		return valStart, afterClose, true
	}
	if s[valStart] == '\'' {
		_, afterClose, _ := scanQuotedEnd(s, valStart+1, '\'', 0)
		return valStart, afterClose, true
	}
	if s[valStart] == '\\' {
		escEnd, _, isQ, qBS, qCh, _ := consumeEscape(s, valStart)
		if isQ && (qCh == '"' || qCh == '\'') {
			_, afterClose, _ := scanQuotedEnd(s, escEnd, qCh, qBS)
			return valStart, afterClose, true
		}
	}

	// Unquoted value.
	cur.advanceTo(keyStart)
	outerQuote := cur.inOuterDoubleQuote()
	escQuoteBS := cur.activeEscapedQuoteBS()
	parenWrap := keyStart >= 1 && s[keyStart-1] == '('
	braceStop := hasSep && sep == ':' && cur.inUnclosedBraceOrBracket()

	// In Go %+v struct output (`{Room:ABCD Password: NextField:val}`), an empty
	// string field prints `Password: ` followed by `NextField:val`. Don't treat
	// the next struct field name as the value of `Password:`.
	if braceStop && sp == keyEnd && valStart > sp+1 {
		nf := valStart
		for nf < len(s) && (isAlphaASCII(s[nf]) || (nf > valStart && (s[nf] >= '0' && s[nf] <= '9' || s[nf] == '_'))) {
			nf++
		}
		if nf > valStart && nf < len(s) && s[nf] == ':' && !strings.HasPrefix(s[nf:], "://") {
			return 0, 0, false
		}
	}

	// Preserve already-redacted `[cut]` idempotently (even inside `[...]`/`{...}`).
	if strings.HasPrefix(s[valStart:], "[cut]") {
		after := valStart + len("[cut]")
		if after == len(s) {
			return 0, 0, false
		}
		if _, ws := isHardWhitespaceAt(s, after); ws {
			return 0, 0, false
		}
		switch s[after] {
		case '"', '\'', '}', ']', ')', ',', ';', '\\':
			return 0, 0, false
		}
	}

	i := valStart
	for i < len(s) {
		if _, ws := isHardWhitespaceAt(s, i); ws {
			break
		}
		c := s[i]
		if c == '\\' {
			escEnd, isWs, isQ, qBS, qCh, _ := consumeEscape(s, i)
			if isWs {
				break
			}
			// If key itself was inside an escaped quote `\"password=foo\"`,
			// stop before the matching closing `\"`.
			if isQ && qCh == '"' && escQuoteBS > 0 {
				if usedBS, ok := closesQuoteAtDepth(qBS, escQuoteBS); ok {
					i += qBS - usedBS
					break
				}
			}
			if escEnd > i {
				i = escEnd
				continue
			}
		}
		if c == '"' && outerQuote {
			break
		}
		if braceStop && (c == '}' || c == ']' || c == ',' || c == ';') {
			break
		}
		if parenWrap && c == ')' {
			break
		}
		i++
	}
	if i == valStart {
		return 0, 0, false
	}
	rawVal := s[valStart:i]
	if kind == keyKeyOnly && isAllowedKeyNonSecretLiteral(rawVal) {
		return 0, 0, false
	}
	return valStart, i, true
}

func redactBearerTokens(s string) string {
	var b strings.Builder
	pos := 0
	i := 0
	forcedBoundary := true
	for i < len(s) {
		if s[i] == '\\' {
			escEnd, _, _, _, _, isB := consumeEscape(s, i)
			if escEnd <= i {
				i++
				forcedBoundary = true
				continue
			}
			i = escEnd
			forcedBoundary = isB
			continue
		}
		atBoundary := forcedBoundary || i == 0 || !isKeyIdentByte(s[i-1])
		forcedBoundary = false
		if atBoundary && hasFoldAt(s, i, "bearer") && i+6 < len(s) && (s[i+6] == ' ' || s[i+6] == '\t') {
			valStart := i + 6
			for valStart < len(s) && (s[valStart] == ' ' || s[valStart] == '\t') {
				valStart++
			}
			if strings.HasPrefix(s[valStart:], "[cut]") {
				after := valStart + len("[cut]")
				if after == len(s) {
					break
				}
				if _, ws := isHardWhitespaceAt(s, after); ws {
					i = after
					forcedBoundary = true
					continue
				}
				switch s[after] {
				case '"', '\'', '<', '>', ')', ']', '}', ',', ';', '\\':
					i = after
					forcedBoundary = true
					continue
				}
			}
			valEnd := valStart
			for valEnd < len(s) {
				if _, ws := isHardWhitespaceAt(s, valEnd); ws {
					break
				}
				c := s[valEnd]
				if c == '\\' {
					escEnd, isWs, isQ, _, _, _ := consumeEscape(s, valEnd)
					if isWs || isQ {
						break
					}
					if escEnd > valEnd {
						valEnd = escEnd
						continue
					}
				}
				if c == '"' || c == '\'' || c == '<' || c == '>' || c == ')' || c == ']' || c == '}' || c == ',' || c == ';' {
					break
				}
				valEnd++
			}
			if valEnd > valStart {
				if s[valStart:valEnd] != "[cut]" {
					b.WriteString(s[pos:valStart])
					b.WriteString("[cut]")
					pos = valEnd
				}
				i = valEnd
				forcedBoundary = true
				continue
			}
		}
		i++
	}
	if pos == 0 {
		return s
	}
	b.WriteString(s[pos:])
	return b.String()
}

func redactUserinfo(s string) string {
	s = redactSchemeUserinfo(s)
	return redactBareUserinfo(s)
}

func redactSchemeUserinfo(s string) string {
	if strings.IndexByte(s, '@') < 0 || !strings.Contains(s, "://") {
		return s
	}
	var b strings.Builder
	var cur lineCursor
	cur.init(s)
	pos := 0
	searchFrom := 0
	for searchFrom < len(s) {
		rel := strings.Index(s[searchFrom:], "://")
		if rel < 0 {
			break
		}
		sep := searchFrom + rel
		schStart := sep
		for schStart > 0 && isSchemeByte(s[schStart-1]) {
			schStart--
		}
		if schStart == sep || !isAlphaASCII(s[schStart]) {
			searchFrom = sep + 3
			continue
		}
		authStart := sep + 3
		authEnd := scanAuthorityEnd(s, authStart, schStart, &cur)
		lastAt := strings.LastIndexByte(s[authStart:authEnd], '@')
		if lastAt > 0 && authStart+lastAt+1 < authEnd {
			uinfo := s[authStart : authStart+lastAt]
			if uinfo != "[cut]" {
				if pos == 0 {
					b.Grow(len(s))
				}
				b.WriteString(s[pos:authStart])
				b.WriteString("[cut]")
				pos = authStart + lastAt
			}
		}
		searchFrom = authEnd
		if searchFrom <= sep+3 {
			searchFrom = sep + 3
		}
	}
	if pos == 0 {
		return s
	}
	b.WriteString(s[pos:])
	return b.String()
}

func redactBareUserinfo(s string) string {
	if strings.IndexByte(s, '@') < 0 {
		return s
	}
	var b strings.Builder
	var cur lineCursor
	cur.init(s)
	memo := newAuthorityMemo(s)
	pos := 0
	i := 0
	forcedBoundary := true
	for i < len(s) {
		if i >= 3 && s[i-3:i] == "://" {
			end := scanAuthorityEnd(s, i, i-3, &cur)
			if end > i {
				i = end
				forcedBoundary = true
				continue
			}
		}
		if s[i] == '\\' {
			escEnd, _, _, _, _, isB := consumeEscape(s, i)
			if escEnd <= i {
				i++
				forcedBoundary = true
				continue
			}
			i = escEnd
			forcedBoundary = isB
			continue
		}
		atBoundary := (forcedBoundary || i == 0 || !isKeyIdentByte(s[i-1])) && (i == 0 || s[i-1] != '@')
		forcedBoundary = false
		if atBoundary {
			if strings.HasPrefix(s[i:], "[cut]@") {
				i += len("[cut]@")
				continue
			}
			uEnd := i
			for uEnd < len(s) && isBareUserByte(s[uEnd]) {
				uEnd++
			}
			if uEnd > i && uEnd < len(s) && s[uEnd] == ':' && !strings.HasPrefix(s[uEnd:], "://") && !strings.EqualFold(s[i:uEnd], "mailto") {
				authEnd, lastAt, memoEntry := memo.scan(&cur, uEnd+1, i)
				if lastAt > uEnd+1 {
					atIdx := lastAt
					hostEnd, hostOK := memo.host(memoEntry, atIdx, authEnd)
					if hostOK {
						if pos == 0 {
							b.Grow(len(s))
						}
						b.WriteString(s[pos:i])
						b.WriteString("[cut]")
						pos = atIdx
						i = hostEnd
						forcedBoundary = true
						continue
					}
				}
			}
			if uEnd > i {
				i = uEnd
				continue
			}
		}
		i++
	}
	if pos == 0 {
		return s
	}
	b.WriteString(s[pos:])
	return b.String()
}

// authorityMemo makes redactBareUserinfo linear on lines such as
// `us%40er:p@ss@` or `x{a:\"@` repeated: every `user:` boundary used to rescan
// the rest of the authority (and LastIndexByte it for '@'), which is O(n) per
// boundary.
//
// scanAuthorityEnd carries no state besides its position and the top of the
// quote stack at quoteAnchor, and a scan visits every ':' as a plain byte (a
// ':' is never part of an escape sequence or a skipped quote). So a scan that
// starts right after a ':' lying inside an earlier scanned range, with the
// same quote-stack top, ends exactly where that scan ended. One entry is kept
// per quote-stack top (escaped quotes nest at depths 2^d-1, so there are few),
// which keeps the memo hitting when the state alternates between boundaries
// (a repeated `\"` flips it at every `user:`). Scan starts only increase, so
// each state scans every byte at most once: O(n·states) in total. The last
// '@' before the end and the host check are memoized per entry as well.
type authorityMemo struct {
	s       string
	entries [2 + 2*authMemoMaxDepth]authEntry
}

// authMemoMaxDepth covers escaped-quote depths d (2^d-1 backslashes) up to
// 2^24-1 backslashes; deeper states just scan without a memo.
const authMemoMaxDepth = 24

type authEntry struct {
	start   int // previous scan start (-1: none)
	end     int // previous scan end
	lastAt  int // last '@' in s[start:end], or -1
	hostAt  int // memo key for host(): atIdx (-1: none)
	hostEnd int
	hostOK  bool
}

func newAuthorityMemo(s string) authorityMemo {
	m := authorityMemo{s: s}
	for k := range m.entries {
		m.entries[k].start, m.entries[k].hostAt = -1, -1
	}
	return m
}

// scan returns scanAuthorityEnd(s, start, quoteAnchor), the last '@' in
// s[start:end] (or -1), and the memo entry to pass to host (-1: none).
func (m *authorityMemo) scan(cur *lineCursor, start, quoteAnchor int) (end, lastAt, entry int) {
	s := m.s
	cur.advanceTo(quoteAnchor)
	entry = 0
	if stack := cur.openQuotes(); len(stack) > 0 {
		top := stack[len(stack)-1]
		entry = -1
		if d := bits.Len(uint(top.bs)); d <= authMemoMaxDepth {
			entry = 1 + 2*d
			if top.ch == '\'' {
				entry++
			}
		}
	}
	if entry >= 0 {
		e := &m.entries[entry]
		if e.start >= 0 && start > e.start && start < e.end && s[start-1] == ':' {
			if e.lastAt >= start {
				return e.end, e.lastAt, entry
			}
			return e.end, -1, entry
		}
	}
	end = scanAuthorityEnd(s, start, quoteAnchor, cur)
	lastAt = -1
	if rel := strings.LastIndexByte(s[start:end], '@'); rel >= 0 {
		lastAt = start + rel
	}
	if entry >= 0 {
		m.entries[entry] = authEntry{start: start, end: end, lastAt: lastAt, hostAt: -1}
	}
	return end, lastAt, entry
}

// host trims trailing `,;)}]` after the '@' at atIdx and reports whether the
// rest of s[atIdx+1:authEnd] is host-like. authEnd is the end of the scan
// that produced entry.
func (m *authorityMemo) host(entry, atIdx, authEnd int) (int, bool) {
	var e *authEntry
	if entry >= 0 {
		e = &m.entries[entry]
		if e.hostAt == atIdx {
			return e.hostEnd, e.hostOK
		}
	}
	s := m.s
	hostEnd := authEnd
	for hostEnd > atIdx+1 {
		last := s[hostEnd-1]
		if last == ',' || last == ';' || last == ')' || last == '}' || (last == ']' && s[atIdx+1] != '[') {
			hostEnd--
			continue
		}
		break
	}
	ok := hostEnd > atIdx+1 && isHostLike(s[atIdx+1:hostEnd])
	if e != nil {
		e.hostAt, e.hostEnd, e.hostOK = atIdx, hostEnd, ok
	}
	return hostEnd, ok
}

func scanAuthorityEnd(s string, start, quoteAnchor int, cur *lineCursor) int {
	cur.advanceTo(quoteAnchor)
	stack := cur.openQuotes()
	i := start
	for i < len(s) {
		c := s[i]
		if c == '/' || c == '?' || c == '#' || c == '<' || c == '>' {
			break
		}
		if hasPct23At(s, i) {
			break
		}
		if _, ws := isHardWhitespaceAt(s, i); ws {
			break
		}
		if c == '\\' {
			escEnd, isWs, _, _, _, _ := consumeEscape(s, i)
			if isWs {
				break
			}
			j := i
			for j < len(s) && s[j] == '\\' {
				j++
			}
			cbs := j - i
			if j < len(s) && (s[j] == '"' || s[j] == '\'') {
				if len(stack) > 0 {
					top := stack[len(stack)-1]
					if s[j] == top.ch {
						if usedBS, ok := closesQuoteAtDepth(cbs, top.bs); ok {
							return j - usedBS
						}
					}
				} else if cbs%2 == 0 {
					return j
				}
				i = j + 1
				continue
			}
			if escEnd > i {
				i = escEnd
				continue
			}
		}
		if c == '"' || c == '\'' {
			if len(stack) > 0 {
				top := stack[len(stack)-1]
				if c == top.ch {
					return i
				}
			} else {
				return i
			}
		}
		i++
	}
	return i
}

func isAlphaASCII(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isSchemeByte(c byte) bool {
	return isAlphaASCII(c) || (c >= '0' && c <= '9') || c == '+' || c == '.' || c == '-'
}

func isBareUserByte(c byte) bool {
	return isAlphaASCII(c) || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '%' || c == '~' || c == '-'
}

func hasBareUserinfo(s string) bool {
	if strings.IndexByte(s, '@') < 0 {
		return false
	}
	return strings.HasPrefix(s, "[cut]@") || redactBareUserinfo(s) != s
}

func redactQueryParams(s string) string {
	if !strings.ContainsAny(s, "?&;") {
		return s
	}
	var b strings.Builder
	var cur lineCursor
	cur.init(s)
	pos := 0
	urlStart := 0
	urlScannedTo := 0
	cachedURLStart := -1
	cachedOpenBS := 0

	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '?' && c != '&' && c != ';' {
			continue
		}
		kStart := i + 1
		kEnd := kStart
		for kEnd < len(s) && isKeyIdentByte(s[kEnd]) {
			kEnd++
		}
		if kEnd == kStart || kEnd >= len(s) || s[kEnd] != '=' {
			continue
		}
		if classifyKey(s[kStart:kEnd]) == keyNone {
			continue
		}
		for urlScannedTo < i {
			u := s[urlScannedTo]
			if u <= 0x20 || u >= 0x7f || u == '"' || u == '\'' || u == '`' || u == '<' || u == '>' || u == '(' || u == '{' || u == '\\' {
				urlStart = urlScannedTo + 1
			}
			urlScannedTo++
		}
		if urlStart != cachedURLStart {
			cachedURLStart = urlStart
			if urlStart >= 1 && s[urlStart-1] == '"' {
				cachedOpenBS = countBackslashesBefore(s, urlStart-1)
			} else {
				cachedOpenBS = 0
			}
		}
		eqEnd := kEnd + 1
		end := scanQueryValueEnd(s, urlStart, cachedOpenBS, eqEnd, &cur)
		if end == eqEnd {
			continue
		}
		val := s[eqEnd:end]
		if val == "[cut]" {
			i = end - 1
			continue
		}
		if pos == 0 {
			b.Grow(len(s) + len(s)/4)
		}
		b.WriteString(s[pos:eqEnd])
		if strings.HasSuffix(val, ":") && (end == len(s) || s[end] == ' ' || s[end] == '\t') {
			b.WriteString("[cut]:")
		} else {
			b.WriteString("[cut]")
		}
		pos = end
		i = end - 1
	}
	if pos == 0 {
		return s
	}
	b.WriteString(s[pos:])
	return b.String()
}

func scanQueryValueEnd(s string, urlStart, openBS, valStart int, cur *lineCursor) int {
	i := valStart
	// Quoted URL value: runs to the next '&'/';', fragment ('#'/'%23'), or closing quote.
	if urlStart >= 1 && s[urlStart-1] == '"' {
		for i < len(s) {
			if s[i] == '&' || s[i] == ';' || s[i] == '#' || s[i] == '\r' || s[i] == '\n' {
				break
			}
			if hasPct23At(s, i) {
				break
			}
			if s[i] == '\\' {
				_, isWs, _, _, _, _ := consumeEscape(s, i)
				if isWs {
					break
				}
			}
			if s[i] == '"' {
				cbs := countBackslashesBefore(s, i)
				if usedBS, ok := closesQuoteAtDepth(cbs, openBS); ok {
					return i - usedBS
				}
			}
			i++
		}
		return i
	}
	if urlStart >= 1 && s[urlStart-1] == '\'' {
		cur.advanceTo(urlStart)
		outerQuote := cur.inOuterDoubleQuote()
		for i < len(s) {
			if s[i] == '&' || s[i] == ';' || s[i] == '#' || s[i] == '\'' || s[i] == '\r' || s[i] == '\n' {
				break
			}
			if hasPct23At(s, i) {
				break
			}
			if s[i] == '"' && countBackslashesBefore(s, i)%2 == 0 && outerQuote {
				break
			}
			i++
		}
		return i
	}

	cur.advanceTo(urlStart)
	outerQuote := cur.inOuterDoubleQuote()
	escQuoteBS := cur.activeEscapedQuoteBS()
	for i < len(s) {
		c := s[i]
		if c == '&' || c == ';' || c == '#' {
			break
		}
		if hasPct23At(s, i) {
			break
		}
		if _, ws := isHardWhitespaceAt(s, i); ws {
			break
		}
		if c == '\\' {
			escEnd, isWs, isQ, qBS, qCh, _ := consumeEscape(s, i)
			if isWs {
				break
			}
			if isQ && qCh == '"' && escQuoteBS > 0 {
				if usedBS, ok := closesQuoteAtDepth(qBS, escQuoteBS); ok {
					return i + qBS - usedBS
				}
			}
			if escEnd > i {
				i = escEnd
				continue
			}
		}
		if c == '"' && outerQuote {
			break
		}
		i++
	}
	return i
}

func hasPct23At(s string, i int) bool {
	if i+3 <= len(s) && s[i] == '%' && s[i+1] == '2' {
		if s[i+2] == '3' {
			return true
		}
		if s[i+2] == '5' && i+5 <= len(s) && s[i+3] == '2' && s[i+4] == '3' {
			return true
		}
	}
	return false
}

// URL redacts userinfo, secret query parameters, and any '#' or '%23' fragment
// on a single URL, share link, or CODE#pw join string, keeping the host/path
// and room code intact.
func URL(s string) (out string) {
	if s == "" {
		return ""
	}
	defer func() {
		if recover() != nil {
			out = redactionFailed
		}
	}()
	return redactURL(s)
}

// redactURL is URL without the recover() safety net.
func redactURL(s string) string {
	if testPassHook != nil {
		testPassHook(s)
	}
	s = redactUserinfo(s)
	if idx, _ := nextFragMarker(s); idx >= 0 {
		head := redactURLQueryAll(s[:idx])
		return head + "#[cut]"
	}
	return redactURLQueryAll(s)
}

func redactURLQueryAll(s string) string {
	if !strings.ContainsAny(s, "?&;") {
		return s
	}
	var b strings.Builder
	pos := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '?' && s[i] != '&' && s[i] != ';' {
			continue
		}
		kStart := i + 1
		kEnd := kStart
		for kEnd < len(s) && isKeyIdentByte(s[kEnd]) {
			kEnd++
		}
		if kEnd == kStart || kEnd >= len(s) || s[kEnd] != '=' {
			continue
		}
		if classifyKey(s[kStart:kEnd]) == keyNone {
			continue
		}
		eqEnd := kEnd + 1
		end := eqEnd
		for end < len(s) && s[end] != '&' && s[end] != ';' {
			end++
		}
		if end == eqEnd {
			continue
		}
		b.WriteString(s[pos:eqEnd])
		b.WriteString("[cut]")
		pos = end
		i = end - 1
	}
	if pos == 0 {
		return s
	}
	b.WriteString(s[pos:])
	return b.String()
}

// Args returns a copy of CLI arguments with URL/share-link/join-code fragments,
// URL credentials, and secret flag values redacted.
func Args(args []string) []string {
	if len(args) == 0 {
		return nil
	}
	out := make([]string, len(args))
	copy(out, args)
	for i := 0; i < len(out); i++ {
		a := out[i]
		if flag, val, ok := cutFlagEq(a); ok {
			lf := strings.ToLower(flag)
			if classifyKey(lf) != keyNone {
				out[i] = flag + "=[cut]"
				continue
			}
			switch lf {
			case "-link", "--link", "-join", "--join", "-server", "--server":
				out[i] = flag + "=" + URL(val)
				continue
			case "-name", "--name":
				continue
			}
		}
		lower := strings.ToLower(a)
		if strings.HasPrefix(lower, "-") && !strings.Contains(lower, "=") && strings.TrimLeft(lower, "-") != "" && classifyKey(lower) != keyNone {
			if i+1 < len(out) {
				out[i+1] = "[cut]"
				i++
			}
			continue
		}
		switch lower {
		case "-link", "--link", "-join", "--join", "-server", "--server":
			if i+1 < len(out) {
				out[i+1] = URL(out[i+1])
				i++
			}
			continue
		case "-name", "--name":
			if i+1 < len(out) {
				i++
			}
			continue
		}
		if isArgURLOrJoinInput(a) {
			out[i] = URL(a)
		}
	}
	return out
}

func cutFlagEq(a string) (flag, val string, ok bool) {
	if !strings.HasPrefix(a, "-") {
		return "", "", false
	}
	return strings.Cut(a, "=")
}

func isArgURLOrJoinInput(s string) bool {
	trimmed := strings.TrimSpace(s)
	if strings.Contains(trimmed, "://") {
		return true
	}
	if hasBareUserinfo(trimmed) {
		return true
	}
	before := trimmed
	hasFrag := false
	if idx, _ := nextFragMarker(trimmed); idx >= 0 {
		before = trimmed[:idx]
		hasFrag = true
	}
	if isSchemelessShareURL(before) {
		return true
	}
	return hasFrag && isValidRoomCode(before)
}

func nextFragMarker(s string) (idx, markerLen int) {
	idx = -1
	sub := s
	if i := strings.IndexByte(s, '#'); i >= 0 {
		idx, markerLen = i, 1
		sub = s[:i]
	}
	if strings.IndexByte(sub, '%') >= 0 {
		if i := strings.Index(sub, "%2523"); i >= 0 {
			idx, markerLen = i, 5
			sub = sub[:i]
		}
		if i := strings.Index(sub, "%23"); i >= 0 {
			idx, markerLen = i, 3
		}
	}
	return idx, markerLen
}

func redactFragments(s string, hasPct23 bool) string {
	var b strings.Builder
	var cur lineCursor
	cur.init(s)
	var uf shareURLFinder
	uf.init(s)
	// escCut is where the latest matched fragment ended at a literal
	// whitespace escape (\n, \t, \u000a, ...), or -1. ufAfterEsc only considers
	// URLs starting after that escape.
	escCut := -1
	var ufAfterEsc shareURLFinder
	ufAfterEsc.init(s)
	var bt bareSegTracker
	bt.init(s)

	emitPos := 0
	scanPos := 0
	hashAt := -2
	pctAt, pctLen := -2, 0
	if !hasPct23 {
		pctAt = -1
	}

	for scanPos < len(s) {
		if hashAt != -1 && hashAt < scanPos {
			if rel := strings.IndexByte(s[scanPos:], '#'); rel >= 0 {
				hashAt = scanPos + rel
			} else {
				hashAt = -1
			}
		}
		if pctAt != -1 && pctAt < scanPos {
			pctAt = -1
			p := scanPos
			for p+3 <= len(s) {
				rel := strings.IndexByte(s[p:], '%')
				if rel < 0 {
					break
				}
				cand := p + rel
				if cand+3 <= len(s) && s[cand+1] == '2' {
					if s[cand+2] == '3' {
						pctAt, pctLen = cand, 3
						break
					}
					if s[cand+2] == '5' && cand+5 <= len(s) && s[cand+3] == '2' && s[cand+4] == '3' {
						pctAt, pctLen = cand, 5
						break
					}
				}
				p = cand + 1
			}
		}

		fragIdx, fragLen := -1, 0
		if hashAt >= 0 && (pctAt < 0 || hashAt < pctAt) {
			fragIdx, fragLen = hashAt, 1
		} else if pctAt >= 0 {
			fragIdx, fragLen = pctAt, pctLen
		} else {
			break
		}

		tokenStart, ok := matchFragmentPrefix(s, fragIdx, fragLen, &uf, &bt)
		if ok && tokenStart < escCut && nameTagBefore(s, ufAfterEsc.floor, fragIdx, fragLen) {
			// The only URL reaching this name#tag runs through an earlier,
			// already-cut fragment that ended at a literal escape (`…#pw\nmartin#0A1B`).
			// Judge it by what follows the escape: a URL there, else bare rules.
			if st, urlOK := ufAfterEsc.find(fragIdx); urlOK {
				tokenStart = st
			} else {
				tokenStart, ok = matchBareRoomCode(s, fragIdx, fragLen, &bt)
			}
		}
		if !ok {
			scanPos = fragIdx + fragLen
			continue
		}
		end := scanFragmentEnd(s, tokenStart, fragIdx, fragLen, &cur)
		if end < len(s) && s[end] == '\\' {
			if escEnd, isWs, _, _, _, _ := consumeEscape(s, end); isWs {
				escCut = end
				ufAfterEsc.floor = escEnd
			}
		}
		if fragLen == 1 && s[fragIdx:end] == "#[cut]" {
			scanPos = end
			continue
		}
		if emitPos == 0 {
			b.Grow(len(s))
		}
		b.WriteString(s[emitPos:fragIdx])
		b.WriteString("#[cut]")
		emitPos = end
		scanPos = end
	}

	if emitPos == 0 {
		return s
	}
	b.WriteString(s[emitPos:])
	return b.String()
}

// matchFragmentPrefix checks whether s[..fragIdx] ends with a share link / URL
// (on any path) or a bare room code accepted by ParseJoinInput, starting after
// ANY non-[A-Za-z0-9_.-] byte (including '"', '\\', ':', '(', '[', '{', ',',
// control bytes, non-ASCII UTF-8 bytes) or literal escape sequence (\n, \\n,
// \u000a, \x0a, etc.). uf carries the incremental share-URL state; fragIdx must
// increase strictly across calls that share uf.
func matchFragmentPrefix(s string, fragIdx, fragLen int, uf *shareURLFinder, bt *bareSegTracker) (int, bool) {
	if fragIdx == 0 {
		return 0, false
	}
	if urlStart, ok := uf.find(fragIdx); ok {
		return urlStart, true
	}
	return matchBareRoomCode(s, fragIdx, fragLen, bt)
}

// isURLWindowStop reports bytes that can never be part of a share URL: ASCII
// control/space, non-ASCII UTF-8 (>=0x7f), and quote/angle/open-group
// delimiters. A URL candidate never spans one of these. '#' and '%23' do NOT
// stop the window: `http://%23x#pw` and `?next=%23host/j/CODE#pw` must still
// find the URL that starts before them.
// nameTagBefore reports whether the ASCII letters right before fragIdx (not
// reaching back past from) form a name#tag such as martin#0A1B that ends its
// token (end, whitespace or a quote follows).
func nameTagBefore(s string, from, fragIdx, fragLen int) bool {
	ns := fragIdx
	for ns > from && isAlphaASCII(s[ns-1]) {
		ns--
	}
	if ns == fragIdx || !isNameWithHexTag(s[ns:fragIdx], s, fragIdx, fragLen) {
		return false
	}
	// Only a tag that ends the token: anything after it in the same token
	// would have been part of the cut fragment before.
	after := fragIdx + 5
	if after == len(s) || s[after] == '"' || s[after] == '\'' {
		return true
	}
	if _, ws := isHardWhitespaceAt(s, after); ws {
		return true
	}
	_, _, isQ, _, _, _ := consumeEscape(s, after)
	return isQ
}

func isURLWindowStop(c byte) bool {
	return c <= 0x20 || c >= 0x7f || c == '"' || c == '\'' || c == '`' || c == '<' || c == '>' || c == '(' || c == '{'
}

const (
	maxSchemeProbe   = 9   // "bshare://" is the longest share scheme prefix
	maxSchemelessURL = 320 // a schemeless host must be followed by '/' within this many bytes
)

type urlCand struct {
	start  int // boundary where the URL candidate starts
	thresh int // smallest fragIdx for which s[start:fragIdx] is a share URL
}

// shareURLFinder answers "what is the leftmost structural boundary i in the
// current URL window such that s[i:fragIdx] is a share URL?" for a strictly
// increasing sequence of fragIdx values, in O(n) total.
//
// Whether s[i:fragIdx] is a share URL depends on fragIdx only through a
// per-boundary threshold: a scheme URL needs fragIdx > i+len("scheme://"), and
// a schemeless one needs its first '/' (within 320 bytes) before fragIdx. So a
// boundary, once valid, stays valid for every later fragment in the same
// window, and every boundary that is not yet valid lies within 321 bytes of
// fragIdx. Each window is walked once; the only per-fragment work is a check
// of those few pending boundaries. This gives exactly the result of the
// former per-fragment backward rescan (which was quadratic) without bounding
// the window by an earlier '#'/'%23' or by the previous match.
type shareURLFinder struct {
	s         string
	floor     int  // windows never start before floor
	stopTo    int  // bytes before stopTo have been checked for window stops
	winStart  int  // start of the window containing the latest fragIdx
	walk      int  // next position for the boundary walk within the window
	forced    bool // the boundary walk is at a forced boundary (after an escape)
	best      int  // leftmost valid boundary in the window, or -1
	pendN     int  // number of pending candidates (sorted by start)
	pendBuf   [16]urlCand
	pendMore  []urlCand // pending candidates beyond len(pendBuf)
	nextSlash int       // first '/' at or after the last query position (len(s) if none)
	memoStart int       // memo for isHostLike(s[memoStart:memoEnd])
	memoEnd   int
	memoOK    bool
	si        slashInfo // per-'/' summary for hostOK
}

func (f *shareURLFinder) init(s string) {
	f.s = s
	f.stopTo = 0
	f.winStart = 0
	f.walk = 0
	f.forced = true
	f.best = -1
	f.pendN = 0
	f.nextSlash = -1
	f.memoStart, f.memoEnd = -1, -1
	f.si.p = -1
}

func (f *shareURLFinder) find(fragIdx int) (int, bool) {
	s := f.s
	win := f.winStart
	for k := f.stopTo; k < fragIdx; k++ {
		if isURLWindowStop(s[k]) {
			win = k + 1
		}
	}
	f.stopTo = fragIdx
	win = max(win, f.floor)
	if win != f.winStart || f.walk < win {
		f.winStart = win
		f.walk = win
		f.forced = true
		f.best = -1
		f.pendN = 0
	}

	// Walk new boundaries left-to-right. Once a valid boundary exists, later
	// (larger) boundaries can never be the leftmost, so skip the walk.
	if f.best < 0 {
		for f.walk < fragIdx {
			i := f.walk
			if s[i] == '\\' {
				escEnd, _, _, _, _, isB := consumeEscape(s, i)
				if escEnd > fragIdx {
					break
				}
				f.walk = escEnd
				f.forced = isB
				continue
			}
			atBoundary := f.forced || i == f.winStart || !isKeyIdentByte(s[i-1])
			f.forced = false
			f.walk++
			if atBoundary {
				if th := f.threshold(i); th >= 0 {
					f.pushPending(urlCand{start: i, thresh: th})
					if th <= fragIdx {
						// Valid now, so no later boundary can be leftmost.
						break
					}
				}
			}
		}
	}

	// Pending candidates are sorted by start and all lie left of best.
	for k := 0; k < f.pendN; k++ {
		if c := f.pendAt(k); c.thresh <= fragIdx {
			f.best = c.start
			f.pendN = k
			break
		}
	}
	if f.best >= 0 {
		return f.best, true
	}
	return 0, false
}

func (f *shareURLFinder) pushPending(c urlCand) {
	if f.pendN < len(f.pendBuf) {
		f.pendBuf[f.pendN] = c
	} else {
		f.pendMore = append(f.pendMore[:f.pendN-len(f.pendBuf)], c)
	}
	f.pendN++
}

func (f *shareURLFinder) pendAt(k int) urlCand {
	if k < len(f.pendBuf) {
		return f.pendBuf[k]
	}
	return f.pendMore[k-len(f.pendBuf)]
}

// threshold returns the smallest fragIdx for which s[i:fragIdx] is a share
// URL, or -1 if it never is:
//   - a share scheme ("ws", "wss", "http", "https", "blite", "bshare", any
//     case) followed by "://" within the first maxSchemeProbe bytes, plus at
//     least one more byte; or
//   - isSchemelessShareURL: the first '/' lies within maxSchemelessURL bytes
//     and the host before it (after any userinfo '@') is host-like.
func (f *shareURLFinder) threshold(i int) int {
	s := f.s
	th := -1
	if c := s[i] | 0x20; c == 'w' || c == 'h' || c == 'b' {
		probe := s[i:min(len(s), i+maxSchemeProbe)]
		if sep := strings.Index(probe, "://"); sep >= 2 && isShareSchemeName(probe[:sep]) {
			th = i + sep + 4
		}
	}
	if f.nextSlash < i {
		if rel := strings.IndexByte(s[i:], '/'); rel >= 0 {
			f.nextSlash = i + rel
		} else {
			f.nextSlash = len(s)
		}
	}
	p := f.nextSlash
	if p >= len(s) || p == i || p-i >= maxSchemelessURL {
		return th
	}
	if th >= 0 && th <= p+1 {
		return th
	}
	if f.hostOK(i, p) {
		return p + 1
	}
	return th
}

// slashInfo summarizes the bytes before a '/' at p that any schemeless URL
// candidate ending its host at p can contain. Every boundary i whose first
// '/' is p lies in (previous '/', p) and within maxSchemelessURL of p, so
// each byte is summarized for one p only. All fields are positions, or -1.
type slashInfo struct {
	p         int
	lastAt    int // last '@'
	badLoose  int // last byte outside [A-Za-z0-9.:[]+-]
	badStrict int // last byte outside [A-Za-z0-9.-]
	colon1    int // last ':'
	colon2    int // second-to-last ':'
	strictB1  int // last byte outside [A-Za-z0-9.-] before colon1
	open2     int // second-to-last '['
}

func (f *shareURLFinder) slash(p int) *slashInfo {
	si := &f.si
	if si.p == p {
		return si
	}
	s := f.s
	*si = slashInfo{p: p, lastAt: -1, badLoose: -1, badStrict: -1, colon1: -1, colon2: -1, strictB1: -1, open2: -1}
	open1 := -1
	lo := max(0, p-maxSchemelessURL)
	for k := p - 1; k >= lo; k-- {
		c := s[k]
		if c == '/' {
			break
		}
		strict := isAlphaASCII(c) || (c >= '0' && c <= '9') || c == '.' || c == '-'
		if !strict {
			if si.badStrict < 0 {
				si.badStrict = k
			}
			if si.colon1 >= 0 && si.strictB1 < 0 && k < si.colon1 {
				si.strictB1 = k
			}
		}
		switch c {
		case ':':
			if si.colon1 < 0 {
				si.colon1 = k
			} else if si.colon2 < 0 {
				si.colon2 = k
			}
		case '[':
			if open1 < 0 {
				open1 = k
			} else if si.open2 < 0 {
				si.open2 = k
			}
		case '@':
			if si.lastAt < 0 {
				si.lastAt = k
			}
		}
		if !strict && c != ':' && c != '[' && c != ']' && c != '+' && si.badLoose < 0 {
			si.badLoose = k
		}
	}
	return si
}

// hostOK reports isSchemelessShareURL's host test for the candidate starting
// at i whose first '/' is at p: the host is s[i:p] after its last '@'. Cheap
// structural rules (each a consequence of how isHostLike parses) reject most
// candidates without calling isHostLike, so at most a bounded number of
// boundaries per '/' pay for the full check.
func (f *shareURLFinder) hostOK(i, p int) bool {
	s := f.s
	si := f.slash(p)
	h0 := i
	if si.lastAt >= i {
		h0 = si.lastAt + 1
	}
	if h0 >= p || h0 <= si.badLoose {
		return false
	}
	if h0 <= si.badStrict {
		// The host contains ':', '[', ']' or '+'.
		switch {
		case h0 <= si.open2:
			return false // two '[': SplitHostPort fails, and so do the name rules
		case si.colon1 < h0:
			return false // no ':': '[', ']', '+' fit neither IPs nor labels
		case si.colon2 < h0:
			// One ':': a plain host:port needs a [A-Za-z0-9.-] host.
			if s[h0] != '[' && h0 <= si.strictB1 {
				return false
			}
		default:
			// Two or more ':' and no '[': only an IPv6 literal (<= 45 bytes).
			if s[h0] != '[' && p-h0 > 45 {
				return false
			}
		}
	}
	if h0 != f.memoStart || p != f.memoEnd {
		f.memoStart, f.memoEnd = h0, p
		f.memoOK = isHostLike(s[h0:p])
	}
	return f.memoOK
}

func advancePastAllEscapesAndNonASCII(s string, start, limit int) int {
	last := start
	for i := start; i < limit; {
		if s[i] == '\\' {
			escEnd, _, _, _, _, isB := consumeEscape(s, i)
			if escEnd <= i {
				i++
				continue
			}
			i = escEnd
			if isB && i <= limit {
				last = i
			}
			continue
		}
		if s[i] < 0x20 || s[i] >= 0x7f {
			i++
			last = i
			continue
		}
		i++
	}
	return last
}

// maxBareLookback bounds the backward scan in matchBareRoomCode to a small
// constant window (~maxCodeLen + optional key/padding/escape prefix) so lines
// with thousands of '#' characters run in O(n) time. The window is measured
// from the last non-space byte before the marker, so `CODE      #pw` with any
// amount of padding before '#' still matches.
const maxBareLookback = 48

// bareSegTracker records, in one forward pass, the last bare-segment stop and
// the last backslash before each (strictly increasing) fragIdx, so
// matchBareRoomCode needs no per-fragment backward rescans of the window.
type bareSegTracker struct {
	s        string
	scanned  int // bytes before scanned have been classified
	lastStop int // index just after the last isBareSegmentStop byte (0: none)
	lastBS   int // index of the last '\\' (-1: none)
	lastSep  int // index of the last '=' or ':' (-1: none)
	lastNS   int // index of the last non-space byte (-1: none)
	spaceBNS int // index of the last ' ' before lastNS (-1: none)
	lastSp   int // index of the last ' ' (-1: none)
}

func (t *bareSegTracker) init(s string) {
	*t = bareSegTracker{s: s, lastBS: -1, lastSep: -1, lastNS: -1, spaceBNS: -1, lastSp: -1}
}

func (t *bareSegTracker) advance(to int) {
	s := t.s
	for k := t.scanned; k < to; k++ {
		c := s[k]
		if c == ' ' {
			t.lastSp = k
			continue
		}
		t.lastNS = k
		t.spaceBNS = t.lastSp
		switch {
		case c == '\\':
			t.lastBS = k
		case c == '=' || c == ':':
			t.lastSep = k
		case isBareSegmentStop(c):
			t.lastStop = k + 1
		}
	}
	if to > t.scanned {
		t.scanned = to
	}
}

func matchBareRoomCode(s string, fragIdx, fragLen int, bt *bareSegTracker) (int, bool) {
	bt.advance(fragIdx)
	// anchor: just after the last non-space byte, not before the segment stop.
	anchor := max(bt.lastNS+1, bt.lastStop)
	// Every match (key=CODE, "CODE, prose CODE) needs a code byte as the last
	// non-space byte before the marker.
	if anchor == bt.lastStop || !isCodeTokenByte(s[anchor-1]) {
		return 0, false
	}
	minScan := max(0, anchor-maxBareLookback)
	if bt.lastStop <= minScan {
		for minScan > 0 && s[minScan-1] == '\\' {
			minScan--
		}
	}
	// Same as scanning back from fragIdx to the first stop byte, bounded by
	// minScan: no stop byte lies in [bt.lastStop, fragIdx).
	rawSeg := max(bt.lastStop, minScan)
	hitStop := rawSeg == 0 || isBareSegmentStop(s[rawSeg-1])
	segStart := rawSeg
	if bt.lastBS >= rawSeg {
		segStart = advancePastAllEscapesAndNonASCII(s, rawSeg, fragIdx)
	}
	seg := s[segStart:fragIdx]
	if seg == "" {
		return 0, false
	}
	// Cases A and C need at least 4 code bytes right before the anchor; only
	// case B (an opening quote, where "k7 pq" style inner spaces count) can
	// match with fewer.
	if run := codeRunBefore(s, anchor, segStart, 4); run < 4 &&
		!(hitStop && rawSeg == segStart && segStart >= 1 && (s[segStart-1] == '"' || s[segStart-1] == '\'')) {
		return 0, false
	}

	// Case A: unquoted key=value or key:value in the immediate token before
	// fragIdx (e.g. peer=alice#7F2A, name=martin#0A1B, link:K7PQ2M4X#pw,
	// code:K7PQ2M4X#pw, ch=chan#3, join=7K3QX9MD#pw).
	// The token is the last space-delimited run before the marker (tabs are
	// segment stops). The tracker already knows its bounds and its last
	// '='/':' so nothing is rescanned here.
	tokEnd := max(anchor, segStart)
	tokStart := segStart
	if tokEnd == anchor && bt.spaceBNS+1 > tokStart {
		tokStart = bt.spaceBNS + 1
	}
	sepAt := -1 // last '=' or ':' in the token
	if bt.lastSep >= tokStart && bt.lastSep < tokEnd {
		sepAt = bt.lastSep
	}
	lastTok := s[tokStart:tokEnd]
	if sepAt >= 0 {
		sep := sepAt - tokStart
		keyPart := lastTok[:sep]
		codeStart := tokStart + sep + 1
		codeCand := s[codeStart:tokEnd]
		key := trailingAttrKey(keyPart)
		if isExplicitCodeAttrKey(key) {
			if isValidRoomCode(codeCand) {
				return codeStart, true
			}
			return 0, false
		}
		if isExplicitNameAttrKey(key) {
			if isNameWithHexTag(codeCand, s, fragIdx, fragLen) {
				return 0, false
			}
		}
		if isBareCodeCandidate(codeCand, s, fragIdx, fragLen) {
			return codeStart, true
		}
		return 0, false
	}

	// Case B: immediately preceded by an opening quote ("..." or '...').
	if hitStop && segStart >= 1 && (s[segStart-1] == '"' || s[segStart-1] == '\'') && rawSeg == segStart {
		keyWinStart := max(0, segStart-1-64)
		prefixKey := trailingQuotedAttrKey(s[keyWinStart : segStart-1])
		if isValidRoomCode(seg) {
			if isExplicitCodeAttrKey(prefixKey) {
				return segStart, true
			}
			if (isExplicitNameAttrKey(prefixKey) || strings.EqualFold(prefixKey, "msg")) &&
				isNameWithHexTag(seg, s, fragIdx, fragLen) {
				return 0, false
			}
			if !strings.EqualFold(prefixKey, "msg") && !isShortLowerLabelWithNum(seg, s, fragIdx, fragLen) {
				return segStart, true
			}
		}
	}

	// Case C: prose / suffix of seg (handles spaces/dashes before '#', non-ASCII
	// / escape boundaries, and 8-char / 4-4 / uppercase hosted codes).
	return matchProseRoomCodeSuffix(s, rawSeg, segStart, fragIdx, fragLen)
}

// codeRunBefore counts isCodeTokenByte bytes ending at end, not below from
// and at most limit.
func codeRunBefore(s string, end, from, limit int) int {
	n := 0
	for end-n > from && n < limit && isCodeTokenByte(s[end-n-1]) {
		n++
	}
	return n
}

func isCodeBoundaryBefore(s string, idx, rawSeg, segStart int) bool {
	if idx == 0 {
		return true
	}
	if idx == segStart && segStart > rawSeg {
		return true
	}
	return !isKeyIdentByte(s[idx-1])
}

func matchProseRoomCodeSuffix(s string, rawSeg, segStart, fragIdx, fragLen int) (int, bool) {
	end := fragIdx
	for end > segStart && s[end-1] == ' ' {
		end--
	}
	hadTrailingSpace := end < fragIdx
	if end == segStart {
		return 0, false
	}

	cStart := end
	for cStart > segStart && isCodeTokenByte(s[cStart-1]) {
		cStart--
	}
	if cStart == end {
		return 0, false
	}
	tok := s[cStart:end]

	// Check two 4-char Crockford groups separated by spaces: "k7pq 2m4x#pw",
	// "k7pq  2m4x  #pw14", "paste k7pq 2m4x #pwSp5".
	if len(tok) == 4 && isFourCrockford(tok) {
		sp := cStart
		for sp > segStart && s[sp-1] == ' ' {
			sp--
		}
		if sp < cStart {
			g1Start := sp
			for g1Start > segStart && isCodeTokenByte(s[g1Start-1]) {
				g1Start--
			}
			if sp-g1Start == 4 && isCodeBoundaryBefore(s, g1Start, rawSeg, segStart) {
				g1 := s[g1Start:sp]
				if isFourCrockford(g1) && (hasDigit(g1) || hasDigit(tok) || (isAllUpperASCII(g1) && isAllUpperASCII(tok))) {
					return g1Start, true
				}
			}
		}
	}

	// Structural start boundary: start of segment or any non-[A-Za-z0-9_.-] byte.
	if !isCodeBoundaryBefore(s, cStart, rawSeg, segStart) {
		return 0, false
	}

	if hadTrailingSpace {
		if isFourDashFour(tok) {
			return cStart, true
		}
		if normalizedCodeLen(tok) == 8 && (hasDigit(tok) || isAllUpperASCII(tok)) {
			return cStart, true
		}
		return 0, false
	}

	if isBareCodeCandidate(tok, s, fragIdx, fragLen) {
		return cStart, true
	}
	return 0, false
}

func isBareCodeCandidate(tok, s string, fragIdx, fragLen int) bool {
	if !isValidRoomCode(tok) {
		return false
	}
	if fragLen > 1 {
		return true
	}
	if isNameWithHexTag(tok, s, fragIdx, fragLen) {
		return false
	}
	if isFourDashFour(tok) || normalizedCodeLen(tok) == 8 {
		return true
	}
	return isAllUpperASCII(tok)
}

func isFourDashFour(tok string) bool {
	return len(tok) == 9 && tok[4] == '-' && isFourCrockford(tok[:4]) && isFourCrockford(tok[5:])
}

func hasDigit(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			return true
		}
	}
	return false
}

func isAllUpperASCII(s string) bool {
	hasAlpha := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			return false
		}
		if c >= 'A' && c <= 'Z' {
			hasAlpha = true
		}
	}
	return hasAlpha || len(s) > 0
}

func isNameWithHexTag(name, s string, fragIdx, fragLen int) bool {
	if fragLen != 1 || name == "" {
		return false
	}
	hasLower := false
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'a' && c <= 'z' {
			hasLower = true
			continue
		}
		if c >= 'A' && c <= 'Z' {
			continue
		}
		return false
	}
	if !hasLower || fragIdx+5 > len(s) {
		return false
	}
	for i := 0; i < 4; i++ {
		c := s[fragIdx+1+i]
		if !isHexByte(c) {
			return false
		}
	}
	if fragIdx+5 == len(s) {
		return true
	}
	if _, ws := isHardWhitespaceAt(s, fragIdx+5); ws {
		return true
	}
	if s[fragIdx+5] == '\\' {
		_, _, isQ, _, _, _ := consumeEscape(s, fragIdx+5)
		if isQ {
			return true
		}
	}
	switch s[fragIdx+5] {
	case '"', '\'', ')', ']', '}', ',', ';', ':':
		return true
	}
	return false
}

func isShortLowerLabelWithNum(seg, s string, fragIdx, fragLen int) bool {
	if fragLen != 1 || normalizedCodeLen(seg) == 8 || strings.ContainsAny(seg, "- ") {
		return false
	}
	for i := 0; i < len(seg); i++ {
		if !(seg[i] >= 'a' && seg[i] <= 'z') {
			return false
		}
	}
	rest := s[fragIdx+1:]
	n := 0
	for n < len(rest) && n <= 4 && rest[n] >= '0' && rest[n] <= '9' {
		n++
	}
	return n >= 1 && n <= 3
}

func trailingAttrKey(s string) string {
	end := len(s)
	start := end
	for start > 0 && isKeyIdentByte(s[start-1]) {
		start--
	}
	return s[start:end]
}

func trailingQuotedAttrKey(prefix string) string {
	p := strings.TrimRight(prefix, " \t")
	if strings.HasSuffix(p, "=") {
		return trailingAttrKey(strings.TrimRight(p[:len(p)-1], " \t"))
	}
	if strings.HasSuffix(p, ":") {
		p = strings.TrimRight(p[:len(p)-1], " \t")
		p = strings.TrimSuffix(p, `"`)
		p = strings.TrimRight(p, `\`)
		return trailingAttrKey(p)
	}
	return ""
}

func isExplicitCodeAttrKey(k string) bool {
	k = strings.TrimLeft(k, "-")
	return strings.EqualFold(k, "join") || strings.EqualFold(k, "link") ||
		strings.EqualFold(k, "code") || strings.EqualFold(k, "room")
}

func isExplicitNameAttrKey(k string) bool {
	k = strings.TrimLeft(k, "-")
	return strings.EqualFold(k, "peer") || strings.EqualFold(k, "name") ||
		strings.EqualFold(k, "member") || strings.EqualFold(k, "from") ||
		strings.EqualFold(k, "by") || strings.EqualFold(k, "owner") ||
		strings.EqualFold(k, "user")
}

func isBareSegmentStop(c byte) bool {
	if c < 0x20 || c >= 0x7f {
		return true
	}
	switch c {
	case '#', '"', '\'', '`', '<', '>', '(', '[', '{', ',', ';':
		return true
	}
	return false
}

func isCodeTokenByte(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '-'
}

func isFourCrockford(s string) bool {
	return len(s) == 4 && normalizedCodeLen(s) == 4
}

func normalizedCodeLen(in string) int {
	n := 0
	for i := 0; i < len(in); i++ {
		c := in[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		switch {
		case c == '-' || c == ' ':
			continue
		case c == 'O':
			c = '0'
		case c == 'I' || c == 'L':
			c = '1'
		}
		// c is in crockford: a digit or an upper-case letter other than I, L,
		// O (mapped above) and U.
		if !((c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z' && c != 'U')) {
			return -1
		}
		n++
		if n > maxCodeLen {
			return -1
		}
	}
	return n
}

func isValidRoomCode(in string) bool {
	n := normalizedCodeLen(in)
	return n >= minCodeLen && n <= maxCodeLen
}

// isShareSchemeName reports whether name (case-insensitively) is ws, wss,
// http, https, blite or bshare.
func isShareSchemeName(name string) bool {
	switch len(name) {
	case 2:
		return hasFoldAt(name, 0, "ws")
	case 3:
		return hasFoldAt(name, 0, "wss")
	case 4:
		return hasFoldAt(name, 0, "http")
	case 5:
		return hasFoldAt(name, 0, "https") || hasFoldAt(name, 0, "blite")
	case 6:
		return hasFoldAt(name, 0, "bshare")
	}
	return false
}

func isSchemelessShareURL(cand string) bool {
	lim := min(len(cand), 320)
	slash := strings.IndexByte(cand[:lim], '/')
	if slash <= 0 {
		return false
	}
	hostPort := cand[:slash]
	if at := strings.LastIndexByte(hostPort, '@'); at >= 0 {
		hostPort = hostPort[at+1:]
	}
	return hostPort != "" && isHostLike(hostPort)
}

// mayBeIP is a cheap, allocation-free superset test for net.ParseIP: IPv4
// and IPv6 literals only use hex digits, '.' and ':'.
func mayBeIP(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !isHexByte(c) && c != '.' && c != ':' {
			return false
		}
	}
	return true
}

func isHostLike(h string) bool {
	name, port := h, ""
	if strings.IndexByte(h, ':') < 0 {
		// net.SplitHostPort always fails (and allocates its error) here.
		if strings.HasPrefix(h, "[") {
			return false
		}
	} else if hh, p, err := net.SplitHostPort(h); err == nil {
		name, port = hh, p
	} else if strings.Count(h, ":") == 1 || strings.HasPrefix(h, "[") {
		return false
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	if mayBeIP(name) && net.ParseIP(name) != nil {
		return true
	}
	if strings.EqualFold(name, "localhost") {
		return true
	}
	dot := strings.LastIndexByte(name, '.')
	if dot <= 0 || dot == len(name)-1 || len(name) > 253 {
		return false
	}
	tld := name[dot+1:]
	if len(tld) < 2 {
		return false
	}
	for i := 0; i < len(tld); i++ {
		c := tld[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			return false
		}
	}
	start := 0
	for start <= len(name) {
		end := strings.IndexByte(name[start:], '.')
		if end < 0 {
			end = len(name)
		} else {
			end += start
		}
		lbl := name[start:end]
		if lbl == "" || len(lbl) > 63 || lbl[0] == '-' || lbl[len(lbl)-1] == '-' {
			return false
		}
		for i := 0; i < len(lbl); i++ {
			c := lbl[i]
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-') {
				return false
			}
		}
		start = end + 1
	}
	return true
}

// scanFragmentEnd finds where the fragment starting at fragIdx (with marker
// length fragLen) ends:
//   - inside a quoted value, at the closing quote of matching backslash depth;
//   - otherwise, at whitespace / control / non-ASCII separator / whitespace
//     escape (or closing quote of an outer slog/JSON string, or matching ')' /
//     '}' when the URL/struct itself opened with '(' or '{').
func scanFragmentEnd(s string, tokenStart, fragIdx, fragLen int, cur *lineCursor) int {
	open := tokenStart
	for _, pfx := range []string{"--link=", "-link=", "--join=", "-join="} {
		if open > len(pfx) && strings.EqualFold(s[open-len(pfx):open], pfx) && s[open-len(pfx)-1] == '"' {
			qIdx := open - len(pfx) - 1
			prev := qIdx - countBackslashesBefore(s, qIdx)
			if prev == 0 || (s[prev-1] != '=' && s[prev-1] != ':') {
				open -= len(pfx)
			}
			break
		}
	}
	i := fragIdx + fragLen

	// Double-quoted value (unescaped "..." or escaped \"...\" at any depth).
	if open >= 1 && s[open-1] == '"' {
		openBS := countBackslashesBefore(s, open-1)
		contentEnd, _, _ := scanQuotedEnd(s, i, '"', openBS)
		return contentEnd
	}

	// Single-quoted value when preceded by boundary.
	if open >= 1 && s[open-1] == '\'' && (open == 1 || !isKeyIdentByte(s[open-2])) {
		contentEnd, _, _ := scanQuotedEnd(s, i, '\'', 0)
		return contentEnd
	}

	cur.advanceTo(open)
	parenWrapped := open >= 1 && s[open-1] == '('
	braceWrapped := cur.inUnclosedBrace()
	outerQuote := cur.inOuterDoubleQuote()
	escQuoteBS := cur.activeEscapedQuoteBS()
	parenDepth, braceDepth := 0, 0

	if fragLen == 1 && strings.HasPrefix(s[i:], "[cut]") {
		next := i + len("[cut]")
		if next == len(s) {
			return next
		}
		if _, ws := isHardWhitespaceAt(s, next); ws {
			return next
		}
		if isTokenEndByte(s[next], outerQuote, parenWrapped, braceWrapped) {
			return next
		}
		if s[next] == ',' || s[next] == ';' || (s[next] == ':' && next+1 < len(s) && (s[next+1] == ' ' || s[next+1] == '\t')) {
			return next
		}
	}

	for i < len(s) {
		if _, ws := isHardWhitespaceAt(s, i); ws {
			break
		}
		c := s[i]
		if c == '\\' {
			escEnd, isWs, isQ, qBS, qCh, _ := consumeEscape(s, i)
			if isWs {
				break
			}
			if isQ && qCh == '"' && escQuoteBS > 0 {
				if usedBS, ok := closesQuoteAtDepth(qBS, escQuoteBS); ok {
					i += qBS - usedBS
					break
				}
			}
			if escEnd > i {
				i = escEnd
				continue
			}
		}
		if parenWrapped && c == '(' {
			parenDepth++
			i++
			continue
		}
		if parenWrapped && c == ')' && parenDepth > 0 {
			parenDepth--
			i++
			continue
		}
		if braceWrapped && c == '{' {
			braceDepth++
			i++
			continue
		}
		if braceWrapped && c == '}' && braceDepth > 0 {
			braceDepth--
			i++
			continue
		}
		if isTokenEndByte(c, outerQuote, parenWrapped && parenDepth == 0, braceWrapped && braceDepth == 0) {
			break
		}
		i++
	}
	return i
}

func countBackslashesBefore(s string, idx int) int {
	n := 0
	for idx-1-n >= 0 && s[idx-1-n] == '\\' {
		n++
	}
	return n
}

func isTokenEndByte(c byte, outerQuote, stopOnCloseParen, stopOnCloseBrace bool) bool {
	switch c {
	case ' ', '\t', '\r', '\n':
		return true
	case '"':
		return outerQuote
	case ')':
		return stopOnCloseParen
	case '}':
		return stopOnCloseBrace
	}
	return false
}
