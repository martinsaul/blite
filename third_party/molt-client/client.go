package client

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// maxManifestBytes bounds a manifest response; real ones are ~1 KB.
const maxManifestBytes = 64 << 10

// Client checks for and downloads updates for one installed app. All fields
// except HTTP, Now and Platform are required.
type Client struct {
	BaseURL  string              // the update server, e.g. https://updates.example.com
	Package  string              // e.g. "blite"
	Channel  string              // e.g. "stable"
	Platform string              // default runtime.GOOS + "-" + runtime.GOARCH
	Version  string              // installed version name, e.g. "0.1.0"
	Build    int64               // installed build number; the ordering key
	Keys     []ed25519.PublicKey // trusted signing keys (baked into the binary)

	HTTP *http.Client     // default: 30s timeout for manifests; downloads use ctx
	Now  func() time.Time // default time.Now
}

// Update is an available, verified release newer than the installed build.
type Update struct {
	Payload
	// Mandatory is true when the installed build is below the channel's
	// minSupportedBuild. The app decides when to act (never mid-session).
	Mandatory bool
}

func (c *Client) base() string { return strings.TrimRight(c.BaseURL, "/") }

func (c *Client) platform() string {
	if c.Platform != "" {
		return c.Platform
	}
	return runtime.GOOS + "-" + runtime.GOARCH
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// UserAgent is sent as X-Client so the server can count adoption per version.
func (c *Client) UserAgent() string {
	return fmt.Sprintf("%s/%s (%d) %s", c.Package, c.Version, c.Build, c.platform())
}

// Check fetches the channel manifest. It returns (nil, nil) when the installed
// build is current (or newer: Molt never downgrades). Errors wrap
// ErrCouldNotCheck (stay quiet), ErrBadSignature (tampering) or ErrNoRelease.
func (c *Client) Check(ctx context.Context) (*Update, error) {
	if c.Package == "" || c.Channel == "" || len(c.Keys) == 0 {
		return nil, errors.New("molt: Client needs Package, Channel and Keys")
	}
	u := fmt.Sprintf("%s/v1/manifest/%s/%s/%s", c.base(),
		url.PathEscape(c.Package), url.PathEscape(c.Channel), url.PathEscape(c.platform()))
	p, err := c.fetch(ctx, u, c.Channel)
	if err != nil {
		return nil, err
	}
	if p.Build <= c.Build {
		return nil, nil
	}
	return &Update{Payload: p, Mandatory: c.Build < p.MinSupportedBuild}, nil
}

// Release fetches the build-pinned manifest for one specific build (used to
// obtain a rollback payload for the installed build).
func (c *Client) Release(ctx context.Context, build int64) (Payload, error) {
	u := fmt.Sprintf("%s/v1/release/%s/%s/%d", c.base(),
		url.PathEscape(c.Package), url.PathEscape(c.platform()), build)
	p, err := c.fetch(ctx, u, "")
	if err == nil && p.Build != build {
		err = fmt.Errorf("%w: asked for build %d, got %d", ErrBadSignature, build, p.Build)
	}
	return p, err
}

func (c *Client) fetch(ctx context.Context, u, channel string) (Payload, error) {
	return fetchManifest(ctx, c.http(), u, c.UserAgent(), c.Keys, c.Package, channel, c.platform(), c.now())
}

func fetchManifest(ctx context.Context, hc *http.Client, u, ua string, keys []ed25519.PublicKey,
	pkg, channel, platform string, now time.Time) (Payload, error) {
	var p Payload
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return p, fmt.Errorf("%w: %v", ErrCouldNotCheck, err)
	}
	req.Header.Set("X-Client", ua)
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return p, fmt.Errorf("%w: %v", ErrCouldNotCheck, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return p, ErrNoRelease
	case resp.StatusCode != http.StatusOK:
		return p, fmt.Errorf("%w: HTTP %d", ErrCouldNotCheck, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return p, fmt.Errorf("%w: %v", ErrCouldNotCheck, err)
	}
	if len(body) > maxManifestBytes {
		return p, fmt.Errorf("%w: manifest too large", ErrBadSignature)
	}
	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		// A captive portal or proxy page is far likelier than an attack.
		return p, fmt.Errorf("%w: not a manifest: %v", ErrCouldNotCheck, err)
	}
	if p, err = Verify(env, keys); err != nil {
		return p, err
	}
	return p, p.Check(pkg, channel, platform, now)
}

