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

func download(ctx context.Context, p Payload, dir, ua string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	final := filepath.Join(dir, safeName(p.Filename, p.SHA256))
	if ok, _ := fileMatches(final, p); ok {
		return final, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Client", ua)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrCouldNotCheck, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: download HTTP %d", ErrCouldNotCheck, resp.StatusCode)
	}
	tmp, err := os.CreateTemp(dir, ".molt-dl-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, p.Size+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrCouldNotCheck, err)
	}
	if n != p.Size {
		return "", fmt.Errorf("%w: size %d, want %d", ErrBadPayload, n, p.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != p.SHA256 {
		return "", fmt.Errorf("%w: sha256 %s, want %s", ErrBadPayload, got, p.SHA256)
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return "", err
	}
	return final, nil
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
