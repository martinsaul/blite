package main

import "testing"

func TestUpdateChannel(t *testing.T) {
	t.Setenv("BLITE_CHANNEL", "")
	defaultChannel = "beta"
	for _, c := range []struct {
		name, channel, pinned string
		set, relaunched       bool
		want                  string
		pin                   bool
	}{
		{"plain start", "beta", "", false, false, "beta", false},
		{"chosen by hand", "stable", "", true, false, "stable", true},
		{"old build relaunch passes its stamp", "stable", "", true, true, "beta", false},
		{"pinned survives relaunch", "beta", "stable", false, true, "stable", true},
	} {
		got, pin := updateChannel(c.channel, c.pinned, c.set, c.relaunched)
		if got != c.want || pin != c.pin {
			t.Errorf("%s: got %q pin=%v, want %q pin=%v", c.name, got, pin, c.want, c.pin)
		}
	}
}
