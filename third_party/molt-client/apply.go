package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Environment variables shared with molt-swap.
const (
	// EnvHealthFile is set on a freshly swapped-in app. The app must call
	// ReportHealthy once it is genuinely up, or molt-swap rolls back.
	EnvHealthFile = "MOLT_HEALTH_FILE"
	// EnvUpdateResult is set on the app molt-swap (re)launches: "updated" or
	// "rolled-back".
	EnvUpdateResult = "MOLT_UPDATE_RESULT"
)

// SwapPackage is the Molt package that distributes molt-swap itself.
const SwapPackage = "molt-swap"

// ApplyOptions describes how molt-swap should install and relaunch the app.
type ApplyOptions struct {
	// InstallDir is the app's install root (kind=zip), e.g. %LOCALAPPDATA%\Blite.
	InstallDir string
	// Exe is the program to relaunch: relative to InstallDir for kind=zip,
	// absolute (e.g. /usr/bin/blite) for kind=deb.
	Exe string
	// Args are passed to the relaunched program.
	Args []string
	// CacheDir holds downloads, the rollback payload and the result file.
	// Default: os.UserCacheDir()/molt/<package>.
	CacheDir string
	// SwapPath uses an already-present molt-swap instead of fetching it.
	SwapPath string
	// HealthTimeout bounds how long the new version has to ReportHealthy.
	// Default 60s.
	HealthTimeout time.Duration
}

// Result is what molt-swap recorded about the last apply.
type Result struct {
	Outcome   string    `json:"outcome"` // updated | rolled-back | failed
	FromBuild int64     `json:"fromBuild"`
	ToBuild   int64     `json:"toBuild"`
	Reason    string    `json:"reason,omitempty"`
	At        time.Time `json:"at"`
}

// CacheDir returns the effective cache dir for opts.
func (c *Client) CacheDir(opts ApplyOptions) (string, error) {
	if opts.CacheDir != "" {
		return opts.CacheDir, nil
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "molt", c.Package), nil
}

// Apply launches molt-swap to install the verified payload at payloadPath and
// relaunch the app. On success the caller MUST exit promptly: molt-swap waits
// for this process to exit before touching the install. Apply never runs on
// its own; the app decides when (never mid-session).
func (c *Client) Apply(ctx context.Context, u *Update, payloadPath string, opts ApplyOptions) error {
	if u == nil {
		return errors.New("molt: nil update")
	}
	if ok, err := fileMatches(payloadPath, u.Payload); !ok {
		return fmt.Errorf("%w: %s no longer matches (%v)", ErrBadPayload, payloadPath, err)
	}
	cache, err := c.CacheDir(opts)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cache, 0o700); err != nil {
		return err
	}

	args := []string{"apply",
		"--pid", itoa(int64(os.Getpid())),
		"--kind", u.Kind,
		"--payload", payloadPath,
		"--exe", opts.Exe,
		"--from-build", itoa(c.Build),
		"--to-build", itoa(u.Build),
		"--status", filepath.Join(cache, "last-update.json"),
		"--log", filepath.Join(cache, "molt-swap.log"),
	}
	if opts.HealthTimeout > 0 {
		args = append(args, "--health-timeout", opts.HealthTimeout.String())
	}
	switch u.Kind {
	case "zip":
		if opts.InstallDir == "" || opts.Exe == "" || filepath.IsAbs(opts.Exe) {
			return errors.New("molt: kind=zip needs InstallDir and an Exe relative to it")
		}
		args = append(args, "--install-dir", opts.InstallDir)
	case "deb":
		if !filepath.IsAbs(opts.Exe) {
			return errors.New("molt: kind=deb needs an absolute Exe")
		}
		args = append(args, "--deb-package", c.Package)
		rb, err := c.rollbackPayload(ctx, cache)
		if err != nil {
			return err
		}
		if rb != "" {
			args = append(args, "--rollback", rb)
		}
	default:
		return fmt.Errorf("molt: cannot apply kind %q", u.Kind)
	}

	swap := opts.SwapPath
	if swap == "" {
		if swap, err = c.fetchSwap(ctx, cache); err != nil {
			return err
		}
	}
	args = append(args, "--")
	args = append(args, opts.Args...)

	cmd := exec.Command(swap, args...)
	cmd.Env = os.Environ()
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("molt: start molt-swap: %w", err)
	}
	return cmd.Process.Release()
}

// rollbackPayload fetches the installed build's own payload so molt-swap can
// reinstall it if the new version fails. "" means none is published.
func (c *Client) rollbackPayload(ctx context.Context, cache string) (string, error) {
	p, err := c.Release(ctx, c.Build)
	if errors.Is(err, ErrNoRelease) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return download(ctx, p, filepath.Join(cache, "rollback"), c.UserAgent())
}

func (c *Client) fetchSwap(ctx context.Context, cache string) (string, error) {
	u := fmt.Sprintf("%s/v1/manifest/%s/stable/%s", c.base(), SwapPackage, c.platform())
	p, err := fetchManifest(ctx, c.http(), u, c.UserAgent(), c.Keys, SwapPackage, "stable", c.platform(), c.now())
	if err != nil {
		return "", fmt.Errorf("molt-swap: %w", err)
	}
	path, err := download(ctx, p, filepath.Join(cache, "swap", itoa(p.Build)), c.UserAgent())
	if err != nil {
		return "", fmt.Errorf("molt-swap: %w", err)
	}
	return path, os.Chmod(path, 0o755)
}

// ReportHealthy tells molt-swap this freshly installed version is up. It is a
// no-op when the app was not launched by molt-swap. Call it once startup has
// genuinely succeeded (UI listening, tray ready).
func ReportHealthy() error {
	f := os.Getenv(EnvHealthFile)
	if f == "" {
		return nil
	}
	return os.WriteFile(f, []byte("ok"), 0o600)
}

// UpdateResult reports how this process was launched by molt-swap:
// "updated", "rolled-back", or "" when not launched by molt-swap.
func UpdateResult() string { return strings.TrimSpace(os.Getenv(EnvUpdateResult)) }

// LastResult reads and clears the result molt-swap recorded for the last
// apply. It returns (nil, nil) when there is none.
func (c *Client) LastResult(opts ApplyOptions) (*Result, error) {
	cache, err := c.CacheDir(opts)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(cache, "last-update.json")
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = os.Remove(path)
	var r Result
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}