// Download fetches the payload into dir, verifying size and sha256 over the
// whole file before atomically renaming it to its final name. It returns the
// verified file's path. A partial or mismatched file is removed.
func (c *Client) Download(ctx context.Context, p Payload, dir string) (string, error) {
	return download(ctx, p, dir, c.UserAgent())
}

// downloadAttempts bounds how often a dropped transfer is resumed. Proxies and
// CDNs in front of the server can cut long transfers on slow links; each retry
// continues from the bytes already on disk with a Range request.
const downloadAttempts = 6

// retryDelay is the backoff unit between resume attempts (a var for tests).
var retryDelay = 2 * time.Second

func download(ctx context.Context, p Payload, dir, ua string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	final := filepath.Join(dir, safeName(p.Filename, p.SHA256))
	if ok, _ := fileMatches(final, p); ok {
		return final, nil
	}
	// The partial file is named after the hash, so a later call (even from a
	// new process) resumes it instead of starting over.
	part := filepath.Join(dir, ".molt-dl-"+p.SHA256+".part")
	var lastErr error
	for attempt := 0; attempt < downloadAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", fmt.Errorf("%w: %v", ErrCouldNotCheck, ctx.Err())
			case <-time.After(time.Duration(attempt) * retryDelay):
			}
		}
		done, err := fetchRange(ctx, p, part, ua)
		if err != nil {
			lastErr = err
			if errors.Is(err, ErrBadPayload) || ctx.Err() != nil {
				break
			}
			continue
		}
		if done {
			lastErr = nil
			break
		}
	}
	if lastErr != nil {
		if errors.Is(lastErr, ErrBadPayload) {
			os.Remove(part)
			return "", lastErr
		}
		return "", fmt.Errorf("%w: %v", ErrCouldNotCheck, lastErr)
	}
	if ok, err := fileMatches(part, p); !ok {
		os.Remove(part)
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("%w: size or sha256 mismatch, want %d bytes %s", ErrBadPayload, p.Size, p.SHA256)
	}
	if err := os.Rename(part, final); err != nil {
		return "", err
	}
	return final, nil
}

// fetchRange appends the rest of the payload to part. It reports done once
// part holds p.Size bytes; a transfer cut short returns the read error and
// keeps what arrived for the next attempt.
func fetchRange(ctx context.Context, p Payload, part, ua string) (bool, error) {
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return false, err
	}
	defer f.Close()
	have, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return false, err
	}
	if have > p.Size {
		if err := f.Truncate(0); err != nil {
			return false, err
		}
		have = 0
	}
	if have == p.Size {
		return true, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("X-Client", ua)
	if have > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(have, 10)+"-")
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusPartialContent && have > 0:
		if !strings.HasPrefix(resp.Header.Get("Content-Range"), "bytes "+strconv.FormatInt(have, 10)+"-") {
			return false, fmt.Errorf("download: unexpected Content-Range %q", resp.Header.Get("Content-Range"))
		}
	case resp.StatusCode == http.StatusOK:
		// Server ignored the range: start over.
		if err := f.Truncate(0); err != nil {
			return false, err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return false, err
		}
		have = 0
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		f.Truncate(0)
		return false, fmt.Errorf("download HTTP %d", resp.StatusCode)
	default:
		return false, fmt.Errorf("download HTTP %d", resp.StatusCode)
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, p.Size-have+1))
	if err != nil {
		return false, err
	}
	switch got := have + n; {
	case got > p.Size:
		return false, fmt.Errorf("%w: more than %d bytes", ErrBadPayload, p.Size)
	case got < p.Size:
		return false, fmt.Errorf("download ended at %d of %d bytes", got, p.Size)
	}
	return true, nil
}

func fileMatches(path string, p Payload) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return false, err
	}
	return n == p.Size && hex.EncodeToString(h.Sum(nil)) == p.SHA256, nil
}

// safeName keeps only the base name and falls back to the hash, so a hostile
// filename can never escape dir.
func safeName(name, sha string) string {
	b := filepath.Base(filepath.Clean("/" + name))
	if b == "/" || b == "." || b == "" || b == string(filepath.Separator) {
		return sha
	}
	return b
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
