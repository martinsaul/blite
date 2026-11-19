package capture

import (
	"encoding/binary"
	"testing"
)

func TestDecodeNetWMIcon(t *testing.T) {
	var v []byte
	put := func(n uint32) { v = binary.LittleEndian.AppendUint32(v, n) }
	icon := func(w, h int, argb uint32) {
		put(uint32(w))
		put(uint32(h))
		for range w * h {
			put(argb)
		}
	}
	icon(16, 16, 0xff0000ff)
	icon(48, 48, 0x80ff0000)
	icon(32, 32, 0xff00ff00)
	img := decodeNetWMIcon(v)
	if img == nil || img.Bounds().Dx() != 32 {
		t.Fatalf("want the 32 px icon, got %v", img)
	}
	if r, g, b, a := img.At(0, 0).RGBA(); r != 0 || g != 0xffff || b != 0 || a != 0xffff {
		t.Fatalf("pixel = %x %x %x %x, want opaque green", r, g, b, a)
	}
	if decodeNetWMIcon(v[:20]) != nil { // truncated
		t.Fatal("decoded a truncated icon")
	}
	// Cut off in the second icon: the first one is still good.
	if img := decodeNetWMIcon(v[:(2+16*16+2+100)*4]); img == nil || img.Bounds().Dx() != 16 {
		t.Fatalf("want the 16 px icon before the cut, got %v", img)
	}
}
