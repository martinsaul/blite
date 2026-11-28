package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"blite/internal/proto"
	"blite/internal/room"
	"blite/internal/secret"
	"blite/internal/session"
	"blite/internal/sigclient"
)

// Recent rooms: the last 5 rooms created or joined, newest first, stored in
// recent.json next to settings.json and servers.json with 0600 permissions
// because entries include room passwords.

const maxRecentRooms = 5

// recentGoneHold is how long a missing room stays in "gone" state (for the UI's
// red fade-out) before being removed from memory. Overridden in tests.
var recentGoneHold = 850 * time.Millisecond

// recentPath is overridden by tests.
var recentPath = func() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "blite", "recent.json"), nil
}

type recentDiskEntry struct {
	Server    string `json:"server"`
	Code      string `json:"code"`
	Password  string `json:"password,omitempty"`
	Encrypted bool   `json:"encrypted,omitempty"`
}

type recentEntry struct {
	server    string
	code      string
	password  string
	encrypted bool
	status    string // "checking", "ready", "gone"
}

// RecentRoom is a recent room entry exposed to the UI (without its password).
type RecentRoom struct {
	Code        string `json:"code"`
	Display     string `json:"display"`
	Server      string `json:"server"`
	Host        string `json:"host"`
	BuiltIn     bool   `json:"builtIn,omitempty"`
	HasPassword bool   `json:"hasPassword,omitempty"`
	Status      string `json:"status"` // "checking", "ready", "gone"
}

func normalizeRecentCode(c string) string {
	if n := room.NormalizeCode(c); n != "" {
		return n
	}
	return strings.TrimSpace(c)
}

func sameRecentServer(a, b string) bool {
	return a == b || session.SameServer(a, b)
}

func loadRecent(defaultServer string) []recentEntry {
	f, err := recentPath()
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(f)
	if err != nil {
		return nil
	}
	var disk []recentDiskEntry
	if json.Unmarshal(b, &disk) != nil {
		return nil
	}
	var out []recentEntry
	for _, d := range disk {
		code := normalizeRecentCode(d.Code)
		if code == "" {
			continue
		}
		srv := strings.TrimSpace(d.Server)
		if srv == "" {
			srv = defaultServer
		} else if n, err := session.NormalizeServer(srv); err == nil {
			srv = n
		} else {
			continue
		}
		if slices.ContainsFunc(out, func(e recentEntry) bool {
			return e.code == code && sameRecentServer(e.server, srv)
		}) {
			continue
		}
		pw := d.Password
		if pw != "" {
			plain, err := secret.Open(pw)
			if err != nil {
				pw = ""
			} else {
				pw = string(plain)
				clear(plain)
			}
		}
		out = append(out, recentEntry{
			server:    srv,
			code:      code,
			password:  pw,
			encrypted: d.Encrypted,
			status:    "checking",
		})
		if len(out) >= maxRecentRooms {
			break
		}
	}
	return out
}

