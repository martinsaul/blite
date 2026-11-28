package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"blite/internal/audio"
	"blite/internal/media"
	"blite/internal/session"
)

// Crash-report preferences (CrashReports, SetCrashReports).
const (
	CrashReportsAsk    = "ask"
	CrashReportsAlways = "always"
	CrashReportsNever  = "never"
)

type appSettings struct {
	AutoWatch  *bool `json:"autoWatch,omitempty"`
	PlaySounds *bool `json:"playSounds,omitempty"`
	// VoiceInSystem: whole-system share audio includes voice chat apps.
	VoiceInSystem bool `json:"voiceInSystem,omitempty"`
	// HideChangelog: don't show what changed after an update.
	HideChangelog bool   `json:"hideChangelog,omitempty"`
	PortMap       *bool  `json:"portMap,omitempty"`
	Resolution    string `json:"resolution,omitempty"`
	FPS           *int   `json:"fps,omitempty"`
	SlowViewer    string `json:"slowViewer,omitempty"`
	MinHeight     *int   `json:"minHeight,omitempty"`
	SmallerCopy   *bool  `json:"smallerCopy,omitempty"`
	KeepQuality   bool   `json:"keepQuality,omitempty"`
	UploadKbps    *int   `json:"uploadKbps,omitempty"`
	E2EE          *bool  `json:"e2ee,omitempty"`
	// UIScale is the window's interface size in percent ("" = follow the
	// desktop). Read once at startup (uiscale.Apply), so it applies on restart.
	UIScale string `json:"uiScale,omitempty"`
	// SelfView is how our own preview shows in a call (session.SelfView*).
	SelfView string `json:"selfView,omitempty"`
	// Presets are the user's own quality presets (BuiltinPresets aren't kept).
	Presets []QualityPreset `json:"presets,omitempty"`
	// AnonymizeLogs: reports (Send logs, crash reports) leave out names,
	// room codes, servers, window titles and this computer's names.
	// nil = never chose (default true).
	AnonymizeLogs *bool `json:"anonymizeLogs,omitempty"`
	// CrashReports: when crash logs from earlier runs are sent
	// (CrashReportsAsk, CrashReportsAlways, CrashReportsNever; "" = ask).
	CrashReports string `json:"crashReports,omitempty"`
	// DiscordLinked: this install linked a Discord account on the server,
	// so RunDiscord opens the follow socket at start.
	DiscordLinked bool `json:"discordLinked,omitempty"`
}

func settingsFile() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "blite", "settings.json"), nil
}

// settingsMu serializes reading and writing settings.json: each save reads
// the file, changes one setting and writes it all back.
var settingsMu sync.Mutex

func loadSettings() appSettings {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	cleanTempOnce.Do(removeStaleTemp)
	return readSettings()
}

var cleanTempOnce sync.Once

// removeStaleTemp removes temp files a crash left mid-save (a minute old:
// not one another Blite is writing right now).
func removeStaleTemp() {
	f, err := settingsFile()
	if err != nil {
		return
	}
	old, _ := filepath.Glob(filepath.Join(filepath.Dir(f), "settings-*.tmp"))
	for _, p := range old {
		if fi, err := os.Stat(p); err == nil && time.Since(fi.ModTime()) > time.Minute {
			os.Remove(p)
		}
	}
}

func readSettings() appSettings {
	f, err := settingsFile()
	if err != nil {
		return appSettings{}
	}
	b, err := os.ReadFile(f)
	if err != nil {
		return appSettings{}
	}
	var st appSettings
	if json.Unmarshal(b, &st) != nil {
		return appSettings{}
	}
	return st
}

func loadAutoWatch() bool {
	st := loadSettings()
	return st.AutoWatch == nil || *st.AutoWatch
}

func loadPlaySounds() bool {
	st := loadSettings()
	return st.PlaySounds == nil || *st.PlaySounds
}

func anonymizeLogs(st appSettings) bool {
	return st.AnonymizeLogs == nil || *st.AnonymizeLogs
}

func normalizeCrashReports(v string) string {
	switch v {
	case CrashReportsAlways, CrashReportsNever:
		return v
	default:
		return CrashReportsAsk
	}
}

// SaveAnonymizeLogs keeps the anonymize setting on disk.
func SaveAnonymizeLogs(anon bool) error {
	return updateSettings(func(st *appSettings) {
		st.AnonymizeLogs = &anon
	})
}

