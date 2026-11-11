package media

import (
	"errors"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// d3d11videosink draws into a child window of ours and follows its size.
var previewSinks = []string{"d3d11videosink"}

var (
	user32               = windows.NewLazySystemDLL("user32.dll")
	gdi32                = windows.NewLazySystemDLL("gdi32.dll")
	procRegisterClassExW = user32.NewProc("RegisterClassExW")
	procCreateWindowExW  = user32.NewProc("CreateWindowExW")
	procDefWindowProcW   = user32.NewProc("DefWindowProcW")
	procDestroyWindow    = user32.NewProc("DestroyWindow")
	procShowWindow       = user32.NewProc("ShowWindow")
	procGetMessageW      = user32.NewProc("GetMessageW")
	procTranslateMessage = user32.NewProc("TranslateMessage")
	procDispatchMessageW = user32.NewProc("DispatchMessageW")
	procPostMessageW     = user32.NewProc("PostMessageW")
	procPostQuitMessage  = user32.NewProc("PostQuitMessage")
	procAdjustWindowRect = user32.NewProc("AdjustWindowRectEx")
	procLoadCursorW      = user32.NewProc("LoadCursorW")
	procGetStockObject   = gdi32.NewProc("GetStockObject")
)

const (
	wmDestroy       = 0x0002
	wmClose         = 0x0010
	wmGetMinMaxInfo = 0x0024
	wmSizing        = 0x0214
	wmApp           = 0x8000
	wmQuitPreview   = wmApp + 1 // from Destroy: tear the window down

	wsOverlapped  = 0x00000000
	wsCaption     = 0x00C00000
	wsSysMenu     = 0x00080000
	wsThickFrame  = 0x00040000
	wsMinimizeBox = 0x00020000
	wsExTopmost   = 0x00000008
	cwUseDefault  = ^uintptr(0x7FFFFFFF) // (int)0x80000000
	swHide        = 0
	swShowNA      = 8
	idcArrow      = 32512
	blackBrush    = 4

	previewStyle   = wsOverlapped | wsCaption | wsSysMenu | wsThickFrame | wsMinimizeBox
	previewExStyle = wsExTopmost

	// WM_SIZING edges.
	wmszLeft, wmszRight, wmszTop, wmszTopLeft, wmszTopRight = 1, 2, 3, 4, 5
	wmszBottom, wmszBottomLeft, wmszBottomRight             = 6, 7, 8
)

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       windows.Handle
	Cursor     windows.Handle
	Background windows.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSm     windows.Handle
}

type point struct{ X, Y int32 }

type minMaxInfo struct {
	Reserved, MaxSize, MaxPosition, MinTrackSize, MaxTrackSize point
}

type winRect struct{ Left, Top, Right, Bottom int32 }

type msg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
	Private uint32
}

var (
	classOnce sync.Once
	classErr  error
	className = windows.StringToUTF16Ptr("BellionSharePreview")
	// windowsByHwnd lets the shared window procedure find its window.
	windowsMu     sync.Mutex
	windowsByHwnd = map[uintptr]*winPreview{}
)

func registerClass() error {
	classOnce.Do(func() {
		cursor, _, _ := procLoadCursorW.Call(0, idcArrow)
		brush, _, _ := procGetStockObject.Call(blackBrush)
		wc := wndClassEx{WndProc: syscall.NewCallback(previewProc), ClassName: className,
			Cursor: windows.Handle(cursor), Background: windows.Handle(brush)}
		wc.Size = uint32(unsafe.Sizeof(wc))
		if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
			classErr = err
		}
	})
	return classErr
}

// frame is how much the window border adds to the client size.
func frame() (int32, int32) {
	r := winRect{0, 0, 100, 100}
	procAdjustWindowRect.Call(uintptr(unsafe.Pointer(&r)), previewStyle, 0, previewExStyle)
	return r.Right - r.Left - 100, r.Bottom - r.Top - 100
}

func previewProc(hwnd, m, wp, lp uintptr) uintptr {
	switch m {
	case wmGetMinMaxInfo:
		fw, fh := frame()
		mi := (*minMaxInfo)(unsafe.Pointer(lp))
		mi.MinTrackSize = point{previewMinW + fw, previewMinH + fh}
		mi.MaxTrackSize = point{PreviewW + fw, PreviewH + fh}
		return 0
	case wmSizing: // keep 16:9 while dragging
		fw, fh := frame()
		r := (*winRect)(unsafe.Pointer(lp))
		byHeight := wp == wmszTop || wp == wmszBottom
		w, h := fitAspect(int(r.Right-r.Left-fw), int(r.Bottom-r.Top-fh), byHeight)
		switch wp {
		case wmszLeft, wmszTopLeft, wmszBottomLeft:
			r.Left = r.Right - int32(w) - fw
		default:
			r.Right = r.Left + int32(w) + fw
		}
		switch wp {
		case wmszTop, wmszTopLeft, wmszTopRight:
			r.Top = r.Bottom - int32(h) - fh
		default:
			r.Bottom = r.Top + int32(h) + fh
		}
		return 1
	case wmClose: // the user closed it: hide now, Destroy tears it down
		procShowWindow.Call(hwnd, swHide)
		windowsMu.Lock()
		w := windowsByHwnd[hwnd]
		windowsMu.Unlock()
		if w != nil {
			w.closeOnce.Do(func() { close(w.closed) })
		}
		return 0
	case wmQuitPreview:
		procDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, m, wp, lp)
	return r
}

type winPreview struct {
	hwnd      uintptr
	closed    chan struct{}
	closeOnce sync.Once
	gone      chan struct{} // the message loop ended
	destroy   sync.Once
}

// openPreviewWindow creates the window on its own locked thread, which then
// pumps its messages until Destroy.
func openPreviewWindow() (previewWindow, error) {
	if err := registerClass(); err != nil {
		return nil, err
	}
	w := &winPreview{closed: make(chan struct{}), gone: make(chan struct{})}
	ready := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer close(w.gone)
		fw, fh := frame()
		title := windows.StringToUTF16Ptr(previewTitle)
		hwnd, _, err := procCreateWindowExW.Call(previewExStyle, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(title)),
			previewStyle, cwUseDefault, cwUseDefault, uintptr(previewStartW+fw), uintptr(previewStartH+fh), 0, 0, 0, 0)
		if hwnd == 0 {
			ready <- err
			return
		}
		w.hwnd = hwnd
		windowsMu.Lock()
		windowsByHwnd[hwnd] = w
		windowsMu.Unlock()
		procShowWindow.Call(hwnd, swShowNA) // don't take focus from the game
		ready <- nil
		var m msg
		for {
			r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
			if r == 0 || int32(r) == -1 {
				break
			}
			procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		}
		windowsMu.Lock()
		delete(windowsByHwnd, hwnd)
		windowsMu.Unlock()
	}()
	if err := <-ready; err != nil {
		return nil, errors.Join(errors.New("create preview window"), err)
	}
	return w, nil
}

func (w *winPreview) Handle() uintptr         { return w.hwnd }
func (w *winPreview) Closed() <-chan struct{} { return w.closed }

func (w *winPreview) Destroy() {
	w.destroy.Do(func() {
		procPostMessageW.Call(w.hwnd, wmQuitPreview, 0, 0)
		<-w.gone
	})
}
