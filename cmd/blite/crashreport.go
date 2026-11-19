package main

import (
	"fmt"
	"regexp"
	"strings"

	"blite/internal/redact"
)

// A crash report is built from sections, not the log's raw tail: with a few
// hundred goroutines the traceback alone fills reportTail, and the panic
// header and what the app did before it were cut off.
const (
	crashRead     = 32 << 20 // bytes of a crashed log read to find the traceback
	crashAppLines = 64 << 10 // app log before the traceback
	crashGuardMax = 16 << 10 // guard lines after it
	// crashHeaderMax caps the header (a panic's message can be huge) so the
	// crashing goroutine and main still fit.
	crashHeaderMax = 16 << 10
)

// goroutineHead matches a goroutine's first line: "goroutine 7 [running]:",
// also with GOTRACEBACK=system's "gp=... m=..." fields.
var goroutineHead = regexp.MustCompile(`^goroutine (\d+) .*\[([^\]]*)\]:$`)

// crashHead matches the first line of a crash's output: a Go panic or fatal
// error, a Windows exception (winthrow), a Unix signal ("SIGSEGV: ...").
var crashHead = regexp.MustCompile(`^(panic: |fatal error: |Exception 0x|SIG[A-Z0-9]+: )`)

// crashHeadMore matches the lines that go with a crash header: before it
// ("unexpected fault address" precedes "fatal error: fault") or after it.
var crashHeadMore = regexp.MustCompile(`^(panic: |fatal error: |Exception 0x|SIG[A-Z0-9]+: |\[signal |PC=|signal arrived during|unexpected fault address|\tpanic: )`)

// crashStart is the line where the log's crash output starts: the last
// crash header (an earlier recovered panic, such as net/http's, may have
// printed a goroutine before), else the last running goroutine; -1 if none.
func crashStart(lines []string) int {
	start := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if crashHead.MatchString(strings.TrimRight(lines[i], "\r\n")) {
			start = i
			break
		}
	}
	if start < 0 {
		for i := len(lines) - 1; i >= 0; i-- {
			m := goroutineHead.FindStringSubmatch(strings.TrimRight(lines[i], "\r\n"))
			if m != nil && strings.HasPrefix(m[2], "running") {
				return i
			}
		}
		return -1
	}
	for start > 0 && crashHeadMore.MatchString(strings.TrimRight(lines[start-1], "\r\n")) {
		start--
	}
	return start
}

