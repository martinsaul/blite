package diag

import (
	"fmt"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32                          = windows.NewLazySystemDLL("user32.dll")
	procGetDisplayConfigBufferSizes = user32.NewProc("GetDisplayConfigBufferSizes")
	procQueryDisplayConfig          = user32.NewProc("QueryDisplayConfig")
	procDisplayConfigGetDeviceInfo  = user32.NewProc("DisplayConfigGetDeviceInfo")
	procEnumDisplayMonitors         = user32.NewProc("EnumDisplayMonitors")
	procGetMonitorInfoW             = user32.NewProc("GetMonitorInfoW")
	procSetDpiAwarenessContext      = user32.NewProc("SetProcessDpiAwarenessContext")
	shcore                          = windows.NewLazySystemDLL("shcore.dll")
	procGetDpiForMonitor            = shcore.NewProc("GetDpiForMonitor")
	procSetProcessDpiAwareness      = shcore.NewProc("SetProcessDpiAwareness")
)

// The DisplayConfig structures (wingdi.h).
type (
	dcLUID struct {
		Low  uint32
		High int32
	}
	dcPathInfo struct {
		SrcAdapter dcLUID
		SrcID      uint32
		SrcModeIdx uint32
		SrcFlags   uint32
		TgtAdapter dcLUID
		TgtID      uint32
		TgtModeIdx uint32
		Tech       uint32
		Rotation   uint32
		Scaling    uint32
		RefreshNum uint32
		RefreshDen uint32
		ScanLine   uint32
		Available  int32
		TgtFlags   uint32
		Flags      uint32
	}
	dcModeInfo struct {
		InfoType uint32
		ID       uint32
		Adapter  dcLUID
		// Union: for a source mode, width, height, pixel format, x, y.
		U [48]byte
	}
	dcHeader struct {
		Type    uint32
		Size    uint32
		Adapter dcLUID
		ID      uint32
	}
)

var dpiOnce sync.Once

// SetDPIAware makes Windows report real pixels and the real scale to this
// process (else a 4K monitor at 150% looks 2560x1440 and 100%).
func SetDPIAware() {
	dpiOnce.Do(func() {
		if procSetDpiAwarenessContext.Find() == nil {
			const perMonitorV2 = ^uintptr(3) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 (-4)
			if r, _, _ := procSetDpiAwarenessContext.Call(perMonitorV2); r != 0 {
				return
			}
		}
		if procSetProcessDpiAwareness.Find() == nil {
			procSetProcessDpiAwareness.Call(2) // PROCESS_PER_MONITOR_DPI_AWARE
		}
	})
}

// outputTech names DISPLAYCONFIG_VIDEO_OUTPUT_TECHNOLOGY values.
var outputTech = map[uint32]string{0: "VGA", 4: "DVI", 5: "HDMI", 6: "LVDS (built-in)", 10: "DisplayPort",
	11: "DisplayPort (built-in)", 15: "Miracast", 16: "indirect display (wired)", 17: "indirect display (virtual)",
	0x80000000: "built-in"}

type monitor struct {
	h       uintptr
	primary bool
	dpi     uint32
}

var (
	monitorsMu sync.Mutex
	monitorsBy map[string]monitor
	monitorCB  = syscall.NewCallback(func(h, _, _, _ uintptr) uintptr {
		var mi struct {
			Size    uint32
			Monitor windows.Rect
			Work    windows.Rect
			Flags   uint32
			Device  [32]uint16
		}
		mi.Size = uint32(unsafe.Sizeof(mi))
		if r, _, _ := procGetMonitorInfoW.Call(h, uintptr(unsafe.Pointer(&mi))); r == 0 {
			return 1
		}
		m := monitor{h: h, primary: mi.Flags&1 != 0}
		var dx, dy uint32
		if procGetDpiForMonitor.Find() == nil {
			if r, _, _ := procGetDpiForMonitor.Call(h, 0, uintptr(unsafe.Pointer(&dx)), uintptr(unsafe.Pointer(&dy))); r == 0 {
				m.dpi = dx
			}
		}
		monitorsBy[windows.UTF16ToString(mi.Device[:])] = m
		return 1
	})
)

