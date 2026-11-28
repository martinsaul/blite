package client

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func init() { retryDelay = time.Millisecond }

// cutWriter forwards at most limit bytes of the body, then aborts the
// connection, the way a proxy dropping a slow transfer does.
type cutWriter struct {
	http.ResponseWriter
	limit int
}

func (c *cutWriter) Write(b []byte) (int, error) {
	if c.limit <= 0 {
		panic(http.ErrAbortHandler)
	}
	if len(b) > c.limit {
		b = b[:c.limit]
	}
	n, err := c.ResponseWriter.Write(b)
	c.limit -= n
	return n, err
}

func testPayload(t *testing.T, url string, body []byte) Payload {
	t.Helper()
	sum := sha256.Sum256(body)
	return Payload{URL: url, Filename: "app.zip", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(body))}
}

func TestDownloadResumesAfterDroppedConnection(t *testing.T) {
	body := bytes.Repeat([]byte("molt-resume-"), 50_000) // 600 KB
	var requests, ranged atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		if r.Header.Get("Range") != "" {
			ranged.Add(1)
		}
		var rw http.ResponseWriter = w
		if n <= 2 { // the first two transfers are cut after 200 KB
			rw = &cutWriter{ResponseWriter: w, limit: 200_000}
		}
		http.ServeContent(rw, r, "", time.Time{}, bytes.NewReader(body))
	}))
	defer srv.Close()

	p := testPayload(t, srv.URL+"/blob", body)
	dir := t.TempDir()
	got, err := download(context.Background(), p, dir, "test", nil)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	data, _ := os.ReadFile(got)
	if !bytes.Equal(data, body) {
		t.Fatalf("content mismatch: %d bytes", len(data))
	}
	if requests.Load() != 3 || ranged.Load() != 2 {
		t.Fatalf("requests=%d ranged=%d, want 3 and 2", requests.Load(), ranged.Load())
	}
	if parts, _ := filepath.Glob(filepath.Join(dir, ".molt-dl-*")); len(parts) != 0 {
		t.Fatalf("partial files left behind: %v", parts)
	}
}

func TestDownloadRestartsWhenRangeIgnored(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 300_000)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rw http.ResponseWriter = w
		if requests.Add(1) == 1 {
			rw = &cutWriter{ResponseWriter: w, limit: 100_000}
		}
		w.Write(nil) // ignore Range: always a full 200
		rw.Write(body)
	}))
	defer srv.Close()

	got, err := download(context.Background(), testPayload(t, srv.URL, body), t.TempDir(), "test", nil)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if data, _ := os.ReadFile(got); !bytes.Equal(data, body) {
		t.Fatalf("content mismatch: %d bytes", len(data))
	}
}

func TestDownloadRejectsWrongHash(t *testing.T) {
	body := []byte("the real payload")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader([]byte("a forged payload")))
	}))
	defer srv.Close()

	dir := t.TempDir()
	_, err := download(context.Background(), testPayload(t, srv.URL, body), dir, "test", nil)
	if !errors.Is(err, ErrBadPayload) {
		t.Fatalf("err = %v, want ErrBadPayload", err)
	}
	if parts, _ := filepath.Glob(filepath.Join(dir, "*")); len(parts) != 0 {
		t.Fatalf("files left behind: %v", parts)
	}
}

// A transfer that stops sending is dropped after stallTimeout and resumed;
// one that keeps trickling is never cut, however long it takes.
func TestDownloadStallAndSlowLink(t *testing.T) {
	old := stallTimeout
	stallTimeout = 200 * time.Millisecond
	defer func() { stallTimeout = old }()
	body := bytes.Repeat([]byte("molt-stall-"), 2_000) // 22 KB
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		if n == 1 { // half the body, then silence
			w.Header().Set("Content-Length", itoa(int64(len(body))))
			w.Write(body[:len(body)/2])
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		// The rest in 10 slow pieces: 500 ms in all, past stallTimeout.
		start := 0
		if rg := r.Header.Get("Range"); rg != "" {
			start = len(body) / 2
			w.Header().Set("Content-Range", "bytes "+itoa(int64(start))+"-"+itoa(int64(len(body)-1))+"/"+itoa(int64(len(body))))
			w.Header().Set("Content-Length", itoa(int64(len(body)-start)))
			w.WriteHeader(http.StatusPartialContent)
		}
		rest := body[start:]
		step := len(rest)/10 + 1
		for i := 0; i < len(rest); i += step {
			w.Write(rest[i:min(i+step, len(rest))])
			w.(http.Flusher).Flush()
			time.Sleep(50 * time.Millisecond)
		}
	}))
	defer srv.Close()
	var last, total atomic.Int64
	got, err := download(context.Background(), testPayload(t, srv.URL, body), t.TempDir(), "test",
		func(done, size int64) { last.Store(done); total.Store(size) })
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(got); !bytes.Equal(b, body) {
		t.Fatal("wrong content")
	}
	if requests.Load() != 2 {
		t.Fatalf("%d requests, want 2 (one stalled, one resumed)", requests.Load())
	}
	if last.Load() != int64(len(body)) || total.Load() != int64(len(body)) {
		t.Fatalf("progress ended at %d of %d", last.Load(), total.Load())
	}
}

