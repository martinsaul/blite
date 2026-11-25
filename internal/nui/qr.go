package nui

import (
	"image"
	"image/color"
	"log/slog"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	qrcode "github.com/skip2/go-qrcode"

	"blite/internal/i18n"
)

// qrSide is the QR code's size on screen: big enough for a phone to scan
// from arm's length.
const qrSide = 280

// qrImage draws text as a QR code (medium error correction): dark modules
// on white, with the standard four-module quiet zone, each module px pixels
// square so it stays crisp when shown at that size.
func qrImage(text string, px int) (image.Image, error) {
	q, err := qrcode.New(text, qrcode.Medium)
	if err != nil {
		return nil, err
	}
	bits := q.Bitmap() // includes the quiet zone
	n := len(bits)
	img := image.NewGray(image.Rect(0, 0, n*px, n*px))
	for i := range img.Pix {
		img.Pix[i] = 0xFF
	}
	dark := color.Gray{Y: 0x10}
	for y, row := range bits {
		for x, on := range row {
			if !on {
				continue
			}
			for dy := range px {
				for dx := range px {
					img.SetGray(x*px+dx, y*px+dy, dark)
				}
			}
		}
	}
	return img, nil
}

// qrDialog shows the room's link as a QR code, so a phone can join by
// scanning it.
func (u *UI) qrDialog() {
	s := u.st.Session
	if s == nil || s.Link == "" {
		return
	}
	img, err := qrImage(s.Link, 8)
	if err != nil {
		u.showErr(err)
		return
	}
	slog.Info("qr dialog opened", "room", s.Room)

	code := canvas.NewImageFromImage(img)
	code.FillMode = canvas.ImageFillContain
	code.ScaleMode = canvas.ImageScalePixels // nearest neighbour: sharp module edges
	code.SetMinSize(fyne.NewSquareSize(qrSide))

	hint := caption(i18n.L("Scan with the Bellion Share app or your phone's camera."))
	hint.Alignment = fyne.TextAlignCenter

	link := widget.NewLabel(s.Link)
	link.TextStyle.Monospace = true
	link.Wrapping = fyne.TextWrapBreak
	link.Alignment = fyne.TextAlignCenter
	link.Selectable = true

	copyBtn := widget.NewButtonWithIcon(i18n.L("Copy link"), theme.ContentCopyIcon(), u.copyLink)

	content := container.NewVBox(
		container.NewCenter(code),
		hint,
		link,
		container.NewCenter(copyBtn),
	)
	d := dialog.NewCustom(i18n.L("Join from a phone"), i18n.L("Close"), content, u.win)
	d.Resize(fyne.NewSize(420, 0))
	d.Show()
}
