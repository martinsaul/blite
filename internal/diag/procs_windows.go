package diag

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

func processes() ([]Proc, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	var out []Proc
	e := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		out = append(out, Proc{Exe: windows.UTF16ToString(e.ExeFile[:]), PID: int(e.ProcessID)})
	}
	return out, nil
}

var (
	pdh                             = windows.NewLazySystemDLL("pdh.dll")
	procPdhOpenQuery                = pdh.NewProc("PdhOpenQueryW")
	procPdhAddEnglishCounter        = pdh.NewProc("PdhAddEnglishCounterW")
	procPdhCollectQueryData         = pdh.NewProc("PdhCollectQueryData")
	procPdhGetFormattedCounterArray = pdh.NewProc("PdhGetFormattedCounterArrayW")
	procPdhCloseQuery               = pdh.NewProc("PdhCloseQuery")
)

// sampleGPU starts measuring the GPU engines' use per process (the "GPU
// Engine" performance counters Task Manager shows); stop returns the use
// since, busiest first.
func sampleGPU() (stop func() []EngineUse, err error) {
	if err := procPdhOpenQuery.Find(); err != nil {
		return nil, err
	}
	var q, c uintptr
	if r, _, _ := procPdhOpenQuery.Call(0, 0, uintptr(unsafe.Pointer(&q))); r != 0 {
		return nil, fmt.Errorf("PdhOpenQuery: %#x", uint32(r))
	}
	path, _ := windows.UTF16PtrFromString(`\GPU Engine(*)\Utilization Percentage`)
	if r, _, _ := procPdhAddEnglishCounter.Call(q, uintptr(unsafe.Pointer(path)), 0, uintptr(unsafe.Pointer(&c))); r != 0 {
		procPdhCloseQuery.Call(q)
		return nil, fmt.Errorf("no GPU Engine counters (%#x)", uint32(r))
	}
	procPdhCollectQueryData.Call(q)
	return func() []EngineUse {
		defer procPdhCloseQuery.Call(q)
		if r, _, _ := procPdhCollectQueryData.Call(q); r != 0 {
			return nil
		}
		items, err := pdhArray(c)
		if err != nil {
			return nil
		}
		// Instances: pid_1234_luid_0x..._0x..._phys_0_eng_3_engtype_VideoEncode
		sum := map[[2]string]float64{}
		for name, v := range items {
			pid, typ := between(name, "pid_", "_"), name[strings.LastIndex(name, "engtype_")+len("engtype_"):]
			if !strings.Contains(name, "engtype_") || pid == "" {
				continue
			}
			sum[[2]string{pid, typ}] += v
		}
		var out []EngineUse
		for k, v := range sum {
			pid, _ := strconv.Atoi(k[0])
			if v > 0.05 {
				out = append(out, EngineUse{PID: pid, Engine: k[1], Percent: min(v, 100)})
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Percent > out[j].Percent })
		return out
	}, nil
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	s = s[i+len(a):]
	if j := strings.Index(s, b); j >= 0 {
		return s[:j]
	}
	return s
}

// pdhArray reads a wildcard counter's values by instance name.
func pdhArray(c uintptr) (map[string]float64, error) {
	const (
		fmtDouble = 0x200
		moreData  = 0x800007D2
	)
	var size, n uint32
	r, _, _ := procPdhGetFormattedCounterArray.Call(c, fmtDouble, uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&n)), 0)
	if uint32(r) != moreData || size == 0 {
		return nil, fmt.Errorf("PdhGetFormattedCounterArray: %#x", uint32(r))
	}
	// PDH_FMT_COUNTERVALUE_ITEM_W: name pointer, then status and the value.
	type item struct {
		Name   *uint16
		Status uint32
		_      uint32
		Value  float64
	}
	buf := make([]item, size/uint32(unsafe.Sizeof(item{}))+1)
	r, _, _ = procPdhGetFormattedCounterArray.Call(c, fmtDouble, uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&n)), uintptr(unsafe.Pointer(&buf[0])))
	if r != 0 {
		return nil, fmt.Errorf("PdhGetFormattedCounterArray: %#x", uint32(r))
	}
	if int(n) > len(buf) {
		return nil, errors.New("PDH array overran")
	}
	out := map[string]float64{}
	for _, it := range buf[:n] {
		if it.Status == 0 && it.Name != nil {
			out[windows.UTF16PtrToString(it.Name)] += it.Value
		}
	}
	return out, nil
}
