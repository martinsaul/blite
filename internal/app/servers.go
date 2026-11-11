package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"blite/internal/session"
)

// Signaling servers. The built-in default (the build's server, or -server /
// BLITE_SERVER) is always there. The user may add their own, make one of
// them the default for new sessions, and pick another for the next
// session in the lobby. Links name their server, so joining needs no
// setting; a server that is neither built in nor added is confirmed once.

// serverSettings is servers.json in the config folder.
type serverSettings struct {
	// Custom are the user's own servers (wss://host/ws).
	Custom []string `json:"custom,omitempty"`
	// Default is the server new sessions start on; "" = the built-in one.
	Default string `json:"default,omitempty"`
	// Trusted are hosts of other servers the user agreed to join once.
	Trusted []string `json:"trusted,omitempty"`
}

// serversPath is overridden by tests.
var serversPath = func() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "blite", "servers.json"), nil
}

func loadServers() serverSettings {
	var s serverSettings
	f, err := serversPath()
	if err != nil {
		return s
	}
	b, err := os.ReadFile(f)
	if err != nil || json.Unmarshal(b, &s) != nil {
		return serverSettings{}
	}
	// Drop anything that isn't a server anymore (edited by hand).
	s.Custom = slices.DeleteFunc(s.Custom, func(c string) bool {
		n, err := session.NormalizeServer(c)
		return err != nil || n != c
	})
	if s.Default != "" && !slices.Contains(s.Custom, s.Default) {
		s.Default = ""
	}
	return s
}

func saveServers(s serverSettings) error {
	f, err := serversPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	return os.WriteFile(f, b, 0o600)
}

// Servers is the server list for the UI.
type Servers struct {
	List []Server `json:"list"`
	// Default is where new sessions start; Selected is where the next one
	// starts (the lobby's pick).
	Default  string `json:"default"`
	Selected string `json:"selected"`
}

// Server is one signaling server.
type Server struct {
	URL     string `json:"url"`
	Host    string `json:"host"`
	BuiltIn bool   `json:"builtIn,omitempty"`
}

// Custom reports whether the list has servers besides the built-in one.
func (s *Servers) Custom() bool { return s != nil && len(s.List) > 1 }

// SelectedBuiltIn reports whether the server picked for new sessions is the
// built-in one.
func (s *Servers) SelectedBuiltIn() bool {
	return s == nil || len(s.List) == 0 || s.Selected == s.List[0].URL
}

// builtIn is the built-in default server.
func (a *App) builtIn() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.Server
}

// serverState builds Servers; a.mu is held.
func (a *App) serverState() *Servers {
	st := &Servers{List: []Server{{URL: a.cfg.Server, Host: session.ServerHost(a.cfg.Server), BuiltIn: true}}}
	for _, c := range a.servers.Custom {
		st.List = append(st.List, Server{URL: c, Host: session.ServerHost(c)})
	}
	st.Default, st.Selected = a.defaultServerLocked(), a.selectedLocked()
	return st
}

func (a *App) defaultServerLocked() string {
	if a.servers.Default != "" {
		return a.servers.Default
	}
	return a.cfg.Server
}

// selectedLocked is the server new sessions are created on.
func (a *App) selectedLocked() string {
	if a.picked != "" && (a.picked == a.cfg.Server || slices.Contains(a.servers.Custom, a.picked)) {
		return a.picked
	}
	return a.defaultServerLocked()
}

// AddServer adds a server of the user's (a host or a wss:// URL).
func (a *App) AddServer(s string) error {
	n, err := session.NormalizeServer(s)
	if err != nil {
		return err
	}
	a.mu.Lock()
	switch {
	case session.SameServer(n, a.cfg.Server):
		a.mu.Unlock()
		return errors.New("that's the built-in server: it's always in the list")
	case slices.ContainsFunc(a.servers.Custom, func(c string) bool { return session.SameServer(c, n) }):
		a.mu.Unlock()
		return fmt.Errorf("%s is already in the list", session.ServerHost(n))
	}
	a.servers.Custom = append(a.servers.Custom, n)
	err = a.saveServersLocked()
	a.mu.Unlock()
	a.bc.Notify()
	return err
}

