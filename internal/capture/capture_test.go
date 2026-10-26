package capture

import "testing"

func TestParse(t *testing.T) {
	r, err := ParseMonitor("1920,0,2560,1440")
	if err != nil || r != (Region{1920, 0, 2560, 1440}) {
		t.Fatalf("%v %v", r, err)
	}
	for _, bad := range []string{"", "1,2,3", "0,0,0,10", "a,b,c,d", "0,0,10,10 ! fakesink"} {
		if _, err := ParseMonitor(bad); err == nil {
			t.Errorf("accepted monitor %q", bad)
		}
	}
	if id, err := ParseWindow("0x3a00007"); err != nil || id != 0x3a00007 {
		t.Fatalf("%v %v", id, err)
	}
	for _, bad := range []string{"", "0", "12 ! fakesink", "-1"} {
		if _, err := ParseWindow(bad); err == nil {
			t.Errorf("accepted window %q", bad)
		}
	}
}
