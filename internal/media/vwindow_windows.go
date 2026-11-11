package media

import (
	"errors"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procSetWindowTextW  = user32.NewProc("SetWindowTextW")
	procGetCursorPos    = user32.NewProc("GetCursorPos")
	procWindowFromPoint = user32.NewProc("WindowFromPoint")
	procGetAncestor     = user32.NewProc("GetAncestor")
	procIsIconic        = user32.NewProc("IsIconic")
)

const (
	wsOverlappedWindow = 0x00CF0000
	wmQuitVideo        = wmApp + 2
	gaRoot             = 2
)

var (
	videoClassOnce sync.Once
	videoClassErr  error
	videoClassName = windows.StringToUTF16Ptr("BellionShareVideo")
	videoWinsMu    sync.Mutex
	videoWins      = map[uintptr]*winVideo{}
)

func registerVideoClass() error {
	videoClassOnce.Do(func() {
		cursor, _, _ := procLoadCursorW.Call(0, idcArrow)
		brush, _, _ := procGetStockObject.Call(blackBrush)
		wc := wndClassEx{WndProc: syscall.NewCallback(videoProc), ClassName: videoClassName,
			Cursor: windows.Handle(cursor), Background: windows.Handle(brush)}
		wc.Size = uint32(unsafe.Sizeof(wc))
		if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
			videoClassErr = err
		}
	})
	return videoClassErr
}

func videoProc(hwnd, m, wp, lp uintptr) uintptr {
	switch m {
	case wmClose: // the user closed it: hide now, the owner destroys it
		procShowWindow.Call(hwnd, swHide)
		videoWinsMu.Lock()
		w := videoWins[hwnd]
		videoWinsMu.Unlock()
		if w != nil {
			w.closeOnce.Do(func() { close(w.closed) })
		}
		return 0
	case wmQuitVideo:
		procDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, m, wp, lp)
	return r
}

type winVideo struct {
	hwnd      uintptr
	closed    chan struct{}
	closeOnce sync.Once
	gone      chan struct{}
	destroy   sync.Once
}

func openVideoWindow(title string, cw, ch int) (Window, error) {
	if err := registerVideoClass(); err != nil {
		return nil, err
	}
	w := &winVideo{closed: make(chan struct{}), gone: make(chan struct{})}
	ready := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer close(w.gone)
		r := winRect{0, 0, int32(cw), int32(ch)}
		procAdjustWindowRect.Call(uintptr(unsafe.Pointer(&r)), wsOverlappedWindow, 0, 0)
		t := windows.StringToUTF16Ptr(title)
		hwnd, _, err := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(videoClassName)), uintptr(unsafe.Pointer(t)),
			wsOverlappedWindow, cwUseDefault, cwUseDefault, uintptr(r.Right-r.Left), uintptr(r.Bottom-r.Top), 0, 0, 0, 0)
		if hwnd == 0 {
			ready <- err
			return
		}
		w.hwnd = hwnd
		videoWinsMu.Lock()
		videoWins[hwnd] = w
		videoWinsMu.Unlock()
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
		videoWinsMu.Lock()
		delete(videoWins, hwnd)
		videoWinsMu.Unlock()
	}()
	if err := <-ready; err != nil {
		return nil, errors.Join(errors.New("create video window"), err)
	}
	return w, nil
}

func (w *winVideo) Handle() uintptr         { return w.hwnd }
func (w *winVideo) Closed() <-chan struct{} { return w.closed }

func (w *winVideo) SetTitle(title string) {
	procSetWindowTextW.Call(w.hwnd, uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(title))))
}

func (w *winVideo) PointerInside() bool {
	var p point
	if r, _, _ := procGetCursorPos.Call(uintptr(unsafe.Pointer(&p))); r == 0 {
		return true // unknown: don't hide the buttons
	}
	h, _, _ := procWindowFromPoint.Call(uintptr(uint32(p.X)) | uintptr(uint32(p.Y))<<32)
	if h == 0 {
		return false
	}
	root, _, _ := procGetAncestor.Call(h, gaRoot)
	return root == w.hwnd || h == w.hwnd
}

func (w *winVideo) Minimized() bool {
	if v, _, _ := procIsIconic.Call(w.hwnd); v != 0 {
		return true
	}
	v, _, _ := procIsWindowVisible.Call(w.hwnd)
	return v == 0
}

func (w *winVideo) Destroy() {
	w.destroy.Do(func() {
		procPostMessageW.Call(w.hwnd, wmQuitVideo, 0, 0)
		<-w.gone
	})
}