// CrashReports returns when crash logs from earlier runs are sent ("ask",
// "always" or "never").
func CrashReports() string { return normalizeCrashReports(loadSettings().CrashReports) }

// SaveCrashReports keeps the crash-report setting on disk.
func SaveCrashReports(mode string) error {
	mode = normalizeCrashReports(mode)
	return updateSettings(func(st *appSettings) {
		st.CrashReports = mode
	})
}

// LoadPortMap reports whether "Open a port on my router automatically" is
// enabled on disk (default true).
func LoadPortMap() bool {
	st := loadSettings()
	return st.PortMap == nil || *st.PortMap
}

func loadShareSettings(flagUploadKbps int) (res media.Resolution, opts session.ShareOptions, e2ee bool) {
	st := loadSettings()
	if media.ValidResolution(media.Resolution(st.Resolution)) {
		res = media.Resolution(st.Resolution)
	}
	opts = session.DefaultShareOptions()
	if st.FPS != nil {
		opts.FPS = *st.FPS
	}
	opts.SlowViewer = st.SlowViewer
	if st.MinHeight != nil {
		opts.MinHeight = *st.MinHeight
	}
	if st.SmallerCopy != nil {
		opts.SmallerCopy = *st.SmallerCopy
	}
	opts.KeepQuality = st.KeepQuality
	if flagUploadKbps > 0 {
		opts.UploadKbps = flagUploadKbps
	} else if st.UploadKbps != nil && *st.UploadKbps > 0 {
		opts.UploadKbps = *st.UploadKbps
	}
	opts = session.NormalizeShareOptions(opts)
	e2ee = st.E2EE == nil || *st.E2EE
	return res, opts, e2ee
}

// updateSettings changes settings.json with change, under settingsMu.
func updateSettings(change func(*appSettings)) error {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	st := readSettings()
	change(&st)
	return writeSettings(st)
}