// crashLog is the report text for a crashed run's log t (its tail, up to
// crashRead), at most limit bytes: the app's last lines before the crash,
// the crash header with the crashing goroutine and main, the other
// goroutines deduplicated by stack, and the guard's lines. Without a crash
// in t it is the log's last lines, as for any other log. Everything taken
// from t goes through redact.Text, in whole lines.
func crashLog(t string, limit int) string {
	lines := strings.SplitAfter(t, "\n")
	start := crashStart(lines)
	if start < 0 {
		return redactTail(t, limit)
	}

	// The crash output: guard lines out, the rest in blank-line paragraphs.
	var guard []string
	var paras [][]string
	var cur []string
	for _, l := range lines[start:] {
		s := strings.TrimRight(l, "\r\n")
		switch {
		case strings.Contains(s, "guard:"):
			guard = append(guard, l)
		case s == "":
			if len(cur) > 0 {
				paras = append(paras, cur)
				cur = nil
			}
		default:
			cur = append(cur, l)
		}
	}
	if len(cur) > 0 {
		paras = append(paras, cur)
	}
	// The first paragraph is the header. The crashing goroutine is the next
	// one: with a "goroutine N [...]:" line after a panic, without one
	// (frames only) after a Windows exception. Then the other goroutines,
	// then anything else (a register dump).
	var header, trailer []string
	var blocks [][]string
	crash, mainG := -1, -1
	for i, p := range paras {
		first := strings.TrimRight(p[0], "\r\n")
		m := goroutineHead.FindStringSubmatch(first)
		switch {
		case i == 0 && m == nil:
			header = append(p, "\n")
		case m != nil:
			if crash < 0 {
				crash = len(blocks)
			} else if mainG < 0 && m[1] == "1" {
				mainG = len(blocks)
			}
			blocks = append(blocks, p)
		case crash < 0 && len(p) > 1 && strings.HasPrefix(p[1], "\t"):
			crash = len(blocks) // frames without a goroutine line
			blocks = append(blocks, p)
		default:
			trailer = append(append(trailer, p...), "\n")
		}
	}

	key := capHead(redact.Text(strings.Join(header, "")), crashHeaderMax)
	for _, i := range []int{crash, mainG} {
		if i >= 0 {
			key += redact.Text(strings.Join(blocks[i], "")) + "\n"
		}
	}
	key += redact.Text(strings.Join(trailer, ""))
	key = capHead(key, limit/2)

	from, n := start, 0
	for from > 0 && n < crashAppLines {
		from--
		n += len(lines[from])
	}
	app := redactTail(strings.Join(lines[from:start], ""), crashAppLines)

	g := redactTail(strings.Join(guard, ""), crashGuardMax)
	// The other goroutines, one per distinct stack, in order of first
	// appearance.
	type group struct {
		text string
		n    int
	}
	var groups []*group
	byStack := map[string]*group{}
	others := 0
	for i, b := range blocks {
		if i == crash || i == mainG {
			continue
		}
		others++
		k := stackKey(b)
		if gr := byStack[k]; gr != nil {
			gr.n++
			continue
		}
		gr := &group{text: strings.Join(b, ""), n: 1}
		byStack[k] = gr
		groups = append(groups, gr)
	}

	heads := [4]string{
		fmt.Sprintf("===== app log before the crash (last %d bytes)\n", len(app)),
		"\n===== traceback: header, crashing goroutine, main\n",
		fmt.Sprintf("\n===== other goroutines (%d, %d distinct stacks)\n", others, len(groups)),
		"\n===== guard\n",
	}
	const dropNote = 160 // room kept for the "dropped" line
	budget := limit - len(app) - len(key) - len(g) - dropNote
	for _, h := range heads {
		budget -= len(h)
	}
	var rest strings.Builder
	kept, keptG := 0, 0
	for _, gr := range groups {
		s := redact.Text(gr.text)
		if gr.n > 1 {
			s = fmt.Sprintf("(%d goroutines with this stack)\n", gr.n) + s
		}
		s += "\n"
		if len(s) > budget {
			break
		}
		budget -= len(s)
		rest.WriteString(s)
		kept += gr.n
		keptG++
	}
	if keptG < len(groups) {
		fmt.Fprintf(&rest, "(%d more goroutines with %d distinct stacks dropped to fit the report)\n", others-kept, len(groups)-keptG)
	}
	return heads[0] + app + heads[1] + key + heads[2] + rest.String() + heads[3] + g
}

// stackKey is a goroutine's stack as its frames' function names, so the
// same stack with other arguments, addresses or wait times groups together.
func stackKey(b []string) string {
	var k strings.Builder
	for _, l := range b[1:] {
		s := strings.TrimRight(l, "\r\n")
		if s == "" || strings.HasPrefix(s, "\t") {
			continue // file:line +0x..
		}
		if strings.HasPrefix(s, "created by ") {
			s, _, _ = strings.Cut(s, " in goroutine ")
		} else if strings.HasSuffix(s, ")") {
			if i := strings.LastIndex(s, "("); i > 0 {
				s = s[:i]
			}
		}
		k.WriteString(s)
		k.WriteByte('\n')
	}
	return k.String()
}

// lastBytes is the last n bytes of s, from a line start (the cut line is
// dropped; a single line longer than n is cut).
func lastBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// redactTail is the redacted end of s, at most n bytes. It redacts whole
// lines, from the start of the line the cut falls in, and cuts after
// redaction: a cut first could start inside a share link ("9MD#pw") that
// redaction no longer recognises, and redaction can make text longer.
func redactTail(s string, n int) string {
	if i := len(s) - n; i > 0 {
		s = s[strings.LastIndexByte(s[:i], '\n')+1:]
	}
	return lastBytes(redact.Text(s), n)
}

// capHead keeps the start of s up to n bytes, noting the cut.
func capHead(s string, n int) string {
	if len(s) <= n {
		return s
	}
	note := fmt.Sprintf("\n(%d bytes cut)\n", len(s)-n)
	s = s[:max(0, n-len(note))]
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[:i+1]
	}
	return s + note
}
