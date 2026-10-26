//go:build windows

package load

import (
	"syscall"
	"time"
)

func processCPU() (time.Duration, bool) {
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0, false
	}
	var c, e, k, u syscall.Filetime
	if err := syscall.GetProcessTimes(h, &c, &e, &k, &u); err != nil {
		return 0, false
	}
	ft := func(f syscall.Filetime) time.Duration {
		return time.Duration((int64(f.HighDateTime)<<32 | int64(f.LowDateTime)) * 100)
	}
	return ft(k) + ft(u), true
}
