package camera

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

var testIDKey = bytes.Repeat([]byte{7}, idKeyLen)

// TestMain injects a fixed ID key, so tests never touch the user's config.
func TestMain(m *testing.M) {
	idKeyOnce.Do(func() { idKey = testIDKey })
	os.Exit(m.Run())
}

func TestIDKeyFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "blite", "camera-id.key")
	old := idKeyPath
	idKeyPath = func() (string, error) { return p, nil }
	defer func() { idKeyPath = old }()

	k1, err := loadOrCreateIDKey()
	if err != nil || len(k1) != idKeyLen {
		t.Fatalf("create: %x %v", k1, err)
	}
	if st, err := os.Stat(p); err != nil {
		t.Fatal(err)
	} else if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Errorf("key file mode %v, want 0600", st.Mode().Perm())
	}
	k2, err := loadOrCreateIDKey()
	if err != nil || !bytes.Equal(k1, k2) {
		t.Errorf("reload gave another key: %x %x %v", k1, k2, err)
	}
	// A corrupt file isn't overwritten; the caller falls back to a per-process key.
	if err := os.WriteFile(p, []byte("nonsense"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateIDKey(); err == nil {
		t.Error("corrupt key accepted")
	}
	if b, _ := os.ReadFile(p); string(b) != "nonsense" {
		t.Error("corrupt key file overwritten")
	}
}

func TestIDDependsOnKey(t *testing.T) {
	a := hashID(BackendV4L2, "/dev/v4l/by-id/usb-046d_HD_Pro_Webcam_C920_ABCD1234-video-index0")
	old := idKey
	idKey = bytes.Repeat([]byte{8}, idKeyLen)
	b := hashID(BackendV4L2, "/dev/v4l/by-id/usb-046d_HD_Pro_Webcam_C920_ABCD1234-video-index0")
	idKey = old
	if a == b || len(a) != 12 || len(b) != 12 {
		t.Errorf("ids %s %s: the key must change the ID", a, b)
	}
}
