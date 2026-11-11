package media

import (
	"os"
	"sync"
	"syscall"
	"unsafe"
)

var (
	visUser32           = syscall.NewLazyDLL("user32.dll")
	procEnumWindows     = visUser32.NewProc("EnumWindows")
	procWindowThreadPID = visUser32.NewProc("GetWindowThreadProcessId")
	procIsWindowVisible = visUser32.NewProc("IsWindowVisible")
	procIsIconicVis     = visUser32.NewProc("IsIconic")
	procGetWindowTextW  = visUser32.NewProc("GetWindowTextW")
	visMu               sync.Mutex
	visCallback         uintptr
	visState            struct{ shown, minimized int }
)

// VideoWindowsMinimized reports whether we show video windows and every
// one of them is minimized. The sinks make their own windows, so
// this looks at all of our visible top-level windows but the preview.
func VideoWindowsMinimized() bool {
	visMu.Lock()
	defer visMu.Unlock()
	if visCallback == 0 {
		visCallback = syscall.NewCallback(func(h, _ uintptr) uintptr {
			var pid uint32
			procWindowThreadPID.Call(h, uintptr(unsafe.Pointer(&pid)))
			if int(pid) != os.Getpid() {
				return 1
			}
			if v, _, _ := procIsWindowVisible.Call(h); v == 0 {
				return 1
			}
			buf := make([]uint16, 128)
			procGetWindowTextW.Call(h, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
			if syscall.UTF16ToString(buf) == previewTitle {
				return 1
			}
			visState.shown++
			if v, _, _ := procIsIconicVis.Call(h); v != 0 {
				visState.minimized++
			}
			return 1
		})
	}
	visState.shown, visState.minimized = 0, 0
	procEnumWindows.Call(visCallback, 0)
	return visState.shown > 0 && visState.minimized == visState.shown
}