// displays lists the active monitors from DisplayConfig (the real mode and
// refresh rate, the connection, HDR), with the scale and primary flag of
// the matching HMONITOR.
func displays() ([]Display, error) {
	SetDPIAware()
	monitorsMu.Lock()
	monitorsBy = map[string]monitor{}
	procEnumDisplayMonitors.Call(0, 0, monitorCB, 0)
	mons := monitorsBy
	monitorsMu.Unlock()

	adapters := map[string]string{}
	if gs, err := dxgiAdapters(); err == nil {
		for _, g := range gs {
			adapters[g.LUID] = g.Name
		}
	}

	var np, nm uint32
	const onlyActive = 2 // QDC_ONLY_ACTIVE_PATHS
	if r, _, _ := procGetDisplayConfigBufferSizes.Call(onlyActive, uintptr(unsafe.Pointer(&np)), uintptr(unsafe.Pointer(&nm))); r != 0 {
		return nil, fmt.Errorf("GetDisplayConfigBufferSizes: %d", r)
	}
	if np == 0 {
		return nil, nil
	}
	paths := make([]dcPathInfo, np)
	modes := make([]dcModeInfo, nm)
	if r, _, _ := procQueryDisplayConfig.Call(onlyActive, uintptr(unsafe.Pointer(&np)), uintptr(unsafe.Pointer(&paths[0])),
		uintptr(unsafe.Pointer(&nm)), uintptr(unsafe.Pointer(&modes[0])), 0); r != 0 {
		return nil, fmt.Errorf("QueryDisplayConfig: %d", r)
	}
	var out []Display
	for _, p := range paths[:np] {
		d := Display{Connection: outputTech[p.Tech]}
		if d.Connection == "" {
			d.Connection = fmt.Sprintf("output %d", p.Tech)
		}
		if p.RefreshDen != 0 {
			d.RefreshHz = float64(p.RefreshNum) / float64(p.RefreshDen)
		}
		d.Adapter = adapters[fmt.Sprintf("%08x:%08x", uint32(p.TgtAdapter.High), p.TgtAdapter.Low)]
		if i := p.SrcModeIdx; i < nm && modes[i].InfoType == 1 { // source mode
			u := modes[i].U[:]
			d.Width = int(le32(u[0:]))
			d.Height = int(le32(u[4:]))
			d.X, d.Y = int(int32(le32(u[12:]))), int(int32(le32(u[16:])))
		}

		var tn struct {
			H                    dcHeader
			Flags, Tech          uint32
			EdidMfr, EdidProduct uint16
			Connector            uint32
			Friendly             [64]uint16
			Path                 [128]uint16
		}
		tn.H = dcHeader{Type: 2, Size: uint32(unsafe.Sizeof(tn)), Adapter: p.TgtAdapter, ID: p.TgtID} // GET_TARGET_NAME
		if r, _, _ := procDisplayConfigGetDeviceInfo.Call(uintptr(unsafe.Pointer(&tn))); r == 0 {
			d.Name = windows.UTF16ToString(tn.Friendly[:])
		}
		var sn struct {
			H    dcHeader
			Name [32]uint16
		}
		sn.H = dcHeader{Type: 1, Size: uint32(unsafe.Sizeof(sn)), Adapter: p.SrcAdapter, ID: p.SrcID} // GET_SOURCE_NAME
		if r, _, _ := procDisplayConfigGetDeviceInfo.Call(uintptr(unsafe.Pointer(&sn))); r == 0 {
			d.Device = windows.UTF16ToString(sn.Name[:])
		}
		if d.Name == "" {
			d.Name = strings.TrimPrefix(d.Device, `\\.\`)
		}
		var ac struct {
			H                     dcHeader
			Value, Encoding, Bits uint32
		}
		ac.H = dcHeader{Type: 9, Size: uint32(unsafe.Sizeof(ac)), Adapter: p.TgtAdapter, ID: p.TgtID} // GET_ADVANCED_COLOR_INFO
		if r, _, _ := procDisplayConfigGetDeviceInfo.Call(uintptr(unsafe.Pointer(&ac))); r == 0 {
			switch {
			case ac.Value&2 != 0:
				d.HDR = "on"
			case ac.Value&1 != 0:
				d.HDR = "off"
			default:
				d.HDR = "unsupported"
			}
		}
		if m, ok := mons[d.Device]; ok {
			d.Primary = m.primary
			if m.dpi > 0 {
				d.ScalePct = int(m.dpi * 100 / 96)
			}
		} else if d.X == 0 && d.Y == 0 {
			d.Primary = true // the primary monitor sits at the origin
		}
		out = append(out, d)
	}
	return out, nil
}

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}
