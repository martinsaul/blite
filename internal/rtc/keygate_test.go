package rtc

import "testing"

func TestKeyGate(t *testing.T) {
	idr := []byte{0, 0, 0, 1, 0x67, 1, 2, 0, 0, 1, 0x68, 3, 0, 0, 1, 0x65, 9}
	p := []byte{0, 0, 0, 1, 0x41, 9, 9}
	plis := 0
	g := &keyGate{pli: func() { plis++ }}
	if g.pass(p, 0) || plis != 1 {
		t.Fatalf("start: a P frame passed, or no PLI (%d)", plis)
	}
	if !g.pass(idr, 0) || !g.pass(p, 0) {
		t.Fatal("IDR and what follows must pass")
	}
	if g.pass(p, 3) {
		t.Fatal("a frame after lost packets passed")
	}
	if g.pass(p, 0) {
		t.Fatal("still waiting for a keyframe")
	}
	if plis != 1 { // the second loss came within pliInterval of the first PLI
		t.Fatalf("PLIs %d, want 1 (throttled)", plis)
	}
	if !g.pass(idr, 0) {
		t.Fatal("keyframe after loss")
	}
	if hasIDR(p) || !hasIDR(idr) {
		t.Fatal("hasIDR")
	}
}
