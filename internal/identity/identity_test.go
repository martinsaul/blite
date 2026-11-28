package identity

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"blite/internal/secret"
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

func TestSignPurpose(t *testing.T) {
	k := Ephemeral()
	sig := k.Sign("discord-follow", []byte("abc"))
	if err := Verify(k.Public(), "discord-follow", []byte("abc"), sig); err != nil {
		t.Fatal(err)
	}
	if Verify(k.Public(), "other", []byte("abc"), sig) == nil || Verify(k.Public(), "discord-follow", []byte("abd"), sig) == nil {
		t.Fatal("signature valid for another purpose or message")
	}
	if Verify(Ephemeral().Public(), "discord-follow", []byte("abc"), sig) == nil {
		t.Fatal("signature valid for another key")
	}
}

func TestFriendlyName(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		n := FriendlyName()
		if CleanName(n) != n || len(strings.Fields(n)) != 2 {
			t.Fatalf("FriendlyName() = %q", n)
		}
		seen[n] = true
	}
	if len(seen) < 50 {
		t.Errorf("only %d different names in 200", len(seen))
	}
}

func tempIdentityDir(t *testing.T) string {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("APPDATA", cfg)
	t.Setenv("HOME", cfg)
	d, err := dir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

func mockDPAPI(t *testing.T, tag string) {
	t.Helper()
	oldEnabled := secret.Enabled
	oldProtect := secret.Protect
	oldUnprotect := secret.Unprotect
	t.Cleanup(func() {
		secret.Enabled = oldEnabled
		secret.Protect = oldProtect
		secret.Unprotect = oldUnprotect
	})
	secret.Enabled = true
	prefix := []byte(tag + ":")
	secret.Protect = func(b []byte) ([]byte, error) {
		out := make([]byte, len(b))
		for i, v := range b {
			out[i] = v ^ 0x5c
		}
		return append(append([]byte(nil), prefix...), out...), nil
	}
	secret.Unprotect = func(b []byte) ([]byte, error) {
		body, ok := bytes.CutPrefix(b, prefix)
		if !ok {
			return nil, errors.New("dpapi: wrong user or machine")
		}
		out := make([]byte, len(body))
		for i, v := range body {
			out[i] = v ^ 0x5c
		}
		return out, nil
	}
}

func TestLoadOldPlainIdentityKey(t *testing.T) {
	d := tempIdentityDir(t)
	want := Ephemeral()
	plain := base64.StdEncoding.EncodeToString(want.Private.Seed()) + "\n"
	keyPath := filepath.Join(d, "identity.key")
	if err := os.WriteFile(keyPath, []byte(plain), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadOrCreate()
	if err != nil {
		t.Fatalf("LoadOrCreate plain key: %v", err)
	}
	if got.Public() != want.Public() {
		t.Fatalf("loaded public = %s, want %s", got.Public(), want.Public())
	}
	// On non-Windows (secret.Enabled == false), the file stays plain.
	disk, err := os.ReadFile(keyPath)
	if err != nil || string(disk) != plain {
		t.Fatalf("non-Windows rewrote plain key: %q, err=%v", disk, err)
	}
}

func TestWindowsCreateMigrateAndInstall(t *testing.T) {
	mockDPAPI(t, "pc1")

	// 1. Create path writes dpapi:<base64> and reloads the same key.
	d := tempIdentityDir(t)
	keyPath := filepath.Join(d, "identity.key")
	created, err := LoadOrCreate()
	if err != nil {
		t.Fatalf("LoadOrCreate create: %v", err)
	}
	rawBytes, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	raw := strings.TrimSpace(string(rawBytes))
	if !secret.Sealed(raw) {
		t.Fatalf("created key not sealed: %q", raw)
	}
	if strings.Contains(raw, base64.StdEncoding.EncodeToString(created.Private.Seed())) {
		t.Fatalf("created key leaked raw base64 seed: %q", raw)
	}
	reloaded, err := LoadOrCreate()
	if err != nil || reloaded.Public() != created.Public() {
		t.Fatalf("reload sealed key = %v %s, want %s", err, reloaded.Public(), created.Public())
	}

	// 2. Migration of a plaintext key seals via .tmp, verifies Open, and renames.
	plainKey := Ephemeral()
	plainContent := base64.StdEncoding.EncodeToString(plainKey.Private.Seed()) + "\n"
	if err := os.WriteFile(keyPath, []byte(plainContent), 0o600); err != nil {
		t.Fatal(err)
	}
	migrated, err := LoadOrCreate()
	if err != nil || migrated.Public() != plainKey.Public() {
		t.Fatalf("LoadOrCreate migrate = %v %s, want %s", err, migrated.Public(), plainKey.Public())
	}
	afterBytes, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !secret.Sealed(strings.TrimSpace(string(afterBytes))) {
		t.Fatalf("migrated file is not dpapi-prefixed: %q", afterBytes)
	}
	if _, err := os.Stat(keyPath + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("identity.key.tmp left behind: %v", err)
	}
	again, err := LoadOrCreate()
	if err != nil || again.Public() != plainKey.Public() {
		t.Fatalf("reload after migration = %v %s, want %s", err, again.Public(), plainKey.Public())
	}

	// 3. Install seals both identity.key and identity.key.old (even if old was plain).
	if err := os.WriteFile(keyPath, []byte(plainContent), 0o600); err != nil {
		t.Fatal(err)
	}
	installed := Ephemeral()
	if err := Install(installed); err != nil {
		t.Fatalf("Install: %v", err)
	}
	oldBytes, err := os.ReadFile(keyPath + ".old")
	if err != nil {
		t.Fatalf("read identity.key.old: %v", err)
	}
	oldRaw := strings.TrimSpace(string(oldBytes))
	if !secret.Sealed(oldRaw) {
		t.Fatalf("identity.key.old not sealed on Windows: %q", oldRaw)
	}
	oldSeed, err := secret.Open(oldRaw)
	if err != nil || !bytes.Equal(oldSeed, plainKey.Private.Seed()) {
		t.Fatalf("identity.key.old Open = %x, %v; want %x", oldSeed, err, plainKey.Private.Seed())
	}
	gotInstalled, err := LoadOrCreate()
	if err != nil || gotInstalled.Public() != installed.Public() {
		t.Fatalf("after Install LoadOrCreate = %v %s, want %s", err, gotInstalled.Public(), installed.Public())
	}
}

func TestWindowsMigrationFailureKeepsPlainKey(t *testing.T) {
	mockDPAPI(t, "pc1")
	d := tempIdentityDir(t)
	keyPath := filepath.Join(d, "identity.key")

	orig := Ephemeral()
	plain := base64.StdEncoding.EncodeToString(orig.Private.Seed()) + "\n"
	if err := os.WriteFile(keyPath, []byte(plain), 0o600); err != nil {
		t.Fatal(err)
	}

	// Case A: Unprotect fails when verifying identity.key.tmp -> must keep plain file.
	goodUnprotect := secret.Unprotect
	secret.Unprotect = func([]byte) ([]byte, error) {
		return nil, errors.New("simulated unprotect failure")
	}
	got, err := LoadOrCreate()
	if err != nil || got.Public() != orig.Public() {
		t.Fatalf("migration with failing Unprotect = %v %s, want %s", err, got.Public(), orig.Public())
	}
	disk, err := os.ReadFile(keyPath)
	if err != nil || string(disk) != plain {
		t.Fatalf("failing Unprotect overwrote plain key: got %q, err=%v", disk, err)
	}
	if _, err := os.Stat(keyPath + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind: %v", err)
	}

	// Case B: Unprotect returns wrong seed bytes -> must keep plain file.
	secret.Unprotect = func([]byte) ([]byte, error) {
		bad := make([]byte, len(orig.Private.Seed()))
		return bad, nil
	}
	got, err = LoadOrCreate()
	if err != nil || got.Public() != orig.Public() {
		t.Fatalf("migration with mismatched Unprotect = %v %s, want %s", err, got.Public(), orig.Public())
	}
	disk, err = os.ReadFile(keyPath)
	if err != nil || string(disk) != plain {
		t.Fatalf("mismatched Unprotect overwrote plain key: got %q, err=%v", disk, err)
	}

	// Case C: Protect fails -> must keep plain file.
	secret.Unprotect = goodUnprotect
	secret.Protect = func([]byte) ([]byte, error) {
		return nil, errors.New("simulated protect failure")
	}
	got, err = LoadOrCreate()
	if err != nil || got.Public() != orig.Public() {
		t.Fatalf("migration with failing Protect = %v %s, want %s", err, got.Public(), orig.Public())
	}
	disk, err = os.ReadFile(keyPath)
	if err != nil || string(disk) != plain {
		t.Fatalf("failing Protect overwrote plain key: got %q, err=%v", disk, err)
	}
}

func TestWindowsSealedKeyWrongAccountDoesNotOverwrite(t *testing.T) {
	mockDPAPI(t, "pc1")
	d := tempIdentityDir(t)
	keyPath := filepath.Join(d, "identity.key")

	first, err := LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	sealedPC1, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate copying profile to another computer / Windows account ("pc2").
	mockDPAPI(t, "pc2")
	_, err = LoadOrCreate()
	if !errors.Is(err, ErrWrongAccount) {
		t.Fatalf("LoadOrCreate on wrong account = %v, want ErrWrongAccount", err)
	}
	afterErr, err := os.ReadFile(keyPath)
	if err != nil || !bytes.Equal(afterErr, sealedPC1) {
		t.Fatalf("wrong-account LoadOrCreate modified identity.key: got %q, want %q", afterErr, sealedPC1)
	}

	// Restoring a backup via Install on pc2 works and keeps pc1's sealed file as .old.
	if err := Install(first); err != nil {
		t.Fatalf("Install on pc2: %v", err)
	}
	oldBytes, err := os.ReadFile(keyPath + ".old")
	if err != nil || !bytes.Equal(oldBytes, sealedPC1) {
		t.Fatalf("Install did not preserve foreign identity.key.old: %v", err)
	}
	restored, err := LoadOrCreate()
	if err != nil || restored.Public() != first.Public() {
		t.Fatalf("LoadOrCreate after restore on pc2 = %v %s, want %s", err, restored.Public(), first.Public())
	}
}
