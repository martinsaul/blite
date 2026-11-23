package update

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

func TestRetry(t *testing.T) {
	defer func(w []time.Duration) { retryWaits = w }(retryWaits)
	retryWaits = []time.Duration{time.Millisecond, time.Millisecond}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	n := 0
	err := retry(ctx, log, func() error {
		if n++; n < 3 {
			return fmt.Errorf("%w: connection reset", molt.ErrCouldNotCheck)
		}
		return nil
	})
	if err != nil || n != 3 {
		t.Fatalf("transient: err %v after %d tries, want success after 3", err, n)
	}

	n = 0
	err = retry(ctx, log, func() error { n++; return fmt.Errorf("%w: size 1, want 2", molt.ErrBadPayload) })
	if !errors.Is(err, molt.ErrBadPayload) || n != 3 {
		t.Fatalf("persistent: err %v after %d tries, want ErrBadPayload after 3", err, n)
	}

	n = 0
	err = retry(ctx, log, func() error { n++; return molt.ErrBadSignature })
	if !errors.Is(err, molt.ErrBadSignature) || n != 1 {
		t.Fatalf("tampering: err %v after %d tries, want no retry", err, n)
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	n = 0
	retryWaits = []time.Duration{time.Hour}
	err = retry(cctx, log, func() error { n++; return molt.ErrCouldNotCheck })
	if n != 1 || err == nil {
		t.Fatalf("cancelled: %d tries, want 1", n)
	}
}

// One install at a time, with progress: a second Install while the first
// downloads gets ErrInstalling, and the UI hears the progress.
func TestInstallSingleFlight(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	body := []byte(strings.Repeat("x", 1000))
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/blob" {
			http.NotFound(w, r) // no molt-swap published: Apply fails after the download
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.Write(body[:500])
		w.(http.Flusher).Flush()
		<-release
		w.Write(body[500:])
	}))
	defer srv.Close()
	u := updater(srv.URL, pub)
	u.opts.CacheDir = t.TempDir()
	u.info.CanApply = true
	p := payload(23, 0)
	sum := sha256.Sum256(body)
	p.URL, p.Size, p.SHA256 = srv.URL+"/blob", int64(len(body)), hex.EncodeToString(sum[:])
	u.pending = &molt.Update{Payload: p}
	u.c.Progress = u.progress
	var changes atomic.Int32
	u.OnChange = func() { changes.Add(1) }

	done := make(chan error, 1)
	go func() { done <- u.Install(context.Background()) }()
	deadline := time.Now().Add(5 * time.Second)
	for i := u.Info(); !(i.Downloading && i.Progress == 50); i = u.Info() {
		if time.Now().After(deadline) {
			t.Fatalf("no progress: %+v", i)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := u.Install(context.Background()); !errors.Is(err, ErrInstalling) {
		t.Fatalf("second Install: %v, want ErrInstalling", err)
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("Apply should fail without molt-swap")
	}
	if i := u.Info(); i.Downloading || i.Problem == "" {
		t.Fatalf("after the install: %+v", i)
	}
	if changes.Load() < 3 {
		t.Fatalf("%d change notifications", changes.Load())
	}
}

// Stage downloads the update for the next start without showing progress,
// and removes what older builds left in the cache (not molt-swap's files).
func TestStage(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	body := []byte(strings.Repeat("y", 1000))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()
	u := updater(srv.URL, pub)
	cache := t.TempDir()
	u.opts.CacheDir = cache
	u.info.CanApply = true
	p := payload(23, 0)
	sum := sha256.Sum256(body)
	p.URL, p.Size, p.SHA256, p.Filename = srv.URL+"/blob", int64(len(body)), hex.EncodeToString(sum[:]), "blite-23.deb"
	u.pending = &molt.Update{Payload: p}
	u.c.Progress = u.progress
	var changes atomic.Int32
	u.OnChange = func() { changes.Add(1) }
	for _, n := range []string{"blite-21.deb", ".molt-dl-old.part", "last-update.json", "molt-swap.log"} {
		os.WriteFile(filepath.Join(cache, n), []byte("old"), 0o600)
	}

	u.Stage(context.Background())
	if b, err := os.ReadFile(filepath.Join(cache, "blite-23.deb")); err != nil || string(b) != string(body) {
		t.Fatalf("staged file: %q %v", b, err)
	}
	for n, want := range map[string]bool{"blite-21.deb": false, ".molt-dl-old.part": false, "last-update.json": true, "molt-swap.log": true} {
		if _, err := os.Stat(filepath.Join(cache, n)); (err == nil) != want {
			t.Errorf("%s: kept=%v, want %v", n, err == nil, want)
		}
	}
	if i := u.Info(); changes.Load() != 0 || i.Downloading || i.Progress != 0 {
		t.Fatalf("Stage showed progress: %d changes, %+v", changes.Load(), i)
	}
}

// An Install while Stage downloads takes the download over (Stage stops),
// and an Install given up on (Skip at start) is no problem to show.
func TestInstallTakesOverStage(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	body := []byte(strings.Repeat("z", 1000))
	var reqs atomic.Int32
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/blob" {
			http.NotFound(w, r) // Apply fails after the download
			return
		}
		if reqs.Add(1) == 1 { // the Stage: hangs until dropped
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			w.Write(body[:100])
			w.(http.Flusher).Flush()
			close(started)
			<-r.Context().Done()
			return
		}
		w.Write(body)
	}))
	defer srv.Close()
	u := updater(srv.URL, pub)
	u.opts.CacheDir = t.TempDir()
	u.info.CanApply = true
	p := payload(23, 0)
	sum := sha256.Sum256(body)
	p.URL, p.Size, p.SHA256 = srv.URL+"/blob", int64(len(body)), hex.EncodeToString(sum[:])
	u.pending = &molt.Update{Payload: p}

	staged := make(chan struct{})
	go func() { u.Stage(context.Background()); close(staged) }()
	<-started
	if err := u.Install(context.Background()); err == nil {
		t.Fatal("Install: Apply should fail without an install layout")
	}
	select {
	case <-staged:
	case <-time.After(5 * time.Second):
		t.Fatal("Stage still running")
	}
	if _, err := os.Stat(filepath.Join(u.opts.CacheDir, "b.deb")); err != nil {
		t.Fatalf("Install didn't finish the download: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	u.info.Problem = ""
	if err := u.InstallAtStart(ctx, []string{"-background"}); err == nil {
		t.Fatal("a cancelled install succeeded")
	}
	if i := u.Info(); i.Problem != "" {
		t.Fatalf("Skip shows a problem: %q", i.Problem)
	}
}