func saveRecentEntries(entries []recentEntry) error {
	f, err := recentPath()
	if err != nil {
		return err
	}
	disk := make([]recentDiskEntry, 0, len(entries))
	for _, e := range entries {
		if e.status == "gone" {
			continue
		}
		pw := e.password
		if pw != "" && secret.Enabled {
			s, err := secret.Seal([]byte(pw))
			if err != nil {
				return err
			}
			pw = s
		}
		disk = append(disk, recentDiskEntry{
			Server:    e.server,
			Code:      e.code,
			Password:  pw,
			Encrypted: e.encrypted,
		})
	}
	if len(disk) == 0 {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(disk, "", "  ")
	if err := os.WriteFile(f, b, 0o600); err != nil {
		return err
	}
	_ = os.Chmod(f, 0o600)
	return nil
}

func (a *App) saveRecentLocked() {
	if err := saveRecentEntries(a.recent); err != nil {
		a.log.Warn("save recent rooms", "err", err)
	}
}

func (a *App) recordRecent(server, code, password string, encrypted bool) {
	code = normalizeRecentCode(code)
	if code == "" {
		return
	}
	a.mu.Lock()
	if server == "" {
		server = a.cfg.Server
	} else if n, err := session.NormalizeServer(server); err == nil {
		server = n
	}
	a.recent = slices.DeleteFunc(a.recent, func(e recentEntry) bool {
		return e.code == code && sameRecentServer(e.server, server)
	})
	a.recent = slices.Insert(a.recent, 0, recentEntry{
		server:    server,
		code:      code,
		password:  password,
		encrypted: encrypted,
		status:    "checking",
	})
	if len(a.recent) > maxRecentRooms {
		a.recent = a.recent[:maxRecentRooms]
	}
	a.saveRecentLocked()
	a.mu.Unlock()
}

func (a *App) recentStateLocked() []RecentRoom {
	if len(a.recent) == 0 {
		return nil
	}
	out := make([]RecentRoom, 0, len(a.recent))
	for _, e := range a.recent {
		st := e.status
		if st == "" {
			st = "checking"
		}
		out = append(out, RecentRoom{
			Code:        e.code,
			Display:     session.DisplayCode(e.code),
			Server:      e.server,
			Host:        session.ServerHost(e.server),
			BuiltIn:     isBuiltIn(e.server, a.cfg.Server),
			HasPassword: e.password != "",
			Status:      st,
		})
	}
	return out
}

// ProbeRecentRooms checks every recent room in the background when Home is
// shown: entries start "checking" (grey), turn "ready" (white) when the room
// exists or the server doesn't support probe, or turn "gone" (red, fading out)
// and are removed when the room no longer exists.
func (a *App) ProbeRecentRooms() {
	a.mu.Lock()
	a.recentGen++
	gen := a.recentGen
	if len(a.recent) == 0 {
		a.mu.Unlock()
		return
	}
	for i := range a.recent {
		a.recent[i].status = "checking"
	}
	snap := slices.Clone(a.recent)
	a.mu.Unlock()
	a.bc.Notify()

	for _, entry := range snap {
		go a.probeOneRecent(gen, entry)
	}
}

func (a *App) probeOneRecent(gen uint64, entry recentEntry) {
	if entry.server == "" {
		a.setRecentStatus(gen, entry.server, entry.code, "ready")
		return
	}
	st, err := sigclient.Probe(context.Background(), entry.server, entry.code, entry.password, sigclient.Options{})
	if err == nil && st == sigclient.ProbeGone {
		a.markRecentGone(gen, entry.server, entry.code)
		return
	}
	// ProbeExists, ProbeUnsupported, or transient transport error: show as
	// clickable so joining can either connect or display the error.
	a.setRecentStatus(gen, entry.server, entry.code, "ready")
}

func (a *App) setRecentStatus(gen uint64, server, code, status string) {
	a.mu.Lock()
	if gen != 0 && a.recentGen != gen {
		a.mu.Unlock()
		return
	}
	changed := false
	for i := range a.recent {
		if a.recent[i].code == code && sameRecentServer(a.recent[i].server, server) {
			if a.recent[i].status != status {
				a.recent[i].status = status
				changed = true
			}
			break
		}
	}
	a.mu.Unlock()
	if changed {
		a.bc.Notify()
	}
}

func (a *App) markRecentGone(gen uint64, server, code string) {
	a.mu.Lock()
	if gen != 0 && a.recentGen != gen {
		a.mu.Unlock()
		return
	}
	found := false
	for i := range a.recent {
		if a.recent[i].code == code && sameRecentServer(a.recent[i].server, server) {
			a.recent[i].status = "gone"
			found = true
			break
		}
	}
	if !found {
		a.mu.Unlock()
		return
	}
	a.saveRecentLocked()
	hold := recentGoneHold
	useGen := a.recentGen
	a.mu.Unlock()
	a.bc.Notify()

	go func() {
		if hold > 0 {
			time.Sleep(hold)
		}
		a.mu.Lock()
		if a.recentGen != useGen {
			a.mu.Unlock()
			return
		}
		before := len(a.recent)
		a.recent = slices.DeleteFunc(a.recent, func(e recentEntry) bool {
			return e.status == "gone" && e.code == code && sameRecentServer(e.server, server)
		})
		removed := len(a.recent) != before
		if removed {
			a.saveRecentLocked()
		}
		a.mu.Unlock()
		if removed {
			a.bc.Notify()
		}
	}()
}

// RemoveRecentRoom removes a recent room by hand.
func (a *App) RemoveRecentRoom(server, code string) error {
	code = normalizeRecentCode(code)
	a.mu.Lock()
	before := len(a.recent)
	a.recent = slices.DeleteFunc(a.recent, func(e recentEntry) bool {
		return e.code == code && sameRecentServer(e.server, server)
	})
	if len(a.recent) != before {
		a.saveRecentLocked()
	}
	a.mu.Unlock()
	a.bc.Notify()
	return nil
}

// JoinRecentRoom joins a recent room using its stored server and password.
func (a *App) JoinRecentRoom(ctx context.Context, server, code string) error {
	code = normalizeRecentCode(code)
	a.mu.Lock()
	var entry recentEntry
	found := false
	for _, e := range a.recent {
		if e.status != "gone" && e.code == code && sameRecentServer(e.server, server) {
			entry = e
			found = true
			break
		}
	}
	a.mu.Unlock()
	if !found {
		return errors.New(serverMessages[proto.ErrNoRoom])
	}
	if a.busy() {
		return ErrInSession
	}
	srv := a.linkServer(entry.server)
	if err := a.confirmServer(srv); err != nil {
		return err
	}
	err := a.joinRaw(ctx, srv, entry.code, entry.password, entry.encrypted, a.DefaultSource)
	if err != nil {
		if isProtoErr(err, proto.ErrNoRoom) || isProtoErr(err, proto.ErrBadPassword) {
			a.markRecentGone(0, entry.server, entry.code)
		}
		return friendly(err)
	}
	return nil
}
