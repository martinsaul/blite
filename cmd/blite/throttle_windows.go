package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var setProcessInformation = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetProcessInformation")

// noThrottle opts out of Windows' power throttling (EcoQoS: efficiency
// cores, low clocks) and timer coalescing, which Windows 11 applies to
// apps behind a fullscreen game: our decoding, drawing and capture must
// keep their pace there.
func noThrottle() error {
	const (
		processPowerThrottling = 4 // PROCESS_INFORMATION_CLASS
		executionSpeed         = 1 // PROCESS_POWER_THROTTLING_EXECUTION_SPEED
		ignoreTimerResolution  = 4 // PROCESS_POWER_THROTTLING_IGNORE_TIMER_RESOLUTION
	)
	s := struct{ version, control, state uint32 }{1, executionSpeed | ignoreTimerResolution, 0}
	if r, _, err := setProcessInformation.Call(uintptr(windows.CurrentProcess()), processPowerThrottling,
		uintptr(unsafe.Pointer(&s)), unsafe.Sizeof(s)); r == 0 {
		return err
	}
	return nil
}
