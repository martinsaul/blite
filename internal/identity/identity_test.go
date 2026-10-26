package identity

import (
	"strings"
	"testing"
)

const sdp = "v=0\r\na=group:BUNDLE 0 1\r\na=fingerprint:sha-256 AB:CD:EF\r\nm=video 9\r\n"

func TestSignVerify(t *testing.T) {
	a, b := Ephemeral(), Ephemeral()
	sig := a.SignSDP("ROOM", "pa", "pb", sdp)
	if err := VerifySDP(a.Public(), "ROOM", "pa", "pb", sdp, sig); err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(sdp, "AB:CD:EF", "11:22:33", 1)
	cases := map[string]error{
		"tampered fingerprint": VerifySDP(a.Public(), "ROOM", "pa", "pb", tampered, sig),
		"wrong signer":         VerifySDP(b.Public(), "ROOM", "pa", "pb", sdp, sig),
		"replayed to other":    VerifySDP(a.Public(), "ROOM", "pa", "pc", sdp, sig),
		"other room":           VerifySDP(a.Public(), "ROOM2", "pa", "pb", sdp, sig),
	}
	for name, err := range cases {
		if err != ErrBadSignature {
			t.Errorf("%s: got %v", name, err)
		}
	}
	if err := VerifySDP("not-a-key", "ROOM", "pa", "pb", sdp, sig); err == nil {
		t.Error("accepted garbage idKey")
	}
}

func TestSafetyCode(t *testing.T) {
	a, b, c := Ephemeral().Public(), Ephemeral().Public(), Ephemeral().Public()
	x := SafetyCode([]string{a, b, c})
	if y := SafetyCode([]string{c, a, b}); x != y {
		t.Fatalf("order matters: %s vs %s", x, y)
	}
	if z := SafetyCode([]string{a, b, Ephemeral().Public()}); z == x {
		t.Fatal("different key, same code")
	}
	if len(strings.Fields(x)) != 5 {
		t.Fatalf("format: %q", x)
	}
}

func TestKnownTOFU(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	k := LoadKnown()
	if k.Check("alice", "K1") {
		t.Fatal("first sight flagged")
	}
	if k.Check("alice", "K1") {
		t.Fatal("same key flagged")
	}
	if !k.Check("alice", "K2") {
		t.Fatal("changed key not flagged")
	}
	if LoadKnown().Check("alice", "K2") != true {
		t.Fatal("store not persisted")
	}
}

func TestTagAndNames(t *testing.T) {
	a, b := Ephemeral().Public(), Ephemeral().Public()
	if ta := Tag(a); len(ta) != 4 || ta != Tag(a) || strings.Trim(ta, "0123456789") != "" {
		t.Fatalf("tag %q", ta)
	}
	if Tag("") != "" || Display("carol", "") != "carol" {
		t.Fatal("no key: no tag")
	}
	if d := Display("carol", a); d != "carol#"+Tag(a) {
		t.Fatalf("display %q", d)
	}
	_ = b
	for in, want := range map[string]string{"  Carol  ": "Carol", "Bob#1234": "Bob1234", "###": "", strings.Repeat("é", 40): strings.Repeat("é", 32)} {
		if got := CleanName(in); got != want {
			t.Errorf("CleanName(%q) = %q, want %q", in, got, want)
		}
	}
}
