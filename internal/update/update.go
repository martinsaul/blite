// Package update keeps Blite current through a Molt update server. It
// checks a signed channel manifest, downloads and verifies the new build,
// and hands it to molt-swap, which replaces the install, relaunches Blite
// and rolls back unless the new build reports healthy. Nothing here runs on
// its own: the app decides when to check and when to install, and never
// installs during a call.
package update

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	molt "moltclient"
)

// Stamped by the build (scripts/version.sh, from BLITE_UPDATE_URL and
// BLITE_UPDATE_KEYS): the update server and the release signing keys it
// accepts (std base64 Ed25519, comma-separated). Builds without both don't
// update themselves. Keep old keys until every client trusts the new one.
var (
	serverURL   = ""
	trustedKeys = ""
)

// Info is what the UI shows about updates.
type Info struct {
	Channel   string `json:"channel"`
	Checking  bool   `json:"checking,omitempty"`
	Checked   string `json:"checked,omitempty"`   // UTC time of the last successful check
	Available string `json:"available,omitempty"` // e.g. "0.1.1 (23)"
	Notes     string `json:"notes,omitempty"`
	Mandatory bool   `json:"mandatory,omitempty"` // below the channel's minimum: no sessions until updated
	CanApply  bool   `json:"canApply"`            // this install can update itself
	Problem   string `json:"problem,omitempty"`   // tampering or a failed install; never "offline"
	Result    string `json:"result,omitempty"`    // outcome of the last update, shown once
}

// Updater checks for and installs updates for this Blite.
type Updater struct {
	c    *molt.Client
	opts molt.ApplyOptions
	log  *slog.Logger

	mu      sync.Mutex
	info    Info
	pending *molt.Update
}

// New returns an Updater for the running build, or nil when this build can't
// take part (local dev builds have build 0). channel is "stable" or "beta";
// pin keeps it across self-updates (only for a channel chosen by hand).
func New(version string, build int64, channel string, pin bool, log *slog.Logger) *Updater {
	var keys []ed25519.PublicKey
	for _, k := range strings.Split(trustedKeys, ",") {
		if pk, err := molt.ParsePublicKey(strings.TrimSpace(k)); err == nil {
			keys = append(keys, pk)
		}
	}
	if build <= 0 || serverURL == "" || len(keys) == 0 {
		return nil
	}
	u := &Updater{
		c:   &molt.Client{BaseURL: serverURL, Package: "blite", Channel: channel, Version: version, Build: build, Keys: keys},
		log: log,
	}
	u.info.Channel = channel
	u.opts, u.info.CanApply = installLayout()
	// A channel chosen by hand survives the update; otherwise the new build
	// uses its own stamped default.
	if pin {
		u.opts.Args = []string{"-pin-channel", channel}
	}
	return u
}

// installLayout works out where this Blite is installed and how molt-swap
// relaunches it. Only the layouts our releases produce can update themselves:
// the Windows zip (<root>\bin\blite.exe) and the .deb (/usr/bin/blite).
func installLayout() (molt.ApplyOptions, bool) {
	exe, err := os.Executable()
	if err != nil {
		return molt.ApplyOptions{}, false
	}
	exe, _ = filepath.EvalSymlinks(exe)
	switch runtime.GOOS {
	case "windows":
		bin := filepath.Dir(exe)
		if !strings.EqualFold(filepath.Base(bin), "bin") {
			return molt.ApplyOptions{}, false
		}
		root := filepath.Dir(bin)
		rel := filepath.Join("bin", filepath.Base(exe))
		// Relaunch through the crash guard when the install has one.
		if _, err := os.Stat(filepath.Join(root, "blite-guard.exe")); err == nil {
			rel = "blite-guard.exe"
		}
		return molt.ApplyOptions{InstallDir: root, Exe: rel}, true
	case "linux":
		if exe != "/usr/bin/blite" {
			return molt.ApplyOptions{}, false
		}
		return molt.ApplyOptions{Exe: exe}, true
	}
	return molt.ApplyOptions{}, false
}

// Info returns the current update state.
func (u *Updater) Info() Info {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.info
}

// Mandatory reports whether the installed build is below the channel minimum.
func (u *Updater) Mandatory() bool {
	if u == nil {
		return false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.info.Mandatory
}

// Started is called once Blite is genuinely up (local UI listening, tray
// ready): it confirms a fresh install to molt-swap and picks up the outcome
// of the last update for the UI.
func (u *Updater) Started() {
	if err := molt.ReportHealthy(); err != nil {
		u.log.Warn("update health report", "err", err)
	}
	r, err := u.c.LastResult(u.opts)
	if err != nil || r == nil {
		if res := molt.UpdateResult(); res != "" {
			u.setResult(res)
		}
		return
	}
	switch r.Outcome {
	case "updated":
		u.setResult(fmt.Sprintf("Updated to build %d.", r.ToBuild))
	case "rolled-back":
		u.setResult(fmt.Sprintf("The update to build %d didn't start properly, so Blite went back to build %d.", r.ToBuild, r.FromBuild))
	default:
		u.setResult(fmt.Sprintf("The update to build %d failed: %s", r.ToBuild, r.Reason))
	}
}

func (u *Updater) setResult(s string) {
	u.mu.Lock()
	u.info.Result = s
	u.mu.Unlock()
}

// ClearResult drops the one-time update outcome once the UI has shown it.
func (u *Updater) ClearResult() { u.setResult("") }

// Check asks Molt for a newer build. Being offline is not an error the user
// sees; a manifest that fails verification is.
func (u *Updater) Check(ctx context.Context) {
	u.mu.Lock()
	if u.info.Checking {
		u.mu.Unlock()
		return
	}
	u.info.Checking = true
	u.mu.Unlock()

	up, err := u.c.Check(ctx)

	u.mu.Lock()
	defer u.mu.Unlock()
	u.info.Checking = false
	switch {
	case err == nil, errors.Is(err, molt.ErrNoRelease):
		u.pending = up
		u.info.Checked = time.Now().UTC().Format("2006-01-02 15:04")
		u.info.Available, u.info.Notes, u.info.Mandatory = "", "", false
		if up != nil {
			u.info.Available = fmt.Sprintf("%s (%d)", up.Version, up.Build)
			u.info.Notes, u.info.Mandatory = up.Notes, up.Mandatory
		}
		if u.info.Problem != "" && !strings.HasPrefix(u.info.Problem, "Install") {
			u.info.Problem = ""
		}
	case errors.Is(err, molt.ErrBadSignature):
		u.log.Error("update manifest failed verification", "err", err)
		u.info.Problem = "The update server sent an update that failed its signature check, so it was ignored."
	default: // ErrCouldNotCheck and anything else: stay quiet
		u.log.Info("update check", "err", err)
	}
}

// Install downloads and verifies the pending update and starts molt-swap.
// On success the caller must exit Blite promptly so the swap can proceed.
func (u *Updater) Install(ctx context.Context) error {
	u.mu.Lock()
	up, canApply := u.pending, u.info.CanApply
	u.mu.Unlock()
	if up == nil {
		return errors.New("no update available")
	}
	if !canApply {
		return errors.New("this copy of Blite can't update itself; install the release package instead")
	}
	cache, err := u.c.CacheDir(u.opts)
	if err != nil {
		return err
	}
	path, err := u.c.Download(ctx, up.Payload, cache)
	if err == nil {
		err = u.c.Apply(ctx, up, path, u.opts)
	}
	if err != nil {
		u.mu.Lock()
		u.info.Problem = "Installing the update failed: " + err.Error()
		u.mu.Unlock()
		return err
	}
	return nil
}
