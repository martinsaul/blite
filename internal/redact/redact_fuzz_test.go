package redact

import (
	"bytes"
	"strings"
	"testing"
)

// fuzzMarker is injected as a secret value; Text must never emit it.
const fuzzMarker = "QzXk7Mrk"

// fuzzSecretLines are lines whose MARK part is always a secret. FuzzText puts
// each on its own line between copies of the fuzz input, so the input cannot
// legitimately change how the line is read (every pass treats CR/LF as a
// token, quote and segment boundary).
var fuzzSecretLines = []string{
	"password=MARK",
	"time=x level=INFO msg=\"joined\" password=MARK peer=alice#7F2A",
	`{"password":"MARK","peer":"x"}`,
	`{"pw":MARK}`,
	"token=MARK",
	"-password MARK",
	`"-password" "MARK"`,
	"Bearer MARK",
	"https://share.bellion.io/j/K7PQ2M4X#MARK",
	"open bshare://join/K7PQ2M4X?server=share.example.com#MARK now",
	`link="https://share.bellion.io/j/K7PQ2M4X#MARK"`,
	"share.bellion.io/j/K7PQ2M4X%23MARK",
	"K7PQ-2M4X#MARK",
	"joining k7pq 2m4x #MARK",
	"wss://user:MARK@h.example.com/ws",
	"https://h.example.com/x?token=MARK&a=b",
}

// FuzzText checks that no input makes a redaction pass panic (the passes are
// called without the recover() net) and that known secrets injected next to
// arbitrary input never survive. Seeds live in testdata/fuzz/FuzzText.
func FuzzText(f *testing.F) {
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		_ = redactText(s, scanPrefilter(s))
		_ = redactURL(s)
		_ = Text(s)

		if strings.Contains(s, fuzzMarker) || len(s) > 4096 {
			return
		}
		for _, tmpl := range fuzzSecretLines {
			line := strings.ReplaceAll(tmpl, "MARK", fuzzMarker)
			in := s + "\n" + line + "\n" + s
			if got := redactText(in, scanPrefilter(in)); strings.Contains(got, fuzzMarker) {
				t.Fatalf("secret survived\ninput: %q\noutput: %q", in, got)
			}
		}

		// Writer: same lines, split at an input-dependent point.
		var out bytes.Buffer
		w := NewWriter(&out)
		all := s + "\n" + strings.ReplaceAll(strings.Join(fuzzSecretLines, "\n"), "MARK", fuzzMarker) + "\n" + s
		cut := len(s) % (len(all) + 1)
		if _, err := w.Write([]byte(all[:cut])); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(all[cut:])); err != nil {
			t.Fatal(err)
		}
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), fuzzMarker) {
			t.Fatalf("Writer let a secret through\ninput: %q\noutput: %q", all, out.String())
		}
		if strings.Contains(out.String(), redactionFailed) {
			t.Fatalf("Writer hit the panic safety net\ninput: %q", all)
		}
	})
}
