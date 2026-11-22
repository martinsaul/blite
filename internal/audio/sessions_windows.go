package audio

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The audio sessions of the default playback device (Core Audio,
// IAudioSessionManager2): which processes play, or have played, sound there.
// Whole-system sharing uses them to leave voice chat apps out, as process
// loopback can exclude only one process tree (ours).

var (
	ole32                = windows.NewLazySystemDLL("ole32.dll")
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")

	clsidMMDeviceEnumerator = windows.GUID{Data1: 0xBCDE0395, Data2: 0xE52F, Data3: 0x467C, Data4: [8]byte{0x8E, 0x3D, 0xC4, 0x57, 0x92, 0x91, 0x69, 0x2E}}
	iidIMMDeviceEnumerator  = windows.GUID{Data1: 0xA95664D2, Data2: 0x9614, Data3: 0x4F35, Data4: [8]byte{0xA7, 0x46, 0xDE, 0x8D, 0xB6, 0x36, 0x17, 0xE6}}
	iidIAudioSessionManager = windows.GUID{Data1: 0x77AA99A0, Data2: 0x1BD6, Data3: 0x484F, Data4: [8]byte{0x8B, 0xC7, 0x2C, 0x65, 0x4C, 0x9A, 0x9B, 0x6F}}
	iidIAudioSessionCtl2    = windows.GUID{Data1: 0xBFB7FF88, Data2: 0x7239, Data3: 0x4FC9, Data4: [8]byte{0x8F, 0xA2, 0x07, 0xC9, 0x50, 0xBE, 0x9C, 0x6D}}
)

// Vtable slots (IUnknown: 0 QueryInterface, 1 AddRef, 2 Release).
const (
	slotQueryInterface       = 0
	slotRelease              = 2
	slotGetDefaultEndpoint   = 4  // IMMDeviceEnumerator
	slotActivate             = 3  // IMMDevice
	slotGetSessionEnumerator = 5  // IAudioSessionManager2
	slotGetCount             = 3  // IAudioSessionEnumerator
	slotGetSession           = 4  // IAudioSessionEnumerator
	slotGetState             = 3  // IAudioSessionControl
	slotGetProcessID         = 14 // IAudioSessionControl2
	slotIsSystemSounds       = 15 // IAudioSessionControl2

	clsctxAll       = 0x17
	eRender         = 0
	eConsole        = 0
	sessionExpired  = 2
	coinitMulti     = 0x0
	rpcEChangedMode = 0x80010106
)

// call invokes vtable slot of COM object obj; it returns the HRESULT.
func call(obj unsafe.Pointer, slot int, args ...uintptr) uint32 {
	vtbl := *(*unsafe.Pointer)(obj)
	fn := *(*uintptr)(unsafe.Add(vtbl, slot*int(unsafe.Sizeof(uintptr(0)))))
	r, _, _ := syscall.SyscallN(fn, append([]uintptr{uintptr(obj)}, args...)...)
	return uint32(r)
}

func release(obj unsafe.Pointer) {
	if obj != nil {
		call(obj, slotRelease)
	}
}

// sessionPIDs returns the processes with a live (not expired) audio
// session on the default playback device, system sounds left out.
func sessionPIDs() ([]int, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// S_FALSE (already initialized) needs its CoUninitialize too;
	// RPC_E_CHANGED_MODE (an apartment thread) works without one.
	switch err := windows.CoInitializeEx(0, coinitMulti); {
	case err == nil, err == syscall.Errno(1):
		defer windows.CoUninitialize()
	case err != syscall.Errno(rpcEChangedMode):
		return nil, fmt.Errorf("CoInitializeEx: %w", err)
	}
	var enum unsafe.Pointer
	if r, _, _ := procCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsidMMDeviceEnumerator)), 0, clsctxAll,
		uintptr(unsafe.Pointer(&iidIMMDeviceEnumerator)), uintptr(unsafe.Pointer(&enum))); r != 0 {
		return nil, fmt.Errorf("MMDeviceEnumerator: %#x", uint32(r))
	}
	defer release(enum)
	var dev unsafe.Pointer
	if hr := call(enum, slotGetDefaultEndpoint, eRender, eConsole, uintptr(unsafe.Pointer(&dev))); hr != 0 {
		return nil, fmt.Errorf("GetDefaultAudioEndpoint: %#x", hr)
	}
	defer release(dev)
	var mgr unsafe.Pointer
	if hr := call(dev, slotActivate, uintptr(unsafe.Pointer(&iidIAudioSessionManager)), clsctxAll, 0, uintptr(unsafe.Pointer(&mgr))); hr != 0 {
		return nil, fmt.Errorf("Activate(IAudioSessionManager2): %#x", hr)
	}
	defer release(mgr)
	var list unsafe.Pointer
	if hr := call(mgr, slotGetSessionEnumerator, uintptr(unsafe.Pointer(&list))); hr != 0 {
		return nil, fmt.Errorf("GetSessionEnumerator: %#x", hr)
	}
	defer release(list)
	var n int32
	if hr := call(list, slotGetCount, uintptr(unsafe.Pointer(&n))); hr != 0 {
		return nil, fmt.Errorf("GetCount: %#x", hr)
	}
	seen := map[int]bool{}
	var pids []int
	for i := int32(0); i < n; i++ {
		var ctl unsafe.Pointer
		if call(list, slotGetSession, uintptr(i), uintptr(unsafe.Pointer(&ctl))) != 0 || ctl == nil {
			continue
		}
		var ctl2 unsafe.Pointer
		hr := call(ctl, slotQueryInterface, uintptr(unsafe.Pointer(&iidIAudioSessionCtl2)), uintptr(unsafe.Pointer(&ctl2)))
		release(ctl)
		if hr != 0 || ctl2 == nil {
			continue
		}
		var state int32
		var pid uint32
		okState := call(ctl2, slotGetState, uintptr(unsafe.Pointer(&state))) == 0
		okPID := call(ctl2, slotGetProcessID, uintptr(unsafe.Pointer(&pid))) == 0
		system := call(ctl2, slotIsSystemSounds) == 0 // S_OK: it is; S_FALSE: it isn't
		release(ctl2)
		if !okState || !okPID || system || state == sessionExpired || pid == 0 || seen[int(pid)] {
			continue
		}
		seen[int(pid)] = true
		pids = append(pids, int(pid))
	}
	return pids, nil
}
