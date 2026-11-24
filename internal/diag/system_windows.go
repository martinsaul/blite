package diag

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	kernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetSystemTimes       = kernel32.NewProc("GetSystemTimes")
	dxgi                     = windows.NewLazySystemDLL("dxgi.dll")
	procCreateDXGIFactory1   = dxgi.NewProc("CreateDXGIFactory1")
)

// displayClass is the registry class of display adapters.
const displayClass = `SYSTEM\CurrentControlSet\Control\Class\{4d36e968-e325-11ce-bfc1-08002be10318}`

func platformSystem(s *System) error {
	var errs []string
	s.OS = windowsVersion()
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DESCRIPTION\System\CentralProcessor\0`, registry.QUERY_VALUE); err == nil {
		v, _, _ := k.GetStringValue("ProcessorNameString")
		s.CPU = strings.TrimSpace(v)
		k.Close()
	}
	var ms struct {
		Length, Load                        uint32
		Total, Avail, TotalPF, AvailPF      uint64
		TotalVirt, AvailVirt, AvailExtended uint64
	}
	ms.Length = uint32(unsafe.Sizeof(ms))
	if ok, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms))); ok != 0 {
		s.RAMGB = float64(ms.Total) / (1 << 30)
	} else {
		errs = append(errs, "memory: "+err.Error())
	}

	reg := registryGPUs()
	dx, err := dxgiAdapters()
	if err != nil {
		errs = append(errs, "DXGI: "+err.Error())
	}
	// DXGI's adapters first (in its order), with the driver from the
	// registry; then registry adapters DXGI didn't list (disabled ones).
	used := map[int]bool{}
	for _, g := range dx {
		for i, rg := range reg {
			if !used[i] && strings.EqualFold(rg.Name, g.Name) {
				used[i] = true
				g.Driver, g.DriverDate, g.Provider = rg.Driver, rg.DriverDate, rg.Provider
				break
			}
		}
		s.GPUs = append(s.GPUs, g)
	}
	for i, rg := range reg {
		if !used[i] {
			s.GPUs = append(s.GPUs, rg)
		}
	}

	regDWORD := func(root registry.Key, path, name string) (uint64, bool) {
		k, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
		if err != nil {
			return 0, false
		}
		defer k.Close()
		v, _, err := k.GetIntegerValue(name)
		return v, err == nil
	}
	onOff := func(v uint64, ok bool, unset string) string {
		switch {
		case !ok:
			return unset
		case v == 0:
			return "off"
		}
		return "on"
	}
	if v, ok := regDWORD(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\GraphicsDrivers`, "HwSchMode"); ok {
		s.Settings["Hardware-accelerated GPU scheduling (HAGS)"] = map[uint64]string{1: "off", 2: "on"}[v] + fmt.Sprintf(" (HwSchMode=%d)", v)
	} else {
		s.Settings["Hardware-accelerated GPU scheduling (HAGS)"] = "not set (driver default)"
	}
	v, ok := regDWORD(registry.CURRENT_USER, `Software\Microsoft\GameBar`, "AutoGameModeEnabled")
	s.Settings["Game Mode"] = onOff(v, ok, "on (not set, the default)")
	v, ok = regDWORD(registry.CURRENT_USER, `System\GameConfigStore`, "GameDVR_Enabled")
	s.Settings["Game DVR / Game Bar captures"] = onOff(v, ok, "not set")
	v, ok = regDWORD(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\GameDVR`, "HistoricalCaptureEnabled")
	s.Settings["Game Bar background recording"] = onOff(v, ok, "not set (off)")
	if v, ok := regDWORD(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\Dwm`, "OverlayTestMode"); ok {
		s.Settings["DWM OverlayTestMode (5 = multiplane overlay off)"] = fmt.Sprint(v)
	}
	if k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\DirectX\UserGpuPreferences`, registry.QUERY_VALUE); err == nil {
		if g, _, err := k.GetStringValue("DirectXUserGlobalSettings"); err == nil {
			s.Settings["DirectX global settings (windowed game optimizations, auto HDR, VRR)"] = g
		}
		// Per-app GPU choice (Settings > Display > Graphics) for our programs.
		names, _ := k.ReadValueNames(-1)
		for _, n := range names {
			if strings.Contains(strings.ToLower(n), "blite") {
				g, _, _ := k.GetStringValue(n)
				s.Settings["GPU preference for "+n] = g + " (1 = power saving, 2 = high performance)"
			}
		}
		k.Close()
	}
	if out, err := runCmd(context.Background(), 10*time.Second, "powercfg", "/getactivescheme"); err == nil {
		if i := strings.Index(out, ":"); i >= 0 {
			out = strings.TrimSpace(out[i+1:])
		}
		s.Settings["Power plan"] = out
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// windowsVersion is e.g. "Windows 11 Pro 24H2 (build 26100.2605)".
func windowsVersion() string {
	v := windows.RtlGetVersion()
	name := "Windows"
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		return fmt.Sprintf("Windows %d.%d (build %d)", v.MajorVersion, v.MinorVersion, v.BuildNumber)
	}
	defer k.Close()
	if p, _, err := k.GetStringValue("ProductName"); err == nil {
		name = p
	}
	// Windows 11 still calls itself Windows 10 here.
	if v.BuildNumber >= 22000 {
		name = strings.Replace(name, "Windows 10", "Windows 11", 1)
	}
	if d, _, err := k.GetStringValue("DisplayVersion"); err == nil {
		name += " " + d
	}
	build := fmt.Sprint(v.BuildNumber)
	if ubr, _, err := k.GetIntegerValue("UBR"); err == nil {
		build += fmt.Sprintf(".%d", ubr)
	}
	return fmt.Sprintf("%s (build %s)", name, build)
}

// registryGPUs lists the display adapters with their drivers.
func registryGPUs() []GPU {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, displayClass, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return nil
	}
	defer k.Close()
	subs, _ := k.ReadSubKeyNames(-1)
	var out []GPU
	for _, sub := range subs {
		if len(sub) != 4 { // 0000, 0001, ...; not Properties or Configuration
			continue
		}
		sk, err := registry.OpenKey(k, sub, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		g := GPU{DXGI: -1}
		g.Name, _, _ = sk.GetStringValue("DriverDesc")
		g.Driver, _, _ = sk.GetStringValue("DriverVersion")
		g.DriverDate, _, _ = sk.GetStringValue("DriverDate")
		g.Provider, _, _ = sk.GetStringValue("ProviderName")
		if mem, _, err := sk.GetIntegerValue("HardwareInformation.qwMemorySize"); err == nil {
			g.VRAMMB = int(mem >> 20)
		}
		sk.Close()
		if g.Name != "" {
			out = append(out, g)
		}
	}
	return out
}

// dxgiAdapters lists the adapters DXGI enumerates, in its order.
func dxgiAdapters() ([]GPU, error) {
	if err := procCreateDXGIFactory1.Find(); err != nil {
		return nil, err
	}
	iid := windows.GUID{Data1: 0x770aae78, Data2: 0xf26f, Data3: 0x4dba, Data4: [8]byte{0xa8, 0x29, 0x25, 0x3c, 0x83, 0xd1, 0xb3, 0x87}}
	var f unsafe.Pointer
	if hr, _, _ := procCreateDXGIFactory1.Call(uintptr(unsafe.Pointer(&iid)), uintptr(unsafe.Pointer(&f))); hr != 0 || f == nil {
		return nil, fmt.Errorf("CreateDXGIFactory1: %#x", uint32(hr))
	}
	defer comCall(f, 2) // Release
	var out []GPU
	for i := 0; i < 16; i++ {
		var a unsafe.Pointer
		if hr := comCall(f, 12, uintptr(i), uintptr(unsafe.Pointer(&a))); hr != 0 || a == nil { // EnumAdapters1
			break
		}
		var d struct {
			Desc                                  [128]uint16
			Vendor, Device, SubSys, Revision      uint32
			DedicatedVideo, DedicatedSys, SharedS uintptr
			LUIDLow                               uint32
			LUIDHigh                              int32
			Flags                                 uint32
		}
		if comCall(a, 10, uintptr(unsafe.Pointer(&d))) == 0 { // GetDesc1
			g := GPU{Name: windows.UTF16ToString(d.Desc[:]), VRAMMB: int(d.DedicatedVideo >> 20), DXGI: i,
				LUID: fmt.Sprintf("%08x:%08x", uint32(d.LUIDHigh), d.LUIDLow)}
			if d.Flags&2 != 0 { // DXGI_ADAPTER_FLAG_SOFTWARE
				g.Name += " (software)"
			}
			out = append(out, g)
		}
		comCall(a, 2)
	}
	return out, nil
}

// comCall calls method i of COM object o's vtable.
func comCall(o unsafe.Pointer, i int, args ...uintptr) uintptr {
	vtbl := *(*unsafe.Pointer)(o)
	fn := *(*uintptr)(unsafe.Add(vtbl, i*int(unsafe.Sizeof(uintptr(0)))))
	r, _, _ := syscall.SyscallN(fn, append([]uintptr{uintptr(o)}, args...)...)
	return r
}

// cpuTimes is the machine's idle and total CPU time so far.
func cpuTimes() (idle, total time.Duration, ok bool) {
	var i, k, u windows.Filetime
	if r, _, _ := procGetSystemTimes.Call(uintptr(unsafe.Pointer(&i)), uintptr(unsafe.Pointer(&k)), uintptr(unsafe.Pointer(&u))); r == 0 {
		return 0, 0, false
	}
	t := func(f windows.Filetime) time.Duration {
		return time.Duration((int64(f.HighDateTime)<<32 | int64(f.LowDateTime)) * 100)
	}
	// Kernel time includes idle time.
	return t(i), t(k) + t(u), true
}
