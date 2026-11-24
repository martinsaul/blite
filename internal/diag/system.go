package diag

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"blite/internal/gst"
	"blite/internal/media"
)

// System is the computer and its settings that bear on capture and encoding.
type System struct {
	OS    string  `json:"os"`
	CPU   string  `json:"cpu"`
	Cores int     `json:"cores"`
	RAMGB float64 `json:"ramGB"`
	GPUs  []GPU   `json:"gpus,omitempty"`
	// Settings: HAGS, Game Mode, background recording, power plan, ...
	Settings map[string]string `json:"settings,omitempty"`
	// GStreamer: version, the plugin file of each element we rely on, and
	// the environment variables that could make it load other plugins.
	GStreamer string            `json:"gstreamer"`
	Plugins   map[string]string `json:"plugins,omitempty"`
	GstEnv    map[string]string `json:"gstEnv,omitempty"`
	Encoders  []string          `json:"encoders"` // installed share encoders, best first
	ExeDir    string            `json:"exeDir"`
	NextToApp bool              `json:"nextToApp"` // blite(.exe) is in the same folder
	Nvidia    *NvidiaSMI        `json:"nvidia,omitempty"`
}

// GPU is one graphics adapter.
type GPU struct {
	Name       string `json:"name"`
	VRAMMB     int    `json:"vramMB,omitempty"`
	Driver     string `json:"driver,omitempty"`
	DriverDate string `json:"driverDate,omitempty"`
	Provider   string `json:"provider,omitempty"`
	LUID       string `json:"luid,omitempty"`
	// DXGI is the adapter's DXGI index (0 is what D3D11 elements pick by
	// default); -1 when it was only found in the registry.
	DXGI int `json:"dxgi"`
}

// Display is one active monitor.
type Display struct {
	Name       string  `json:"name"`
	Device     string  `json:"device,omitempty"`
	Width      int     `json:"width"`
	Height     int     `json:"height"`
	X          int     `json:"x"`
	Y          int     `json:"y"`
	RefreshHz  float64 `json:"refreshHz,omitempty"`
	ScalePct   int     `json:"scalePct,omitempty"`
	Primary    bool    `json:"primary"`
	HDR        string  `json:"hdr,omitempty"` // on, off (supported), unsupported
	Connection string  `json:"connection,omitempty"`
	Adapter    string  `json:"adapter,omitempty"`
}

// NvidiaSMI is what nvidia-smi says, as text.
type NvidiaSMI struct {
	Path    string `json:"path"`
	Query   string `json:"query,omitempty"`
	Encoder string `json:"encoder,omitempty"`
	Error   string `json:"error,omitempty"`
}

// pluginFactories are the elements whose plugin file the report names, so
// it shows whether the bundled plugins were loaded.
var pluginFactories = []string{"videotestsrc", "videoconvertscale", "x264enc", "nvh264enc", "qsvh264enc",
	"amfh264enc", "mfh264enc", "d3d11screencapturesrc", "ximagesrc"}

