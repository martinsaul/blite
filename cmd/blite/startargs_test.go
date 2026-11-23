package main

import (
	"reflect"
	"testing"
)

// The build an update at start relaunches gets this start's flags, minus
// -pin-channel (the updater passes its own).
func TestStartArgs(t *testing.T) {
	for _, c := range []struct{ in, want []string }{
		{nil, nil},
		{[]string{"-background"}, []string{"-background"}},
		{[]string{"-pin-channel", "beta", "-background"}, []string{"-background"}},
		{[]string{"--pin-channel=beta", "https://x/j/AB#k"}, []string{"https://x/j/AB#k"}},
		{[]string{"-link", "pin-channel"}, []string{"-link", "pin-channel"}},
	} {
		if got := startArgs(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("startArgs(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
