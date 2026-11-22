package audio

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows uses WASAPI process loopback (GStreamer wasapi2src, MSVC build):
// "system" = everything except Blite's own process tree; "apps" = one
// include-process-tree capture per chosen app, mixed. Needs Windows 10 build
// 20348+ / Windows 11; older Windows gets plain loopback from the element.
// Property names checked in the 1.26.11 gstwasapi2.dll.

// Sharer holds the selection; the capture description follows it.
type Sharer struct {
	mu    sync.Mutex
	sel   Selection
	shown shareLog
	log   *slog.Logger
	voice string // the voice apps whole-system sharing last left out
}

// NewSharer: log (nil = none) gets the processes each selection captures.
func NewSharer(log *slog.Logger) (*Sharer, error) {
	return &Sharer{sel: Selection{Mode: "none"}, shown: shareLog{log: orDiscard(log)}, log: orDiscard(log)}, nil
}

func (s *Sharer) Select(sel Selection) error {
	switch sel.Mode {
	case "none", "system", "apps", "window":
	default:
		return errors.New("unknown audio mode")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sel = sel
	// Loopback can't tell whether a process plays anything: log the
	// processes captured, and a selection whose processes are all gone
	// (e.g. the app restarted), which captures silence.
	var matched []stream
	for _, pid := range targetPIDs(sel) {
		if name := exeName(uint32(pid)); name != "" {
			matched = append(matched, stream{name: name, pid: pid, links: 1})
		}
	}
	if sel.Mode != "system" {
		s.shown.note(sel, matched, nil, time.Now())
	}
	return nil
}

// targetPIDs are the processes an apps or window selection captures.
func targetPIDs(sel Selection) []int {
	switch sel.Mode {
	case "window":
		if sel.PID > 0 {
			return []int{sel.PID}
		}
	case "apps":
		var pids []int
		for _, k := range sel.Apps {
			if i := strings.LastIndexByte(k, '#'); i >= 0 {
				if pid, err := strconv.Atoi(k[i+1:]); err == nil && pid > 0 {
					pids = append(pids, pid)
				}
			}
		}
		return pids
	}
	return nil
}

func (s *Sharer) Close() {}

const silence = "audiotestsrc is-live=true wave=silence"

func loopback(mode string, pid int) string {
	return fmt.Sprintf("wasapi2src loopback=true low-latency=true loopback-silence-on-device-mute=true loopback-mode=%s loopback-target-pid=%d", mode, pid)
}

// Capture returns the source for the current selection; it changes when the
// selection does, which makes the session restart the audio pipeline.
func (s *Sharer) Capture() CaptureSource {
	s.mu.Lock()
	sel := s.sel
	s.mu.Unlock()
	switch sel.Mode {
	case "system":
		if pids, ok := s.systemPIDs(); ok {
			return mix(pids)
		}
		return CaptureSource(loopback("exclude-process-tree", os.Getpid()))
	case "window", "apps":
		return mix(targetPIDs(sel))
	}
	return silence
}

// mix captures the process trees of pids, mixed.
func mix(pids []int) CaptureSource {
	switch len(pids) {
	case 0:
		return silence
	case 1:
		return CaptureSource(loopback("include-process-tree", pids[0]))
	}
	var b strings.Builder
	b.WriteString("audiomixer name=amix ")
	for _, pid := range pids {
		fmt.Fprintf(&b, " %s ! audioconvert ! audioresample ! amix. ", loopback("include-process-tree", pid))
	}
	b.WriteString(" amix.")
	return CaptureSource(b.String())
}

// systemPIDs: while a voice chat app has an audio session (and voice isn't
// included), whole-system sharing is the mix of every other app with one
// (ok); otherwise everything but us (!ok), which also catches apps that
// start playing later. The session watches Capture for changes.
func (s *Sharer) systemPIDs() ([]int, bool) {
	if includeVoice.Load() {
		s.noteVoice(nil)
		return nil, false
	}
	pids, err := sessionPIDs()
	if err != nil {
		s.log.Warn("audio sessions", "err", err)
		return nil, false
	}
	own := os.Getpid()
	var keep []int
	var voice []string
	for _, pid := range pids {
		name := exeName(uint32(pid))
		switch {
		case pid == own || name == "":
		case isVoiceApp(name):
			voice = append(voice, fmt.Sprintf("%s pid=%d", name, pid))
		default:
			keep = append(keep, pid)
		}
	}
	sort.Ints(keep)
	s.noteVoice(voice)
	return keep, len(voice) > 0
}

func (s *Sharer) noteVoice(voice []string) {
	sort.Strings(voice)
	v := strings.Join(voice, ", ")
	s.mu.Lock()
	changed := v != s.voice
	s.voice = v
	s.mu.Unlock()
	if changed && v != "" {
		s.log.Info("whole-system audio leaves out voice chat", "apps", v)
	} else if changed {
		s.log.Info("whole-system audio: everything but Bellion Share")
	}
}

var (
	user32                       = windows.NewLazySystemDLL("user32.dll")
	procEnumWindows              = user32.NewProc("EnumWindows")
	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
	procGetWindowTextLengthW     = user32.NewProc("GetWindowTextLengthW")
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
)

// ListApps returns processes with a visible, titled top-level window (the
// programs a user would pick), excluding Blite. Process loopback includes the
// process tree, so a browser's audio service child process is covered.
func ListApps() ([]App, error) {
	own := uint32(os.Getpid())
	pids := map[uint32]bool{}
	cb := syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		if v, _, _ := procIsWindowVisible.Call(hwnd); v == 0 {
			return 1
		}
		if n, _, _ := procGetWindowTextLengthW.Call(hwnd); n == 0 {
			return 1
		}
		var pid uint32
		procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
		if pid != 0 && pid != own {
			pids[pid] = true
		}
		return 1
	})
	procEnumWindows.Call(cb, 0)

	var apps []App
	for pid := range pids {
		name := exeName(pid)
		if name == "" || strings.EqualFold(name, "explorer.exe") {
			continue
		}
		apps = append(apps, App{Key: name + "#" + strconv.Itoa(int(pid)), Name: strings.TrimSuffix(name, ".exe"), PID: int(pid)})
	}
	sort.Slice(apps, func(i, j int) bool { return strings.ToLower(apps[i].Name) < strings.ToLower(apps[j].Name) })
	return apps, nil
}

func exeName(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return ""
	}
	return filepath.Base(windows.UTF16ToString(buf[:n]))
}

// ListDevices: choosing a Windows output device needs the GStreamer device
// monitor (not wired yet); the call plays on the default device.
func ListDevices() ([]Device, error) { return nil, nil }

// PlaybackSink plays on the default render device.
func PlaybackSink(string) string { return "wasapi2sink low-latency=true" }
