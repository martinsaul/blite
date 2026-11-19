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

func TestLabel(t *testing.T) {
	for _, c := range []struct {
		t    Target
		want string
	}{
		{Target{Kind: "window", Name: "Notes - Editor", App: "editor", Source: "window:0x307a2"}, "Notes - Editor"},
		{Target{Kind: "window", Name: "  ", App: "Discord", Source: "window:0x307a2"}, "Discord"},
		{Target{Kind: "window", Source: "window:0x307a2"}, ""}, // never the handle
		{Target{Kind: "monitor", Name: "Display 1 (1920x1080)", Source: "monitor:0x10001"}, "Display 1 (1920x1080)"},
	} {
		if got := c.t.Label(); got != c.want {
			t.Errorf("%+v: Label() = %q, want %q", c.t, got, c.want)
		}
	}
}
