package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	got, err := download(context.Background(), p, dir, "test")
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

	got, err := download(context.Background(), testPayload(t, srv.URL, body), t.TempDir(), "test")
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
	_, err := download(context.Background(), testPayload(t, srv.URL, body), dir, "test")
	if !errors.Is(err, ErrBadPayload) {
		t.Fatalf("err = %v, want ErrBadPayload", err)
	}
	if parts, _ := filepath.Glob(filepath.Join(dir, "*")); len(parts) != 0 {
		t.Fatalf("files left behind: %v", parts)
	}
}
