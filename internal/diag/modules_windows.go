// Package diag holds field diagnostics.
package diag

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// WatchModules logs the DLLs loaded into this process: at start,
// every one outside Blite's folder and the Windows directory, and afterwards
// every newly loaded one with its path. Injected DLLs (overlays, AV hooks)
// then show up in the log with a time to compare against a crash.
func WatchModules(ctx context.Context, log *slog.Logger) {
	exe, _ := os.Executable()
	own := strings.ToLower(filepath.Dir(filepath.Dir(exe)))
	sysroot := strings.ToLower(os.Getenv("SystemRoot"))
	seen := map[string]bool{}
	first := true
	for {
		mods, err := modules()
		if err != nil {
			log.Warn("module list", "err", err)
			return
		}
		for _, m := range mods {
			k := strings.ToLower(m)
			if seen[k] {
				continue
			}
			seen[k] = true
			foreign := !strings.HasPrefix(k, own) && (sysroot == "" || !strings.HasPrefix(k, sysroot))
			switch {
			case !first:
				log.Info("module loaded", "path", m, "foreign", foreign)
			case foreign:
				log.Info("module at start", "path", m)
			}
		}
		if first {
			log.Info("modules at start", "count", len(mods))
			first = false
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func modules() ([]string, error) {
	p := windows.CurrentProcess()
	hs := make([]windows.Handle, 512)
	for {
		var need uint32
		if err := windows.EnumProcessModules(p, &hs[0], uint32(len(hs))*uint32(unsafe.Sizeof(hs[0])), &need); err != nil {
			return nil, err
		}
		n := int(need / uint32(unsafe.Sizeof(hs[0])))
		if n <= len(hs) {
			hs = hs[:n]
			break
		}
		hs = make([]windows.Handle, n+64)
	}
	out := make([]string, 0, len(hs))
	buf := make([]uint16, windows.MAX_LONG_PATH)
	for _, h := range hs {
		if err := windows.GetModuleFileNameEx(p, h, &buf[0], uint32(len(buf))); err == nil {
			out = append(out, windows.UTF16ToString(buf))
		}
	}
	return out, nil
}