func runSystem(ctx context.Context, r *Report, p *printer) error {
	s := &System{Cores: runtime.NumCPU(), Settings: map[string]string{}, Plugins: map[string]string{}, GstEnv: map[string]string{}}
	r.with(func() { r.System = s })
	var errs []string
	if err := platformSystem(s); err != nil {
		errs = append(errs, err.Error())
	}
	p.f("   OS:   %s", s.OS)
	p.f("   CPU:  %s, %d threads", s.CPU, s.Cores)
	p.f("   RAM:  %.1f GB", s.RAMGB)
	for _, g := range s.GPUs {
		line := "   GPU:  " + g.Name
		if g.DXGI >= 0 {
			line += fmt.Sprintf(" (adapter %d", g.DXGI)
			if g.VRAMMB > 0 {
				line += fmt.Sprintf(", %d MB", g.VRAMMB)
			}
			line += ")"
		}
		if g.Driver != "" {
			line += ", driver " + g.Driver
		}
		if g.DriverDate != "" {
			line += " of " + g.DriverDate
		}
		p.f("%s", line)
	}
	keys := make([]string, 0, len(s.Settings))
	for k := range s.Settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p.f("   %s: %s", k, s.Settings[k])
	}

	exe, _ := os.Executable()
	s.ExeDir = filepath.Dir(exe)
	app := "blite"
	if runtime.GOOS == "windows" {
		app = "blite.exe"
	}
	_, err := os.Stat(filepath.Join(s.ExeDir, app))
	s.NextToApp = err == nil
	if !s.NextToApp {
		p.f("   NOTE: %s isn't in %s: this isn't the app's bin folder, so the GStreamer measured may not be the app's", app, s.ExeDir)
	}
	s.GStreamer = gst.Version()
	for _, f := range pluginFactories {
		if file := gst.FactoryFile(f); file != "" {
			s.Plugins[f] = file
		}
	}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(strings.ToUpper(k), "GST") || strings.HasPrefix(strings.ToUpper(k), "GSTREAMER") {
			s.GstEnv[k] = v
		}
	}
	s.Encoders = media.AvailableEncoders()
	p.f("   %s, plugins from %s", s.GStreamer, filepath.Dir(s.Plugins["videotestsrc"]))
	for k, v := range s.GstEnv {
		p.f("   environment %s=%s", k, v)
	}
	p.f("   share encoders installed, best first: %s", strings.Join(s.Encoders, ", "))

	ds, err := displays()
	switch {
	case errors.Is(err, errNotHere):
		p.f("   displays: %v", err)
	case err != nil:
		errs = append(errs, "displays: "+err.Error())
	}
	r.with(func() { r.Displays = ds })
	for _, d := range ds {
		line := fmt.Sprintf("   Display %s: %dx%d", d.Name, d.Width, d.Height)
		if d.RefreshHz > 0 {
			line += fmt.Sprintf(" @ %.2f Hz", d.RefreshHz)
		}
		if d.ScalePct > 0 {
			line += fmt.Sprintf(", scale %d%%", d.ScalePct)
		}
		if d.HDR != "" {
			line += ", HDR " + d.HDR
		}
		if d.Connection != "" {
			line += ", " + d.Connection
		}
		if d.Adapter != "" {
			line += ", on " + d.Adapter
		}
		if d.Primary {
			line += ", PRIMARY"
		}
		p.f("%s", line)
	}

	if nv := nvidiaSMI(ctx); nv != nil {
		s.Nvidia = nv
		if nv.Error != "" {
			p.f("   nvidia-smi: %s", nv.Error)
		}
		for _, l := range strings.Split(strings.TrimSpace(nv.Query), "\n") {
			p.f("   nvidia-smi: %s", l)
		}
		for _, l := range strings.Split(strings.TrimSpace(nv.Encoder), "\n") {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "=") && !strings.HasPrefix(l, "Timestamp") {
				p.f("   nvidia-smi encoder: %s", l)
			}
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// nvidiaQuery are the nvidia-smi fields the report asks for. Older drivers
// lack some; nvsmiFallback is the set every driver since 2018 knows.
const (
	nvidiaQuery = "name,driver_version,pstate,clocks.gr,clocks.max.gr,clocks.video,utilization.gpu,utilization.encoder," +
		"encoder.stats.sessionCount,encoder.stats.averageFps,encoder.stats.averageLatency,temperature.gpu,power.draw," +
		"pcie.link.gen.current,pcie.link.width.current,memory.used,memory.total,display_active"
	nvsmiFallback = "name,driver_version,pstate,utilization.gpu,utilization.encoder,temperature.gpu,memory.used,memory.total"
)

// nvidiaSMIPath finds nvidia-smi ("" if there is none).
func nvidiaSMIPath() string {
	if p, err := exec.LookPath("nvidia-smi"); err == nil {
		return p
	}
	if runtime.GOOS == "windows" {
		for _, p := range []string{
			filepath.Join(os.Getenv("SystemRoot"), "System32", "nvidia-smi.exe"),
			filepath.Join(os.Getenv("ProgramFiles"), "NVIDIA Corporation", "NVSMI", "nvidia-smi.exe"),
		} {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

// nvidiaSMI runs nvidia-smi's GPU query and its encoder sessions (nil
// without nvidia-smi).
func nvidiaSMI(ctx context.Context) *NvidiaSMI {
	path := nvidiaSMIPath()
	if path == "" {
		return nil
	}
	nv := &NvidiaSMI{Path: path}
	q, err := nvidiaGPUQuery(ctx, path)
	nv.Query = q
	if err != nil {
		nv.Error = err.Error()
	}
	if out, err := runCmd(ctx, 15*time.Second, path, "-q", "-d", "ENCODER"); err == nil {
		nv.Encoder = out
	} else if nv.Error == "" {
		nv.Error = "-q -d ENCODER: " + err.Error()
	}
	return nv
}

func nvidiaGPUQuery(ctx context.Context, path string) (string, error) {
	out, err := runCmd(ctx, 15*time.Second, path, "--query-gpu="+nvidiaQuery, "--format=csv")
	if err != nil {
		out, err = runCmd(ctx, 15*time.Second, path, "--query-gpu="+nvsmiFallback, "--format=csv")
	}
	return out, err
}

// runCmd runs a program with a time limit and returns its output.
func runCmd(ctx context.Context, limit time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(strings.ReplaceAll(string(out), "\r\n", "\n"))
	if err != nil {
		if s != "" {
			return s, fmt.Errorf("%w: %s", err, firstLine(s))
		}
		return "", err
	}
	return s, nil
}