func TestDownloadSchemeCheck(t *testing.T) {
	body := []byte("payload-bytes")
	for _, bad := range []string{
		"http://updates.example.com/app.zip",
		"http://192.168.1.10/app.zip",
		"ftp://127.0.0.1/app.zip",
		"file:///tmp/app.zip",
		"://broken",
	} {
		if _, err := download(context.Background(), testPayload(t, bad, body), t.TempDir(), "test", nil); !errors.Is(err, ErrBadPayload) {
			t.Fatalf("url %q: got %v, want ErrBadPayload", bad, err)
		}
	}

	// A redirect from loopback http (or https) to non-loopback http must also be refused.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://updates.example.com/app.zip", http.StatusFound)
	}))
	defer srv.Close()
	if _, err := download(context.Background(), testPayload(t, srv.URL+"/blob", body), t.TempDir(), "test", nil); err == nil {
		t.Fatal("expected redirect to non-loopback http to fail")
	}

	// HTTPS and loopback HTTP are allowed by the scheme check.
	for _, okURL := range []string{
		"https://updates.example.com/app.zip",
		"http://127.0.0.1:8080/app.zip",
		"http://[::1]:8080/app.zip",
		"http://localhost:8080/app.zip",
	} {
		if err := checkPayloadURL(okURL); err != nil {
			t.Fatalf("checkPayloadURL(%q) = %v, want nil", okURL, err)
		}
	}
}

func swapTestClient(t *testing.T, srvURL string, pub ed25519.PublicKey) *Client {
	t.Helper()
	return &Client{
		BaseURL:  srvURL,
		Package:  "blite",
		Channel:  "stable",
		Platform: "linux-amd64",
		Version:  "0.1.0",
		Build:    10,
		Keys:     []ed25519.PublicKey{pub},
	}
}

func signedSwapManifest(t *testing.T, key ed25519.PrivateKey, build int64, blobURL string, body []byte) []byte {
	t.Helper()
	now := time.Now().UTC()
	sum := sha256.Sum256(body)
	p := Payload{
		Schema:    SchemaVersion,
		Package:   SwapPackage,
		Channel:   "stable",
		Platform:  "linux-amd64",
		Version:   "0.1.0",
		Build:     build,
		Kind:      "bin",
		Filename:  "molt-swap",
		SHA256:    hex.EncodeToString(sum[:]),
		Size:      int64(len(body)),
		URL:       blobURL,
		IssuedAt:  now,
		ExpiresAt: now.Add(time.Hour),
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	env, err := json.Marshal(Sign(raw, key))
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func TestFetchSwapRefusesInsecureURL(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(nil)
	body := []byte("swap-binary")
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/manifest/molt-swap/stable/linux-amd64":
			w.Write(signedSwapManifest(t, key, 5, "http://evil.example.com/molt-swap", body))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := swapTestClient(t, srv.URL, pub)
	_, err := c.fetchSwap(context.Background(), t.TempDir())
	if !errors.Is(err, ErrBadPayload) {
		t.Fatalf("fetchSwap insecure URL: got %v, want ErrBadPayload", err)
	}
}

func TestFetchSwapNoDowngrade(t *testing.T) {
	oldMin := MinSwapBuild
	defer func() { MinSwapBuild = oldMin }()
	MinSwapBuild = 0

	pub, key, _ := ed25519.GenerateKey(nil)
	body := []byte("#!/bin/sh\nexit 0\n")
	var currentBuild atomic.Int64
	currentBuild.Store(5)

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/manifest/molt-swap/stable/linux-amd64":
			w.Write(signedSwapManifest(t, key, currentBuild.Load(), srv.URL+"/swap.bin", body))
		case "/swap.bin":
			w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cache := t.TempDir()

	// MinSwapBuild floor: refuse build 4 when MinSwapBuild is 5.
	MinSwapBuild = 5
	currentBuild.Store(4)
	c1 := swapTestClient(t, srv.URL, pub)
	if _, err := c1.fetchSwap(context.Background(), cache); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("below MinSwapBuild: got %v, want ErrBadSignature", err)
	}

	// Equal to MinSwapBuild (build 5) is accepted and records high-water mark 5.
	currentBuild.Store(5)
	if _, err := c1.fetchSwap(context.Background(), cache); err != nil {
		t.Fatalf("build 5 (== MinSwapBuild): %v", err)
	}

	// Equal build (5) on another call is accepted.
	if _, err := c1.fetchSwap(context.Background(), cache); err != nil {
		t.Fatalf("build 5 repeat: %v", err)
	}

	// Higher build (7) is accepted and advances high-water mark to 7.
	currentBuild.Store(7)
	if _, err := c1.fetchSwap(context.Background(), cache); err != nil {
		t.Fatalf("build 7 (> 5): %v", err)
	}

	// Lower build (6) is refused even though 6 >= MinSwapBuild (5),
	// and the high-water mark persists across Client instances.
	currentBuild.Store(6)
	c2 := swapTestClient(t, srv.URL, pub)
	if _, err := c2.fetchSwap(context.Background(), cache); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("downgrade to 6 on new Client: got %v, want ErrBadSignature", err)
	}

	// Apply refuses to run the helper when downgraded and keeps the current install untouched.
	installDir := filepath.Join(t.TempDir(), "install")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatal(err)
	}
	existingExe := filepath.Join(installDir, "blite")
	if err := os.WriteFile(existingExe, []byte("current-install"), 0o755); err != nil {
		t.Fatal(err)
	}
	upPayload := testPayload(t, srv.URL+"/swap.bin", body)
	upPayload.Build = 20
	upPayload.Kind = "zip"
	payloadPath, err := c2.Download(context.Background(), upPayload, cache)
	if err != nil {
		t.Fatal(err)
	}
	err = c2.Apply(context.Background(), &Update{Payload: upPayload}, payloadPath, ApplyOptions{
		InstallDir: installDir,
		Exe:        "blite",
		CacheDir:   cache,
	})
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("Apply with downgraded swap: got %v, want ErrBadSignature", err)
	}
	if got, err := os.ReadFile(existingExe); err != nil || string(got) != "current-install" {
		t.Fatalf("current install modified: %q, %v", got, err)
	}
}
