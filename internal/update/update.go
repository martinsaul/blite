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

	"blite/internal/userinstall"
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
	// UpdatedFrom/UpdatedTo: Result is a successful update between these
	// builds (the UI shows the changes in between).
	UpdatedFrom int64 `json:"updatedFrom,omitempty"`
	UpdatedTo   int64 `json:"updatedTo,omitempty"`
	// Downloading: an install is fetching the update, Progress percent done.
	Downloading bool `json:"downloading,omitempty"`
	Progress    int  `json:"progress,omitempty"`
}

// ErrInstalling: Install was called while an install is already running.
var ErrInstalling = errors.New("an update is already being installed")

// Updater checks for and installs updates for this Blite.
type Updater struct {
	c    *molt.Client
	opts molt.ApplyOptions
	log  *slog.Logger

	// OnChange, if set, is told when Info changes during an install.
	OnChange func()

	mu         sync.Mutex
	info       Info
	pending    *molt.Update
	installing bool
	// stopStage cancels a running Stage, so an Install takes the download
	// over (it resumes the partial file).
	stopStage context.CancelFunc
	// seed, if set, prepares the install folder before molt-swap runs.
	seed func() error

	dl sync.Mutex // one download at a time: Stage and Install share the file
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
	u.c.Progress = u.progress
	u.opts, u.info.CanApply = installLayout()
	if runtime.GOOS == "linux" && u.info.CanApply {
		// The per-user copy is its own Molt package: "blite" on Linux is the
		// .deb, for first installs and builds that update through apt.
		u.c.Package = userPackage
		u.seed = func() error { return userinstall.Seed(u.opts.InstallDir, systemExe(), build) }
	}
	// A channel chosen by hand survives the update; otherwise the new build
	// uses its own stamped default.
	if pin {
		u.opts.Args = []string{"-pin-channel", channel}
	}
	return u
}

// installLayout works out where this Blite is installed and how molt-swap
// relaunches it. Only the layouts our releases produce can update themselves:
// the Windows zip (<root>\bin\blite.exe), and on Linux the .deb
// (/usr/bin/blite) and the per-user copy it hands over to.
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
		// The .deb's copy and the per-user one both update the per-user
		// copy (kind=zip, no root needed).
		if exe != userinstall.SystemExe && !userinstall.Inside(exe) {
			return molt.ApplyOptions{}, false
		}
		root, err := userinstall.Root()
		if err != nil {
			return molt.ApplyOptions{}, false
		}
		return molt.ApplyOptions{InstallDir: root, Exe: userinstall.Exe}, true
	}
	return molt.ApplyOptions{}, false
}

// userPackage is the Molt package of the per-user Linux copy.
const userPackage = "blite-home"

// systemExe is the running program (the .deb's or the per-user copy), which
// Seed copies when the per-user folder is still empty.
func systemExe() string {
	exe, _ := os.Executable()
	exe, _ = filepath.EvalSymlinks(exe)
	return exe
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
	env := molt.UpdateResult()
	r, err := u.c.LastResult(u.opts)
	if err == nil && r == nil && env == "updated" {
		// molt-swap records the outcome only once it has our health report:
		// wait for it, or this start misses it and the next one shows it.
		for i := 0; i < 40 && r == nil; i++ {
			time.Sleep(250 * time.Millisecond)
			r, _ = u.c.LastResult(u.opts)
		}
		if r == nil {
			r = &molt.Result{Outcome: "updated", ToBuild: u.c.Build}
		}
	}
	if r != nil {
		u.showResult(r)
		return
	}
	// A message handed on by the instance before us (Windows: the start
	// under the crash guard), not molt-swap's bare outcome.
	if env != "" && env != "updated" && env != "rolled-back" {
		u.setResult(env)
	}
}

// showResult puts the outcome of the last update in Info for the UI.
func (u *Updater) showResult(r *molt.Result) {
	switch r.Outcome {
	case "updated":
		u.setResult(fmt.Sprintf("Updated to build %d.", r.ToBuild))
		u.mu.Lock()
		u.info.UpdatedFrom, u.info.UpdatedTo = r.FromBuild, r.ToBuild
		u.mu.Unlock()
	case "rolled-back":
		u.setResult(fmt.Sprintf("The update to build %d didn't start properly, so Bellion Share went back to build %d.", r.ToBuild, r.FromBuild))
	default:
		u.setResult(fmt.Sprintf("The update to build %d failed: %s", r.ToBuild, r.Reason))
	}
}

