package capture

import (
	"fmt"
	"image"
	"path/filepath"
	"strings"
	"sync"
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
	procGetWindowThreadPID  = user32.NewProc("GetWindowThreadProcessId")
	procSendMessageTimeoutW = user32.NewProc("SendMessageTimeoutW")
	procGetClassLongPtrW    = user32.NewProc("GetClassLongPtrW")
	procGetIconInfo         = user32.NewProc("GetIconInfo")
	procDestroyIcon         = user32.NewProc("DestroyIcon")
	procGetDC               = user32.NewProc("GetDC")
	procReleaseDC           = user32.NewProc("ReleaseDC")
	dwmapi                  = windows.NewLazySystemDLL("dwmapi.dll")
	procDwmGetWindowAttr    = dwmapi.NewProc("DwmGetWindowAttribute")
	gdi32                   = windows.NewLazySystemDLL("gdi32.dll")
	procGetObjectW          = gdi32.NewProc("GetObjectW")
	procGetDIBits           = gdi32.NewProc("GetDIBits")
	procDeleteObject        = gdi32.NewProc("DeleteObject")
	shell32                 = windows.NewLazySystemDLL("shell32.dll")
	procExtractIconExW      = shell32.NewProc("ExtractIconExW")
)

const (
	gwlExStyle       = -20
	wsExToolWindow   = 0x00000080
	gwOwner          = 4
	dwmwaCloaked     = 14
	monitorInfoFPrim = 1

	wmGetIcon       = 0x007F
	iconSmall       = 0
	iconBig         = 1
	iconSmall2      = 2
	smtoAbortIfHung = 0x0002
	iconTimeoutMs   = 50
	gclpHIcon       = ^uintptr(14 - 1) // GCLP_HICON (-14) as a uintptr
	gclpHIconSm     = ^uintptr(34 - 1) // GCLP_HICONSM (-34)
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
		out = append(out, Target{Kind: "monitor", Name: name, Source: fmt.Sprintf("monitor:%#x", h), Primary: mi.Flags&monitorInfoFPrim != 0})
		n++
		return 1
	})
	procEnumDisplayMonitors.Call(0, 0, monCB, 0)

	winCB := syscall.NewCallback(func(h, _ uintptr) uintptr {
		if title := appWindowTitle(h); title != "" {
			out = append(out, Target{Kind: "window", Name: title, Source: fmt.Sprintf("window:%#x", h), App: exeApp(windowExe(h))})
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

func windowPID(id uint64) int {
	var pid uint32
	procGetWindowThreadPID.Call(uintptr(id), uintptr(unsafe.Pointer(&pid)))
	return int(pid)
}

// windowExe is the full path of the program that owns window h, or "".
func windowExe(h uintptr) string {
	var pid uint32
	procGetWindowThreadPID.Call(h, uintptr(unsafe.Pointer(&pid)))
	if pid == 0 {
		return ""
	}
	p, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(p)
	for _, size := range []int{1024, windows.MAX_LONG_PATH} { // most paths fit the first
		buf := make([]uint16, size)
		n := uint32(len(buf))
		err := windows.QueryFullProcessImageName(p, 0, &buf[0], &n)
		if err == nil {
			return windows.UTF16ToString(buf[:n])
		}
		if err != windows.ERROR_INSUFFICIENT_BUFFER {
			break
		}
	}
	return ""
}

// exeApp is a program's name for people: C:\...\Discord.exe -> "Discord".
func exeApp(path string) string {
	if path == "" {
		return ""
	}
	b := filepath.Base(path)
	return strings.TrimSuffix(b, filepath.Ext(b))
}

func windowApp(id uint64) string { return exeApp(windowExe(uintptr(id))) }

var (
	iconMu     sync.Mutex
	hwndIcons  = map[uintptr]image.Image{} // nil = the window has none of its own
	exeIcons   = map[string]image.Image{}  // nil = the exe has none
	maxIconDim = int32(256)
)

func icons(ts []Target) map[string]image.Image {
	out := map[string]image.Image{}
	keep := map[uintptr]bool{}
	defer func() { // forget windows that closed (handles get reused)
		iconMu.Lock()
		for h := range hwndIcons {
			if !keep[h] {
				delete(hwndIcons, h)
			}
		}
		iconMu.Unlock()
	}()
	for _, t := range ts {
		id, err := ParseWindow(strings.TrimPrefix(t.Source, "window:"))
		if err != nil {
			continue
		}
		keep[uintptr(id)] = true
		if img := windowIcon(uintptr(id)); img != nil {
			out[t.Source] = img
		}
	}
	return out
}

// windowIcon is a window's icon: the one it set (WM_GETICON), its class's,
// else its exe's. The big (32 px) icons come first: the list draws them at
// 20 points, which is more than 16 pixels on scaled displays.
func windowIcon(h uintptr) image.Image {
	iconMu.Lock()
	img, ok := hwndIcons[h]
	iconMu.Unlock()
	if ok {
		if img != nil {
			return img
		}
		return exeIcon(windowExe(h))
	}
	img = sharedWindowIcon(h)
	iconMu.Lock()
	hwndIcons[h] = img
	iconMu.Unlock()
	if img != nil {
		return img
	}
	return exeIcon(windowExe(h))
}

// sharedWindowIcon reads the window's or its class's icon. Those belong to
// the window: we only read them, never destroy them.
func sharedWindowIcon(h uintptr) image.Image {
	for _, which := range []uintptr{iconBig, iconSmall2, iconSmall} {
		var hicon uintptr
		// SendMessageTimeout: a hung app must not hang the window list.
		r, _, _ := procSendMessageTimeoutW.Call(h, wmGetIcon, which, 0, smtoAbortIfHung, iconTimeoutMs, uintptr(unsafe.Pointer(&hicon)))
		if r != 0 && hicon != 0 {
			if img := iconImage(hicon); img != nil {
				return img
			}
		}
	}
	for _, idx := range []uintptr{gclpHIcon, gclpHIconSm} {
		if hicon, _, _ := procGetClassLongPtrW.Call(h, idx); hicon != 0 {
			if img := iconImage(hicon); img != nil {
				return img
			}
		}
	}
	return nil
}

// exeIcon is the first icon in a program file, cached by path.
func exeIcon(path string) image.Image {
	if path == "" {
		return nil
	}
	key := strings.ToLower(path)
	iconMu.Lock()
	img, ok := exeIcons[key]
	iconMu.Unlock()
	if ok {
		return img
	}
	p, err := windows.UTF16PtrFromString(path)
	if err == nil {
		var large, small uintptr
		n, _, _ := procExtractIconExW.Call(uintptr(unsafe.Pointer(p)), 0, uintptr(unsafe.Pointer(&large)), uintptr(unsafe.Pointer(&small)), 1)
		if n != 0 {
			if large != 0 {
				img = iconImage(large)
			}
			if img == nil && small != 0 {
				img = iconImage(small)
			}
		}
		// ExtractIconEx's icons are ours to destroy.
		if large != 0 {
			procDestroyIcon.Call(large)
		}
		if small != 0 {
			procDestroyIcon.Call(small)
		}
	}
	iconMu.Lock()
	exeIcons[key] = img
	iconMu.Unlock()
	return img
}

type iconInfo struct {
	FIcon    int32
	XHotspot uint32
	YHotspot uint32
	HbmMask  uintptr
	HbmColor uintptr
}

type bitmap struct {
	Type       int32
	Width      int32
	Height     int32
	WidthBytes int32
	Planes     uint16
	BitsPixel  uint16
	Bits       uintptr
}

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

// iconImage copies an HICON's pixels. 32-bit icons carry their own alpha;
// older ones are opaque except where their AND mask is set. Monochrome icons
// (no color bitmap) are skipped. It frees the bitmaps GetIconInfo makes, not
// the icon.
func iconImage(hicon uintptr) image.Image {
	var ii iconInfo
	if r, _, _ := procGetIconInfo.Call(hicon, uintptr(unsafe.Pointer(&ii))); r == 0 {
		return nil
	}
	if ii.HbmMask != 0 {
		defer procDeleteObject.Call(ii.HbmMask)
	}
	if ii.HbmColor == 0 {
		return nil
	}
	defer procDeleteObject.Call(ii.HbmColor)
	var bm bitmap
	if r, _, _ := procGetObjectW.Call(ii.HbmColor, unsafe.Sizeof(bm), uintptr(unsafe.Pointer(&bm))); r == 0 {
		return nil
	}
	w, h := bm.Width, bm.Height
	if w <= 0 || h <= 0 || w > maxIconDim || h > maxIconDim {
		return nil
	}
	dc, _, _ := procGetDC.Call(0)
	if dc == 0 {
		return nil
	}
	defer procReleaseDC.Call(0, dc)
	px := dibBits(dc, ii.HbmColor, w, h)
	if px == nil {
		return nil
	}
	hasAlpha := false
	for i := 3; i < len(px); i += 4 {
		if px[i] != 0 {
			hasAlpha = true
			break
		}
	}
	var mask []byte
	if !hasAlpha && ii.HbmMask != 0 {
		mask = dibBits(dc, ii.HbmMask, w, h) // with a color bitmap, the mask is as tall as it
	}
	img := image.NewNRGBA(image.Rect(0, 0, int(w), int(h)))
	for i := 0; i+3 < len(px); i += 4 {
		a := px[i+3]
		if !hasAlpha {
			a = 255
			if mask != nil && mask[i] != 0 { // set AND bit: transparent
				a = 0
			}
		}
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = px[i+2], px[i+1], px[i], a // BGRA -> RGBA
	}
	return img
}

// dibBits reads a bitmap as top-down 32-bit BGRA rows.
func dibBits(dc, hbm uintptr, w, h int32) []byte {
	var bi struct {
		Header bitmapInfoHeader
		Colors [256]uint32 // room for a color table GetDIBits may write
	}
	bi.Header = bitmapInfoHeader{Size: uint32(unsafe.Sizeof(bi.Header)), Width: w, Height: -h, Planes: 1, BitCount: 32}
	buf := make([]byte, int(w)*int(h)*4)
	r, _, _ := procGetDIBits.Call(dc, hbm, 0, uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&bi)), 0)
	if r == 0 {
		return nil
	}
	return buf
}