// writeSettings replaces settings.json atomically (a temp file renamed over
// it; os.Rename replaces an existing file on Windows too), so a reader never
// sees it empty or half written.
func writeSettings(st appSettings) error {
	f, err := settingsFile()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(st)
	tmp, err := os.CreateTemp(filepath.Dir(f), "settings-*.tmp")
	if err != nil {
		return err
	}
	_, err = tmp.Write(b)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o600)
	}
	if err == nil {
		err = renameRetry(tmp.Name(), f)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

func saveSelfView(v string) error {
	return updateSettings(func(st *appSettings) {
		st.SelfView = v
	})
}

// keepSelfView is the sessions' Config.OnSelfView: the user changed how
// their preview shows, so the next session starts the same way.
func (a *App) keepSelfView(v string) {
	if err := saveSelfView(v); err != nil {
		a.log.Warn("save settings", "err", err)
	}
	a.mu.Lock()
	a.cfg.SelfView = v
	a.mu.Unlock()
}

func saveAutoWatch(on bool) error {
	return updateSettings(func(st *appSettings) {
		st.AutoWatch = &on
	})
}

// LoadVoiceInSystem reports "Include voice chat apps in whole-system audio"
// (default off).
func LoadVoiceInSystem() bool { return loadSettings().VoiceInSystem }

func saveVoiceInSystem(on bool) error {
	return updateSettings(func(st *appSettings) {
		st.VoiceInSystem = on
	})
}

func saveHideChangelog(on bool) error {
	return updateSettings(func(st *appSettings) {
		st.HideChangelog = on
	})
}

func discordLinked() bool { return loadSettings().DiscordLinked }

func (a *App) setDiscordLinked(on bool) {
	if discordLinked() == on {
		return
	}
	if err := updateSettings(func(st *appSettings) { st.DiscordLinked = on }); err != nil {
		a.log.Warn("save settings", "err", err)
	}
}

func savePlaySounds(on bool) error {
	return updateSettings(func(st *appSettings) {
		st.PlaySounds = &on
	})
}

func saveResolution(res string) error {
	return updateSettings(func(st *appSettings) {
		st.Resolution = res
	})
}

func saveShareOptions(o session.ShareOptions, uploadChanged, flagSet bool) error {
	o = session.NormalizeShareOptions(o)
	return updateSettings(func(st *appSettings) {
		fps, mh, sc := o.FPS, o.MinHeight, o.SmallerCopy
		st.FPS = &fps
		st.SlowViewer = o.SlowViewer
		st.MinHeight = &mh
		st.SmallerCopy = &sc
		st.KeepQuality = o.KeepQuality
		if uploadChanged {
			if o.UploadKbps > 0 {
				up := o.UploadKbps
				st.UploadKbps = &up
			} else if !flagSet {
				st.UploadKbps = nil
			}
		}
	})
}

func saveE2EE(on bool) error {
	return updateSettings(func(st *appSettings) {
		st.E2EE = &on
	})
}

func loadPresets() []QualityPreset { return cleanPresets(loadSettings().Presets) }

func savePresets(ps []QualityPreset) error {
	return updateSettings(func(st *appSettings) {
		st.Presets = ps
	})
}

// LoadUIScale is the saved interface size ("" = automatic).
func LoadUIScale() string { return loadSettings().UIScale }

// SaveUIScale keeps the interface size for the next start.
func SaveUIScale(pref string) error {
	return updateSettings(func(st *appSettings) {
		st.UIScale = pref
	})
}

func savePortMap(on bool) error {
	return updateSettings(func(st *appSettings) {
		st.PortMap = &on
	})
}

// SetPortMap implements ui.Backend: "Open a port on my router automatically",
// kept on disk. Outside a call it applies immediately; during a call it
// applies from the next session.
func (a *App) SetPortMap(on bool) error {
	if err := savePortMap(on); err != nil {
		a.log.Warn("save settings", "err", err)
	}
	a.mu.Lock()
	a.portMap = on
	idle := a.sess == nil
	a.mu.Unlock()
	if idle {
		a.applyPortMap(on)
	}
	a.bc.Notify()
	return nil
}

// SetPlaySounds implements ui.Backend: "Play sounds", kept on disk across
// sessions.
func (a *App) SetPlaySounds(on bool) error {
	if err := savePlaySounds(on); err != nil {
		a.log.Warn("save settings", "err", err)
	}
	a.mu.Lock()
	a.playSounds = on
	s := a.sess
	a.mu.Unlock()
	if s != nil {
		s.SetPlaySounds(on)
	}
	a.bc.Notify()
	return nil
}

// SetVoiceInSystem implements ui.Backend: "Include voice chat apps in
// whole-system audio", kept on disk. Off, others in the voice call don't
// hear themselves twice.
func (a *App) SetVoiceInSystem(on bool) error {
	if err := saveVoiceInSystem(on); err != nil {
		a.log.Warn("save settings", "err", err)
	}
	audio.SetIncludeVoice(on)
	a.mu.Lock()
	s := a.sess
	a.mu.Unlock()
	if s != nil {
		s.RefreshAudio(true)
	}
	a.bc.Notify()
	return nil
}

// SetAutoWatch implements ui.Backend: "Start watching new streams
// automatically", kept on disk across sessions.
func (a *App) SetAutoWatch(on bool) error {
	if err := saveAutoWatch(on); err != nil {
		a.log.Warn("save settings", "err", err)
	}
	a.mu.Lock()
	a.autoWatch = on
	s := a.sess
	a.mu.Unlock()
	if s != nil {
		s.SetAutoWatch(on)
	}
	a.bc.Notify()
	return nil
}

// ToggleWatch implements ui.Backend: toggles the eye on "self" or a peer ID.
func (a *App) ToggleWatch(id string) error {
	s, err := a.current()
	if err != nil {
		return err
	}
	return s.ToggleWatch(id)
}

// PopOut implements ui.Backend: pops "self" or a peer stream out into its own
// window, or back into the main streaming window.
func (a *App) PopOut(id string) error {
	s, err := a.current()
	if err != nil {
		return err
	}
	return s.PopOut(id)
}

// Pin implements ui.Backend: pins or unpins "self" or a peer stream in the
// main streaming window.
func (a *App) Pin(id string) error {
	s, err := a.current()
	if err != nil {
		return err
	}
	return s.Pin(id)
}

// renameRetry renames, retrying briefly: on Windows an antivirus scan or a
// reader holding settings.json open makes it fail for a moment.
func renameRetry(from, to string) error {
	var err error
	for i := range 5 {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		time.Sleep(time.Duration(i+1) * 20 * time.Millisecond)
	}
	return err
}