func (u *Updater) setResult(s string) {
	u.mu.Lock()
	u.info.Result = s
	u.info.UpdatedFrom, u.info.UpdatedTo = 0, 0
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
// One install runs at a time: another call meanwhile gets ErrInstalling.
// The download has no deadline (a slow link takes as long as it takes);
// a stalled one is resumed.
func (u *Updater) Install(ctx context.Context) error { return u.install(ctx, nil) }

// InstallAtStart is Install for an update right at start-up: molt-swap
// relaunches the new build with args too (the link, -background), so it
// carries on the start the user asked for.
func (u *Updater) InstallAtStart(ctx context.Context, args []string) error {
	return u.install(ctx, args)
}

func (u *Updater) install(ctx context.Context, args []string) error {
	u.mu.Lock()
	up, canApply := u.pending, u.info.CanApply
	if up == nil {
		u.mu.Unlock()
		return errors.New("no update available")
	}
	if !canApply {
		u.mu.Unlock()
		return errors.New("this copy of Bellion Share can't update itself; install the release package instead")
	}
	if u.installing {
		u.mu.Unlock()
		return ErrInstalling
	}
	u.installing = true
	u.info.Downloading, u.info.Progress, u.info.Problem = true, 0, ""
	if u.stopStage != nil {
		u.stopStage()
	}
	u.mu.Unlock()
	u.dl.Lock()
	defer u.dl.Unlock()
	u.changed()
	defer func() {
		u.mu.Lock()
		u.installing, u.info.Downloading = false, false
		u.mu.Unlock()
		u.changed()
	}()
	cache, err := u.c.CacheDir(u.opts)
	if err != nil {
		return err
	}
	opts := u.opts
	opts.Args = append(append([]string(nil), u.opts.Args...), args...)
	err = retry(ctx, u.log, func() error {
		path, err := u.c.Download(ctx, up.Payload, cache)
		if err == nil && u.seed != nil {
			err = u.seed()
		}
		if err == nil {
			pruneStaged(cache, path, u.log)
			err = u.c.Apply(ctx, up, path, opts)
		}
		return err
	})
	if err != nil && ctx.Err() != nil {
		return err // given up (Skip at start-up): not a problem to show
	}
	if err != nil {
		u.mu.Lock()
		u.info.Problem = "Installing the update failed: " + err.Error()
		u.mu.Unlock()
		return err
	}
	return nil
}

// Stage downloads the pending update into the Molt cache without
// installing it, so the next start (or an Install) finds it on disk. It
// shows no progress and gives way to an Install. A newer update replaces
// what an earlier Stage left. Errors are only logged: the next check
// tries again, and Install resumes a partial download.
func (u *Updater) Stage(ctx context.Context) {
	u.mu.Lock()
	up := u.pending
	if up == nil || !u.info.CanApply || u.installing || u.stopStage != nil {
		u.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	u.stopStage = cancel
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		u.stopStage = nil
		u.mu.Unlock()
	}()

	u.dl.Lock()
	defer u.dl.Unlock()
	if ctx.Err() != nil {
		return
	}
	cache, err := u.c.CacheDir(u.opts)
	if err != nil {
		u.log.Info("update: pre-download", "err", err)
		return
	}
	quiet := *u.c
	quiet.Progress = nil
	path, err := quiet.Download(ctx, up.Payload, cache)
	if err != nil {
		u.log.Info("update: pre-download", "build", up.Build, "err", err)
		return
	}
	pruneStaged(cache, path, u.log)
	u.log.Info("update: pre-downloaded", "build", up.Build, "file", path)
}

// pruneStaged removes the update payloads in cache other than keep, and
// their partial downloads: a build downloaded but never installed, which a
// newer one has replaced. molt-swap's own files (rollback, swap, status,
// log) stay.
func pruneStaged(cache, keep string, log *slog.Logger) {
	ents, err := os.ReadDir(cache)
	if err != nil {
		return
	}
	for _, e := range ents {
		n := e.Name()
		if !e.Type().IsRegular() || filepath.Join(cache, n) == keep {
			continue
		}
		ext := strings.ToLower(filepath.Ext(n))
		if ext == ".zip" || ext == ".deb" || ext == ".part" { // a .part left now is another build's
			if err := os.Remove(filepath.Join(cache, n)); err == nil {
				log.Info("update: removed an older download", "file", n)
			}
		}
	}
}

// progress notes how much of the update is downloaded (whole percents).
func (u *Updater) progress(done, total int64) {
	if total <= 0 {
		return
	}
	pct := int(done * 100 / total)
	u.mu.Lock()
	same := u.info.Progress == pct
	u.info.Progress = pct
	u.mu.Unlock()
	if !same {
		u.changed()
	}
}

func (u *Updater) changed() {
	if u.OnChange != nil {
		u.OnChange()
	}
}

// retryWaits are the pauses before the second and later install attempts.
var retryWaits = []time.Duration{2 * time.Second, 5 * time.Second}

// retry runs f again when it fails on the network or with a damaged
// download: a cold download through the CDN can be cut off part way (seen
// on 0.1.0 (67)), and the next attempt usually gets it whole. Both happen
// before molt-swap starts, so trying again is safe.
func retry(ctx context.Context, log *slog.Logger, f func() error) error {
	err := f()
	for _, wait := range retryWaits {
		if err == nil || !(errors.Is(err, molt.ErrCouldNotCheck) || errors.Is(err, molt.ErrBadPayload)) {
			return err
		}
		log.Warn("update: install attempt failed, trying again", "err", err, "in", wait)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}
		err = f()
	}
	return err
}
