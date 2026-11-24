package nui

import (
	"image"
	"testing"
)

// Names in any script come out drawn: white (emoji in colour), as high as
// the line, wider for more text.
func TestDrawVideoText(t *testing.T) {
	if drawVideoText("", 20) != nil {
		t.Fatal("drew no text")
	}
	ink := func(img *image.NRGBA, white bool) (n int) {
		for i := 3; i < len(img.Pix); i += 4 {
			if img.Pix[i] > 128 {
				n++
				if white && img.Pix[i-3] < 200 {
					t.Fatalf("not white: %v", img.Pix[i-3:i+1])
				}
			}
		}
		return n
	}
	short := drawVideoText("Bob", 20)
	if short == nil || short.Rect.Dy() < 20 || short.Rect.Dy() > 32 || ink(short, true) == 0 {
		t.Fatalf("Bob: %v", short)
	}
	for _, s := range []string{"Bob#2926 (muted)", "李小龙", "مرحبا", "Zoë 😀"} {
		img := drawVideoText(s, 20)
		if img == nil || img.Rect.Dy() != short.Rect.Dy() || ink(img, s != "Zoë 😀") == 0 {
			t.Errorf("%q: %v", s, img)
		}
	}
	if big := drawVideoText("Bob", 40); big.Rect.Dx() < 2*short.Rect.Dx()-2 {
		t.Errorf("40 px %v, 20 px %v", big.Rect, short.Rect)
	}
	done := make(chan bool)
	for range 4 { // from any goroutine
		go func() { done <- drawVideoText("Carol#0815", 18) != nil }()
	}
	for range 4 {
		if !<-done {
			t.Error("not drawn")
		}
	}
}