// RemoveServer removes one of the user's servers; if it was the default,
// the built-in one is the default again.
func (a *App) RemoveServer(url string) error {
	a.mu.Lock()
	i := slices.Index(a.servers.Custom, url)
	if i < 0 {
		a.mu.Unlock()
		return errors.New("the built-in server can't be removed")
	}
	a.servers.Custom = slices.Delete(a.servers.Custom, i, i+1)
	if a.servers.Default == url {
		a.servers.Default = ""
	}
	err := a.saveServersLocked()
	a.mu.Unlock()
	a.bc.Notify()
	return err
}

// SetDefaultServer makes url the server new sessions start on.
func (a *App) SetDefaultServer(url string) error {
	a.mu.Lock()
	switch {
	case url == a.cfg.Server:
		a.servers.Default = ""
	case slices.Contains(a.servers.Custom, url):
		a.servers.Default = url
	default:
		a.mu.Unlock()
		return errors.New("unknown server")
	}
	a.picked = "" // the lobby follows the new default
	err := a.saveServersLocked()
	a.mu.Unlock()
	a.bc.Notify()
	return err
}

// SelectServer picks the server the next session is created on (not kept
// across restarts: those start on the default).
func (a *App) SelectServer(url string) error {
	a.mu.Lock()
	if url != a.cfg.Server && !slices.Contains(a.servers.Custom, url) {
		a.mu.Unlock()
		return errors.New("unknown server")
	}
	a.picked = url
	a.mu.Unlock()
	a.bc.Notify()
	return nil
}

func (a *App) saveServersLocked() error {
	if err := saveServers(a.servers); err != nil {
		a.log.Warn("save servers", "err", err)
		return errors.New("could not save the server list: " + err.Error())
	}
	return nil
}

// ErrServerNotConfirmed: the user didn't agree to join on another server.
var ErrServerNotConfirmed = errors.New("not joined: that session is on a server not run by Bellion Share")

// linkServer is the server a link's room is on: "" and the built-in
// server's host mean the built-in server.
func (a *App) linkServer(server string) string {
	def := a.builtIn()
	if server == "" || isBuiltIn(server, def) {
		return def
	}
	return server
}

// BuiltInAliases are other host names of the built-in server (set at build
// time), e.g. names it had before; links on them join the built-in server.
var BuiltInAliases []string

// isBuiltIn reports whether server is the built-in one, def: the same host,
// that host with www. in front, or one of BuiltInAliases.
func isBuiltIn(server, def string) bool {
	h := session.ServerHost(server)
	return session.SameServer(server, def) || h == "www."+session.ServerHost(def) || slices.Contains(BuiltInAliases, h)
}

// needsConfirm reports whether joining on server needs the user's OK
// first: it is neither the built-in server, nor one of the user's, nor a
// host they agreed to before.
func (a *App) needsConfirm(server string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if isBuiltIn(server, a.cfg.Server) {
		return false
	}
	h := session.ServerHost(server)
	same := func(c string) bool { return session.ServerHost(c) == h }
	return !slices.ContainsFunc(a.servers.Custom, same) && !slices.Contains(a.servers.Trusted, h)
}

// confirmServer asks the user once before joining on an unknown server
// and remembers a yes.
func (a *App) confirmServer(server string) error {
	if !a.needsConfirm(server) {
		return nil
	}
	h := session.ServerHost(server)
	if a.ConfirmServer == nil || !a.ConfirmServer(h) {
		return ErrServerNotConfirmed
	}
	a.mu.Lock()
	if !slices.Contains(a.servers.Trusted, h) {
		a.servers.Trusted = append(a.servers.Trusted, h)
	}
	a.saveServersLocked()
	a.mu.Unlock()
	return nil
}
