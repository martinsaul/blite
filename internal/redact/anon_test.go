package redact

import (
	"os"
	"strings"
	"testing"
)

func TestAnonymizer(t *testing.T) {
	home, _ := os.UserHomeDir()
	host, _ := os.Hostname()
	a := NewAnonymizer("Quiet Otter", "Desk Sharer")
	in := `time=2026-11-25T10:00:00Z level=INFO msg=joined peer="Desk Sharer" from=qR9nPK295W45 room=011R2RW7 ` +
		`server=share.example.org name=alice#7F2A window="Budget 2027.xlsx - Excel" fps=30 res=1080
level=INFO msg="hello from Quiet Otter" file=` + home + `/x.log host=` + host + `
level=INFO msg=again peer="Desk Sharer" room=011R2RW7
{"level":"INFO","msg":"json","peer":"bob#9A1C","room":"011R2RW7","fps":"30"}`
	out := a.Text(in)
	for _, gone := range []string{"Desk Sharer", "Quiet Otter", "011R2RW7", "share.example.org", "alice", "Budget", home, "host=" + host} {
		if strings.Contains(out, gone) {
			t.Errorf("%q left in:\n%s", gone, out)
		}
	}
	if strings.Contains(out, `"bob#`) || !strings.Contains(out, `"room":"room1"`) || !strings.Contains(out, `#9A1C"`) {
		t.Errorf("JSON attributes not anonymized:\n%s", out)
	}
	for _, kept := range []string{"from=qR9nPK295W45", "#7F2A", "fps=30", "res=1080", "file=~/x.log", "msg=joined"} {
		if !strings.Contains(out, kept) {
			t.Errorf("%q missing from:\n%s", kept, out)
		}
	}
	// One value, one placeholder, across lines.
	if strings.Count(out, "room=room1") != 2 || strings.Count(out, "room1") != 3 || !strings.Contains(out, `peer=name2`) {
		t.Errorf("placeholders not stable:\n%s", out)
	}
}
