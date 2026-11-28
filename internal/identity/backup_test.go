package identity

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupRoundTrip(t *testing.T) {
	k := Ephemeral()
	b, err := k.Backup("Alice", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	got, name, err := OpenBackup(b, "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if got.Public() != k.Public() || name != "Alice" {
		t.Fatalf("got %s %q, want %s Alice", got.Public(), name, k.Public())
	}
	if _, _, err := OpenBackup(b, "wrong horse!"); err != ErrBackupOpen {
		t.Fatalf("wrong passphrase: %v", err)
	}
	if _, err := k.Backup("", "short"); err != ErrShortPassphrase {
		t.Fatalf("short passphrase: %v", err)
	}
}

func TestBackupTampered(t *testing.T) {
	b, err := Ephemeral().Backup("", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	edit := func(f func(m map[string]any)) []byte {
		var m map[string]any
		json.Unmarshal(b, &m)
		f(m)
		out, _ := json.Marshal(m)
		return out
	}
	cases := map[string][]byte{
		"garbage":       []byte("not json"),
		"other format":  edit(func(m map[string]any) { m["format"] = "something-else" }),
		"cheaper kdf":   edit(func(m map[string]any) { m["t"] = 1 }), // header is sealed in
		"huge memory":   edit(func(m map[string]any) { m["m"] = 1 << 30 }),
		"zero threads":  edit(func(m map[string]any) { m["p"] = 0 }),
		"truncated box": edit(func(m map[string]any) { m["box"] = "AAAA" }),
		"short nonce":   edit(func(m map[string]any) { m["nonce"] = "AAAA" }),
	}
	for name, data := range cases {
		if _, _, err := OpenBackup(data, "correct horse"); err != ErrBackupOpen {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

func TestInstall(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("APPDATA", cfg)
	t.Setenv("HOME", cfg)
	first, err := LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	k := Ephemeral()
	if err := Install(k); err != nil {
		t.Fatal(err)
	}
	got, err := LoadOrCreate()
	if err != nil || got.Public() != k.Public() {
		t.Fatalf("after Install: %v %s, want %s", err, got.Public(), k.Public())
	}
	d, _ := dir()
	old, err := os.ReadFile(filepath.Join(d, "identity.key.old"))
	if err != nil || strings.TrimSpace(string(old)) != base64.StdEncoding.EncodeToString(first.Private.Seed()) {
		t.Fatalf("old key not kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(d, "identity.key.tmp")); !os.IsNotExist(err) {
		t.Fatalf("temp file left: %v", err)
	}
}
