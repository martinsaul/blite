package update

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	molt "moltclient"
)

// A Molt stand-in serving one signed manifest, signed by key.
func server(t *testing.T, key ed25519.PrivateKey, p molt.Payload, status int) *httptest.Server {
	t.Helper()
	raw, _ := json.Marshal(p)
	env, _ := json.Marshal(molt.Sign(raw, key))
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/manifest/blite/stable/linux-amd64" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		w.Write(env)
	}))
	t.Cleanup(s.Close)
	return s
}

func updater(base string, pub ed25519.PublicKey) *Updater {
	return &Updater{
		c: &molt.Client{BaseURL: base, Package: "blite", Channel: "stable", Platform: "linux-amd64",
			Version: "0.1.0", Build: 17, Keys: []ed25519.PublicKey{pub}},
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		info: Info{Channel: "stable"},
	}
}

func payload(build, minSupported int64) molt.Payload {
	now := time.Now().UTC()
	return molt.Payload{Schema: molt.SchemaVersion, Package: "blite", Channel: "stable", Platform: "linux-amd64",
		Version: "0.1.1", Build: build, Kind: "deb", Filename: "b.deb", SHA256: strings.Repeat("ab", 32), Size: 1,
		Notes: "faster", MinSupportedBuild: minSupported, IssuedAt: now, ExpiresAt: now.Add(time.Hour)}
}

func TestCheck(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(nil)
	ctx := context.Background()

	u := updater(server(t, key, payload(23, 0), 200).URL, pub)
	u.Check(ctx)
	if i := u.Info(); i.Available != "0.1.1 (23)" || i.Notes != "faster" || i.Mandatory || i.Problem != "" || i.Checked == "" {
		t.Fatalf("newer build: %+v", i)
	}

	u = updater(server(t, key, payload(23, 20), 200).URL, pub)
	u.Check(ctx)
	if !u.Mandatory() {
		t.Fatal("below minSupportedBuild should be mandatory")
	}

	u = updater(server(t, key, payload(17, 0), 200).URL, pub)
	u.Check(ctx)
	if i := u.Info(); i.Available != "" || i.Checked == "" {
		t.Fatalf("same build should be up to date: %+v", i)
	}

	// Signed by someone else: a visible problem, nothing offered.
	_, other, _ := ed25519.GenerateKey(nil)
	u = updater(server(t, other, payload(23, 0), 200).URL, pub)
	u.Check(ctx)
	if i := u.Info(); i.Available != "" || i.Problem == "" {
		t.Fatalf("bad signature: %+v", i)
	}

	// Offline or nothing published: quiet.
	u = updater("http://127.0.0.1:1", pub)
	u.Check(ctx)
	if i := u.Info(); i.Problem != "" || i.Available != "" {
		t.Fatalf("offline should be quiet: %+v", i)
	}
	u = updater(server(t, key, payload(23, 0), 404).URL, pub)
	u.Check(ctx)
	if i := u.Info(); i.Problem != "" || i.Available != "" {
		t.Fatalf("no release should be quiet: %+v", i)
	}

	if New("dev", 0, "stable", false, nil) != nil {
		t.Fatal("dev builds must not self-update")
	}
	if err := u.Install(ctx); err == nil {
		t.Fatal("install with nothing pending")
	}
}

// Against the real Molt with the baked-in key. Opt-in:
//
//	BLITE_MOLT_LIVE=1 go test -run TestLiveMolt ./internal/update
//
// TestLive checks a real update server: BLITE_UPDATE_URL and BLITE_UPDATE_KEYS.
func TestLive(t *testing.T) {
	serverURL, trustedKeys = os.Getenv("BLITE_UPDATE_URL"), os.Getenv("BLITE_UPDATE_KEYS")
	defer func() { serverURL, trustedKeys = "", "" }()
	if serverURL == "" || trustedKeys == "" {
		t.Skip("set BLITE_UPDATE_URL and BLITE_UPDATE_KEYS")
	}
	u := New("0.1.0", 1, "stable", false, slog.New(slog.NewTextHandler(io.Discard, nil)))
	u.Check(context.Background())
	i := u.Info()
	if i.Problem != "" || i.Checked == "" {
		t.Fatalf("live check: %+v", i)
	}
	t.Logf("live: %+v", i)
}
