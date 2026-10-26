package capture

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32                  = windows.NewLazySystemDLL("user32.dll")
	procEnumWindows         = user32.NewProc("EnumWindows")
	procEnumDisplayMonitors = user32.NewProc("EnumDisplayMonitors")
	procGetMonitorInfoW     = user32.NewProc("GetMonitorInfoW")
	procIsWindowVisible     = user32.NewProc("IsWindowVisible")
	procIsIconic            = user32.NewProc("IsIconic")
	procGetWindowTextW      = user32.NewProc("GetWindowTextW")
	procGetWindowLongPtrW   = user32.NewProc("GetWindowLongPtrW")
	procGetWindow           = user32.NewProc("GetWindow")
	procIsWindow            = user32.NewProc("IsWindow")
	dwmapi                  = windows.NewLazySystemDLL("dwmapi.dll")
	procDwmGetWindowAttr    = dwmapi.NewProc("DwmGetWindowAttribute")
)

const (
	gwlExStyle       = -20
	wsExToolWindow   = 0x00000080
	gwOwner          = 4
	dwmwaCloaked     = 14
	monitorInfoFPrim = 1
)

type monitorInfoEx struct {
	Size    uint32
	Monitor windows.Rect
	Work    windows.Rect
	Flags   uint32
	Device  [32]uint16
}

// List returns the monitors (by HMONITOR, for d3d11screencapturesrc
// monitor-handle) and the top-level application windows Alt-Tab would show.
func List() ([]Target, error) {
	var out []Target
	n := 0
	monCB := syscall.NewCallback(func(h, _ uintptr, _ uintptr, _ uintptr) uintptr {
		mi := monitorInfoEx{Size: uint32(unsafe.Sizeof(monitorInfoEx{}))}
		procGetMonitorInfoW.Call(h, uintptr(unsafe.Pointer(&mi)))
		r := mi.Monitor
		name := fmt.Sprintf("Display %d (%dx%d)", n+1, r.Right-r.Left, r.Bottom-r.Top)
		if mi.Flags&monitorInfoFPrim != 0 {
			name += ", primary"
		}
		out = append(out, Target{Kind: "monitor", Name: name, Source: fmt.Sprintf("monitor:%#x", h)})
		n++
		return 1
	})
	procEnumDisplayMonitors.Call(0, 0, monCB, 0)

	winCB := syscall.NewCallback(func(h, _ uintptr) uintptr {
		if title := appWindowTitle(h); title != "" {
			out = append(out, Target{Kind: "window", Name: title, Source: fmt.Sprintf("window:%#x", h)})
		}
		return 1
	})
	procEnumWindows.Call(winCB, 0)
	return out, nil
}

// appWindowTitle returns the title of a window Alt-Tab would list, else "".
func appWindowTitle(h uintptr) string {
	if v, _, _ := procIsWindowVisible.Call(h); v == 0 {
		return ""
	}
	if v, _, _ := procIsIconic.Call(h); v != 0 {
		return ""
	}
	if owner, _, _ := procGetWindow.Call(h, gwOwner); owner != 0 {
		return ""
	}
	ex, _, _ := procGetWindowLongPtrW.Call(h, ^uintptr(-gwlExStyle-1)) // GWL_EXSTYLE (-20) as a uintptr
	if ex&wsExToolWindow != 0 {
		return ""
	}
	var cloaked uint32
	procDwmGetWindowAttr.Call(h, dwmwaCloaked, uintptr(unsafe.Pointer(&cloaked)), 4)
	if cloaked != 0 { // other virtual desktop, suspended UWP app
		return ""
	}
	buf := make([]uint16, 256)
	l, _, _ := procGetWindowTextW.Call(h, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return windows.UTF16ToString(buf[:l])
}

func windowGone(id uint64) bool {
	r, _, _ := procIsWindow.Call(uintptr(id))
	return r == 0
}
