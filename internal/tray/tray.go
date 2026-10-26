// Package tray shows Blite's system-tray icon (StatusNotifierItem over D-Bus on
// Linux, the notification area on Windows).
package tray

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"runtime"

	"fyne.io/systray"
)

// Actions are the tray menu's callbacks.
type Actions struct {
	Open  func() // open the local UI
	Leave func() // leave the current session
	Quit  func()
}

// Run blocks the calling goroutine (it must be the main one on some platforms)
// until Quit. onReady runs once the icon exists.
func Run(a Actions, onReady func()) {
	systray.Run(func() {
		systray.SetIcon(icon())
		systray.SetTitle("Blite")
		systray.SetTooltip("Blite screen sharing")
		open := systray.AddMenuItem("Open Blite", "Create or join a session")
		leave := systray.AddMenuItem("Leave session", "Leave the current session")
		systray.AddSeparator()
		quit := systray.AddMenuItem("Quit", "Quit Blite")
		systray.SetOnTapped(a.Open) // left click opens the UI where supported
		go func() {
			for {
				select {
				case <-open.ClickedCh:
					a.Open()
				case <-leave.ClickedCh:
					a.Leave()
				case <-quit.ClickedCh:
					a.Quit()
					systray.Quit()
					return
				}
			}
		}()
		if onReady != nil {
			onReady()
		}
	}, nil)
}

// Quit removes the icon and makes Run return.
func Quit() { systray.Quit() }

// icon draws a 32x32 rounded "screen" glyph; Windows needs it wrapped in ICO.
func icon() []byte {
	const n = 32
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	blue := color.NRGBA{0x3b, 0x63, 0xe6, 0xff}
	white := color.NRGBA{0xff, 0xff, 0xff, 0xff}
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if roundedRect(x, y, 1, 1, 30, 30, 7) {
				img.Set(x, y, blue)
			}
			if roundedRect(x, y, 7, 9, 18, 12, 2) && !roundedRect(x, y, 9, 11, 14, 8, 1) {
				img.Set(x, y, white) // screen outline
			}
			if (x >= 14 && x <= 17 && y >= 21 && y <= 23) || (x >= 11 && x <= 20 && y == 24) {
				img.Set(x, y, white) // stand
			}
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	if runtime.GOOS == "windows" {
		return pngToICO(buf.Bytes(), n)
	}
	return buf.Bytes()
}

func roundedRect(x, y, x0, y0, w, h, r int) bool {
	if x < x0 || y < y0 || x >= x0+w || y >= y0+h {
		return false
	}
	cx, cy := x, y
	switch {
	case x < x0+r:
		cx = x0 + r
	case x >= x0+w-r:
		cx = x0 + w - r - 1
	}
	switch {
	case y < y0+r:
		cy = y0 + r
	case y >= y0+h-r:
		cy = y0 + h - r - 1
	}
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= r*r
}

// pngToICO wraps one PNG image in an ICO container (Vista+ reads PNG entries).
func pngToICO(p []byte, size int) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, [3]uint16{0, 1, 1}) // reserved, type=icon, count
	b.Write([]byte{byte(size), byte(size), 0, 0})             // w, h, palette, reserved
	binary.Write(&b, binary.LittleEndian, [2]uint16{1, 32})   // planes, bpp
	binary.Write(&b, binary.LittleEndian, [2]uint32{uint32(len(p)), 22})
	b.Write(p)
	return b.Bytes()
}
