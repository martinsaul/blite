package secret

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	cases := [][]byte{
		{},
		[]byte("room-password-123"),
		{0x00, 0xff, 0x80, 0x7f, 0x01, 0xfe, 0x42, 0x99},
	}
	for _, want := range cases {
		s, err := Seal(want)
		if err != nil {
			t.Fatalf("Seal(%x): %v", want, err)
		}
		if !Sealed(s) || !strings.HasPrefix(s, "dpapi:") {
			t.Fatalf("Seal(%x) = %q, missing dpapi: prefix", want, s)
		}
		got, err := Open(s)
		if err != nil {
			t.Fatalf("Open(%q): %v", s, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("Open(Seal(%x)) = %x", want, got)
		}
	}
}

func TestOpenPlaintext(t *testing.T) {
	oldUnprotect := Unprotect
	Unprotect = func([]byte) ([]byte, error) {
		return nil, errors.New("unprotect must not be called for plaintext")
	}
	t.Cleanup(func() { Unprotect = oldUnprotect })

	for _, plain := range []string{"", "legacy-password", "base64seed=="} {
		if Sealed(plain) {
			t.Fatalf("Sealed(%q) = true", plain)
		}
		got, err := Open(plain)
		if err != nil {
			t.Fatalf("Open(%q): %v", plain, err)
		}
		if string(got) != plain {
			t.Fatalf("Open(%q) = %q", plain, got)
		}
	}
}

func TestInjectedProtectAndErrors(t *testing.T) {
	oldProtect, oldUnprotect := Protect, Unprotect
	t.Cleanup(func() {
		Protect = oldProtect
		Unprotect = oldUnprotect
	})

	Protect = func(b []byte) ([]byte, error) {
		out := make([]byte, len(b))
		for i, v := range b {
			out[i] = v ^ 0xa5
		}
		return append([]byte("v1:"), out...), nil
	}
	Unprotect = func(b []byte) ([]byte, error) {
		body, ok := bytes.CutPrefix(b, []byte("v1:"))
		if !ok {
			return nil, errors.New("wrong dpapi key")
		}
		out := make([]byte, len(body))
		for i, v := range body {
			out[i] = v ^ 0xa5
		}
		return out, nil
	}

	s, err := Seal([]byte("secret-seed"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s, "secret-seed") {
		t.Fatalf("sealed string leaked plaintext: %q", s)
	}
	got, err := Open(s)
	if err != nil || string(got) != "secret-seed" {
		t.Fatalf("Open(%q) = %q, %v", s, got, err)
	}

	if _, err := Open("dpapi:!!!not-base64!!!"); err == nil {
		t.Fatal("accepted invalid base64 after dpapi:")
	}

	Unprotect = func([]byte) ([]byte, error) {
		return nil, errors.New("wrong machine")
	}
	if _, err := Open(s); err == nil {
		t.Fatal("expected error when Unprotect fails")
	}

	Protect = func([]byte) ([]byte, error) {
		return nil, errors.New("dpapi unavailable")
	}
	if _, err := Seal([]byte("x")); err == nil {
		t.Fatal("expected error when Protect fails")
	}
}
