package main

import (
	"syscall"
	"time"
)

// cpuTime is the process's user+system CPU time.
func cpuTime() time.Duration {
	var c, e, k, u syscall.Filetime
	h, err := syscall.GetCurrentProcess()
	if err != nil || syscall.GetProcessTimes(h, &c, &e, &k, &u) != nil {
		return 0
	}
	// Process times are durations in 100 ns ticks (not epoch times, so no Nanoseconds()).
	ticks := func(f syscall.Filetime) int64 { return int64(f.HighDateTime)<<32 | int64(f.LowDateTime) }
	return time.Duration((ticks(k) + ticks(u)) * 100)
}
