package nui

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32                 = windows.NewLazySystemDLL("user32.dll")
	procGetWindowPlacement = user32.NewProc("GetWindowPlacement")
	procSetWindowPlacement = user32.NewProc("SetWindowPlacement")
)

// windowPlacement is WINDOWPLACEMENT. rcNormalPosition is the restored
// window's rectangle in workspace coordinates; SetWindowPlacement moves a
// window that would be off every screen back onto one.
type windowPlacement struct {
	length           uint32
	flags            uint32
	showCmd          uint32
	ptMinPosition    [2]int32
	ptMaxPosition    [2]int32
	rcNormalPosition [4]int32
}

const (
	swShowNormal    = 1
	swShowMinimized = 2
	swShowMaximized = 3
)

func getWinGeom(hwnd uintptr) (winGeom, bool) {
	if hwnd == 0 {
		return winGeom{}, false
	}
	wp := windowPlacement{length: uint32(unsafe.Sizeof(windowPlacement{}))}
	if r, _, _ := procGetWindowPlacement.Call(hwnd, uintptr(unsafe.Pointer(&wp))); r == 0 {
		return winGeom{}, false
	}
	rc := wp.rcNormalPosition
	return winGeom{
		X: int(rc[0]), Y: int(rc[1]), W: int(rc[2] - rc[0]), H: int(rc[3] - rc[1]),
		Maximized: wp.showCmd == swShowMaximized,
	}, true
}

func setWinGeom(hwnd uintptr, g winGeom) {
	if hwnd == 0 {
		return
	}
	wp := windowPlacement{length: uint32(unsafe.Sizeof(windowPlacement{}))}
	if r, _, _ := procGetWindowPlacement.Call(hwnd, uintptr(unsafe.Pointer(&wp))); r == 0 {
		return
	}
	wp.rcNormalPosition = [4]int32{int32(g.X), int32(g.Y), int32(g.X + g.W), int32(g.Y + g.H)}
	if g.Maximized {
		wp.showCmd = swShowMaximized
	} else if wp.showCmd == swShowMinimized || wp.showCmd == 0 {
		wp.showCmd = swShowNormal
	}
	procSetWindowPlacement.Call(hwnd, uintptr(unsafe.Pointer(&wp)))
}
