package diag

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Processes are the running programs known to hook into games and the
// desktop (overlays) or to capture and encode video themselves.
type Processes struct {
	Total int    `json:"total"`
	Found []Proc `json:"found"`
	// GPU is what each GPU engine type was busy with over a short sample,
	// by process (Windows).
	GPU []EngineUse `json:"gpu,omitempty"`
}

// Proc is one running program of interest.
type Proc struct {
	Exe  string `json:"exe"`
	PID  int    `json:"pid"`
	What string `json:"what"`
}

// EngineUse is how busy one process kept one GPU engine type (percent of
// that engine; a process on several engines of a type is summed).
type EngineUse struct {
	PID     int     `json:"pid"`
	Exe     string  `json:"exe,omitempty"`
	Engine  string  `json:"engine"` // 3D, VideoEncode, VideoDecode, Copy, ...
	Percent float64 `json:"percent"`
}

// knownProcs are programs worth knowing about while sharing, by lower-case
// executable name (".exe" dropped on Windows).
var knownProcs = map[string]string{
	"blite":                     "Bellion Share itself (close it: it may be capturing or encoding too)",
	"discord":                   "Discord (overlay; Go Live streams use the GPU encoder)",
	"discordptb":                "Discord PTB (overlay, Go Live)",
	"discordcanary":             "Discord Canary (overlay, Go Live)",
	"steam":                     "Steam (in-game overlay, game recording)",
	"gameoverlayui":             "Steam overlay",
	"gameoverlayui64":           "Steam overlay",
	"nvidia share":              "NVIDIA overlay / ShadowPlay (Instant Replay records with NVENC)",
	"nvsphelper64":              "NVIDIA ShadowPlay helper (recording with NVENC)",
	"nvidia overlay":            "NVIDIA App overlay (Instant Replay records with NVENC)",
	"nvidia app":                "NVIDIA App",
	"nvidia geforce experience": "GeForce Experience",
	"nvcontainer":               "NVIDIA Container (driver services; normal)",
	"obs64":                     "OBS Studio (capture, encoder)",
	"obs32":                     "OBS Studio (capture, encoder)",
	"obs":                       "OBS Studio (capture, encoder)",
	"streamlabs obs":            "Streamlabs (capture, encoder)",
	"xsplit.core":               "XSplit (capture, encoder)",
	"gamebar":                   "Xbox Game Bar (overlay)",
	"gamebarftserver":           "Xbox Game Bar",
	"gamebarpresencewriter":     "Xbox Game Bar",
	"broadcastdvrserver":        "Windows Game DVR (background recording uses the encoder)",
	"msiafterburner":            "MSI Afterburner (overlay)",
	"rtss":                      "RivaTuner Statistics Server (overlay, frame limiter)",
	"rtsshooksloader64":         "RivaTuner hooks (overlay)",
	"medal":                     "Medal (clip recorder, uses the encoder)",
	"medalencoder":              "Medal encoder",
	"overwolf":                  "Overwolf (overlay)",
	"outplayed":                 "Outplayed (clip recorder)",
	"radeonsoftware":            "AMD Software (ReLive recording, overlay)",
	"amdrsserv":                 "AMD ReLive (recording)",
	"wallpaper64":               "Wallpaper Engine (animated desktop, GPU load)",
	"wallpaper32":               "Wallpaper Engine (animated desktop, GPU load)",
	"lively":                    "Lively Wallpaper (animated desktop, GPU load)",
	"parsecd":                   "Parsec (capture, encoder, virtual displays)",
	"sunshine":                  "Sunshine (capture, encoder)",
	"teamviewer":                "TeamViewer (screen capture)",
	"anydesk":                   "AnyDesk (screen capture)",
	"rustdesk":                  "RustDesk (screen capture)",
	"zoom":                      "Zoom (screen share)",
	"ms-teams":                  "Microsoft Teams (screen share)",
	"teams":                     "Microsoft Teams (screen share)",
	"bandicam":                  "Bandicam (recorder)",
	"action":                    "Mirillis Action! (recorder)",
	"fraps":                     "Fraps (overlay, recorder)",
	"dxtory":                    "Dxtory (recorder)",
	"eadesktop":                 "EA app (in-game overlay)",
	"upc":                       "Ubisoft Connect (overlay)",
	"battle.net":                "Battle.net (overlay)",
	"epicgameslauncher":         "Epic Games Launcher (overlay)",
	"razer cortex":              "Razer Cortex (game booster)",
	"signalrgb":                 "SignalRGB (screen ambience capture)",
	"gpu-screen-recorder":       "GPU Screen Recorder",
	"gsr-kms-server":            "GPU Screen Recorder",
	"wf-recorder":               "wf-recorder",
	"simplescreenrecorder":      "SimpleScreenRecorder",
	"ffmpeg":                    "ffmpeg (maybe recording)",
}

func runProcs(ctx context.Context, r *Report, p *printer) error {
	ps, err := processes()
	if err != nil {
		return err
	}
	res := &Processes{Total: len(ps)}
	for _, pr := range ps {
		key := strings.ToLower(strings.TrimSuffix(strings.ToLower(pr.Exe), ".exe"))
		if what := knownProcs[key]; what != "" {
			pr.What = what
			res.Found = append(res.Found, pr)
		}
	}
	sort.Slice(res.Found, func(i, j int) bool { return res.Found[i].Exe < res.Found[j].Exe })
	r.with(func() { r.Processes = res })
	p.f("   %d processes running", res.Total)
	if len(res.Found) == 0 {
		p.f("   none of the known overlays, recorders or capture programs")
	}
	for _, pr := range res.Found {
		p.f("   %-28s pid %-6d %s", pr.Exe, pr.PID, pr.What)
	}

	// Who uses the GPU now, the video encoder above all.
	stop, err := sampleGPU()
	if err != nil {
		p.f("   GPU use per process: %v", err)
		return nil
	}
	sleepCtx(ctx, 2*time.Second)
	use := stop()
	nameProcs(use)
	r.with(func() { res.GPU = use })
	shown := 0
	for _, u := range use {
		if u.Percent < 1 || shown >= 12 {
			continue
		}
		p.f("   GPU %-12s %5.1f%%  %s (pid %d)", u.Engine, u.Percent, orUnknown(u.Exe), u.PID)
		shown++
	}
	if shown == 0 {
		p.f("   no process kept a GPU engine above 1%% busy")
	}
	return nil
}

// nameProcs fills in the executable names of use's processes.
func nameProcs(use []EngineUse) {
	ps, _ := processes()
	names := map[int]string{}
	for _, pr := range ps {
		names[pr.PID] = pr.Exe
	}
	for i := range use {
		use[i].Exe = names[use[i].PID]
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

// engineTotals sums use by engine type.
func engineTotals(use []EngineUse) map[string]float64 {
	m := map[string]float64{}
	for _, u := range use {
		m[u.Engine] += u.Percent
	}
	return m
}

func fmtEngines(m map[string]float64) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var parts []string
	for _, k := range ks {
		parts = append(parts, fmt.Sprintf("%s %.0f%%", k, m[k]))
	}
	return strings.Join(parts, ", ")
}
