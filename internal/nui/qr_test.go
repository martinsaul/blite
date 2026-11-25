package nui

import (
	"image"
	"slices"
	"testing"

	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/test"
	qrcode "github.com/skip2/go-qrcode"

	"blite/internal/i18n"
)

// TestQRImage: the image is the link's QR code (medium error correction),
// module for module, on white with a four-module quiet zone.
func TestQRImage(t *testing.T) {
	const link = "https://share.example/j/K7PQ2M4X#pw"
	const px = 8
	img, err := qrImage(link, px)
	if err != nil {
		t.Fatal(err)
	}
	q, err := qrcode.New(link, qrcode.Medium)
	if err != nil {
		t.Fatal(err)
	}
	bits := q.Bitmap()
	n := len(bits)
	if want := (17 + 4*q.VersionNumber + 8) * px; n*px != want || img.Bounds() != image.Rect(0, 0, want, want) {
		t.Fatalf("bounds %v, want %dx%d (version %d)", img.Bounds(), want, want, q.VersionNumber)
	}
	if n*px < qrSide {
		t.Errorf("%d px drawn for %d px on screen: modules would blur", n*px, qrSide)
	}
	g := img.(*image.Gray)
	dark := func(x, y int) bool { return g.GrayAt(x, y).Y < 0x80 }
	for y := range n {
		for x := range n {
			// Every pixel of the module, not just one.
			for _, d := range [][2]int{{0, 0}, {px - 1, 0}, {0, px - 1}, {px - 1, px - 1}, {px / 2, px / 2}} {
				if got := dark(x*px+d[0], y*px+d[1]); got != bits[y][x] {
					t.Fatalf("module %d,%d: dark=%v, want %v", x, y, got, bits[y][x])
				}
			}
			if (x < 4 || y < 4 || x >= n-4 || y >= n-4) && bits[y][x] {
				t.Fatalf("module %d,%d is in the quiet zone but dark", x, y)
			}
		}
	}
	// The top left finder pattern starts right after the quiet zone.
	if !dark(4*px, 4*px) || !dark(10*px, 10*px) || dark(5*px, 5*px) || !dark(6*px, 6*px) {
		t.Error("no finder pattern at the top left")
	}
	// A different link gives a different code.
	other, _ := qrcode.New(link+"x", qrcode.Medium)
	if slices.EqualFunc(other.Bitmap(), bits, slices.Equal) {
		t.Error("two links, one code")
	}
}

// TestQRDialog: in a call the footer's QR button opens a dialog with the
// link; without a session it does nothing.
func TestQRDialog(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	i18n.SetPreference("en")
	defer i18n.SetPreference("")
	a := test.NewApp()
	defer a.Quit()
	a.Settings().SetTheme(themeFor())

	st := demoState(true)
	st.Session.Link = "https://share.example/j/K7PQ2M4X#pw"
	u := &UI{a: &fakeBackend{st: st}, fa: a, win: test.NewWindow(nil)}
	u.view = container.NewStack()
	u.win.SetContent(u.view)
	u.refresh()
	defer func() { u.closed.Store(true); u.win.Close() }()
	if u.call == nil || u.call.qr == nil || u.call.qr.meaning == "" {
		t.Fatal("no QR button with a tooltip in the call view")
	}
	u.call.qr.Tapped(nil)
	ov := u.win.Canvas().Overlays().List()
	if len(ov) == 0 {
		t.Fatal("tapping the QR button opened nothing")
	}
	if got := texts(ov[len(ov)-1]); !slices.Contains(got, st.Session.Link) {
		t.Errorf("dialog texts %q lack the link", got)
	}

	u2 := &UI{a: &fakeBackend{st: demoState(false)}, fa: a, win: test.NewWindow(nil)}
	defer func() { u2.closed.Store(true); u2.win.Close() }()
	u2.qrDialog()
	if n := len(u2.win.Canvas().Overlays().List()); n != 0 {
		t.Errorf("without a session: %d overlays", n)
	}
}
