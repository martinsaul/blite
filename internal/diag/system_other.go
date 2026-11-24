//go:build !windows

package diag

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"time"
)

// platformSystem reads /proc and /etc/os-release (Linux). Displays, GPUs
// and their settings are measured on Windows only.
func platformSystem(s *System) error {
	s.OS = "unknown"
	if b, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(l, "PRETTY_NAME="); ok {
				s.OS = strings.Trim(v, `"`)
			}
		}
	}
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		s.OS += ", kernel " + strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if k, v, ok := strings.Cut(l, ":"); ok && strings.TrimSpace(k) == "model name" {
				s.CPU = strings.TrimSpace(v)
				break
			}
		}
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if f := strings.Fields(l); len(f) >= 2 && f[0] == "MemTotal:" {
				kb, _ := strconv.ParseFloat(f[1], 64)
				s.RAMGB = kb / (1 << 20)
			}
		}
	}
	for _, k := range []string{"XDG_SESSION_TYPE", "XDG_CURRENT_DESKTOP", "DISPLAY", "WAYLAND_DISPLAY"} {
		if v := os.Getenv(k); v != "" {
			s.Settings[k] = v
		}
	}
	return nil
}

func displays() ([]Display, error) { return nil, errNotHere }

// SetDPIAware is Windows-only.
func SetDPIAware() {}

// cpuTimes is the machine's idle and total CPU time so far (/proc/stat).
func cpuTimes() (idle, total time.Duration, ok bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0, 0, false
	}
	fs := strings.Fields(sc.Text())
	if len(fs) < 5 || fs[0] != "cpu" {
		return 0, 0, false
	}
	var t int64
	for i, v := range fs[1:] {
		n, _ := strconv.ParseInt(v, 10, 64)
		if i < 8 { // guest time is already in user
			t += n
		}
		if i == 3 || i == 4 { // idle, iowait
			idle += time.Duration(n) * 10 * time.Millisecond
		}
	}
	return idle, time.Duration(t) * 10 * time.Millisecond, true
}
