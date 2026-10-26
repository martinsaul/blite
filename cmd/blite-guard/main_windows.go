// Command blite-guard runs alongside Blite on Windows, passively, and watches
// it like a debugger. It keeps diagnostics out of Blite itself:
//   - Blite's output and the guard's notes go to hourly segments in
//     %LOCALAPPDATA%\Blite\logs (quiet ones expire after 24 h; ones with an
//     error, crash or injected DLL are kept 7 days);
//   - every DLL loaded into Blite is recorded;
//   - when Blite crashes, a full memory dump goes to
//     %LOCALAPPDATA%\Blite\crash\blite-<time>.dmp (the newest 3 are kept).
//
// The dump is taken at the first access violation (or illegal instruction),
// before the Go runtime prints its traceback. Blite also runs with
// GOTRACEBACK=crash, so other fatal errors end in a fail-fast exception. Arguments are passed
// to bin\blite.exe unchanged.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32              = windows.NewLazySystemDLL("kernel32.dll")
	procWaitForDebugEvent = kernel32.NewProc("WaitForDebugEvent")
	procContinueDebug     = kernel32.NewProc("ContinueDebugEvent")
	procKillOnExit        = kernel32.NewProc("DebugSetProcessKillOnExit")
	dbghelp               = windows.NewLazySystemDLL("dbghelp.dll")
	procMiniDumpWrite     = dbghelp.NewProc("MiniDumpWriteDump")
)

const (
	exceptionEvent     = 1
	createProcessEvent = 3
	exitProcessEvent   = 5
	loadDLLEvent       = 6

	dbgContinue     = 0x00010002
	dbgNotHandled   = 0x80010001
	excBreakpoint   = 0x80000003
	excWx86Break    = 0x4000001F
	excAccess       = 0xC0000005
	excIllegal      = 0xC000001D
	excFailFast     = 0xC0000602
	excStackOverrun = 0xC0000409

	// MiniDumpWithFullMemory | WithHandleData | WithThreadInfo |
	// WithFullMemoryInfo | WithUnloadedModules: goroutine stacks live in the
	// Go heap, so the dump needs all memory.
	dumpType = 0x2 | 0x4 | 0x1000 | 0x800 | 0x20
	keepDump = 3
)

// debugEvent is DEBUG_EVENT on amd64: three DWORDs, padding, then the union.
type debugEvent struct {
	Code, PID, TID uint32
	_              uint32
	U              [160]byte
}

func main() {
	// Asked of Windows rather than taken from LOCALAPPDATA (Blite's logDir
	// does the same).
	local, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, 0)
	if err != nil || local == "" {
		local = os.Getenv("LOCALAPPDATA")
	}
	base := filepath.Join(local, "Blite")
	logs, crash := filepath.Join(base, "logs"), filepath.Join(base, "crash")
	os.MkdirAll(logs, 0o755)
	os.MkdirAll(crash, 0o755)
	exe, _ := os.Executable()
	root := filepath.Dir(exe)
	child := filepath.Join(root, "bin", "blite.exe")

	j, err := openJournal(logs)
	if err != nil {
		// Without a journal there's no point guarding, but Blite must still
		// start; it logs on its own and says why (BLITE_UNGUARDED).
		cmd := exec.Command(child, os.Args[1:]...)
		cmd.Env = append(os.Environ(), "BLITE_UNGUARDED=the guard can't write "+logs+": "+err.Error())
		cmd.Start()
		os.Exit(1)
	}
	defer j.Close()

	// WaitForDebugEvent must run on the thread that created the process.
	runtime.LockOSThread()
	cmd := exec.Command(child, os.Args[1:]...)
	cmd.Stdout, cmd.Stderr = j, j
	// BLITE_GUARD tells Blite it is guarded, so it doesn't relaunch itself
	// through the guard.
	cmd.Env = append(os.Environ(), "GOTRACEBACK=crash", "BLITE_GUARD=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DEBUG_ONLY_THIS_PROCESS | windows.CREATE_NO_WINDOW}
	if err := cmd.Start(); err != nil {
		j.Guardf(true, "start %s: %v", child, err)
		return
	}
	// If the guard dies, Blite keeps running.
	procKillOnExit.Call(0)
	j.Guardf(false, "started %s pid %d args %q", child, cmd.Process.Pid, os.Args[1:])

	code := debugLoop(j, cmd.Process.Pid, crash, root)
	cmd.Wait()
	j.Guardf(code != 0, "blite exited with code %#x", code)
	j.Close()
	os.Exit(int(code & 0xff))
}

func debugLoop(j *journal, pid int, crashDir, root string) uint32 {
	var proc windows.Handle
	dumped := false
	for {
		var ev debugEvent
		if r, _, err := procWaitForDebugEvent.Call(uintptr(unsafe.Pointer(&ev)), uintptr(windows.INFINITE)); r == 0 {
			j.Guardf(true, "WaitForDebugEvent: %v", err)
			return 1
		}
		cont := uintptr(dbgContinue)
		switch ev.Code {
		case createProcessEvent:
			closeHandle(handleAt(ev.U[:], 0)) // hFile
			proc = handleAt(ev.U[:], 8)       // hProcess, owned by the debugger
		case loadDLLEvent:
			f := handleAt(ev.U[:], 0)
			p := pathOf(f)
			closeHandle(f)
			// A DLL from outside Blite and Windows is an injection (overlay,
			// AV hook): worth keeping the segment for.
			j.Guardf(foreign(p, root), "dll %s", p)
		case exceptionEvent:
			code := *(*uint32)(unsafe.Pointer(&ev.U[0]))
			first := *(*uint32)(unsafe.Pointer(&ev.U[152])) != 0
			switch {
			case code == excBreakpoint || code == excWx86Break:
				// The loader's initial breakpoint: ours to swallow.
			// Dump at the first access violation: it catches memory before
			// the runtime unwinds. A nil dereference that Go recovers from
			// faults too, which costs at most one spare dump per run.
			case !first || code == excAccess || code == excIllegal || code == excFailFast || code == excStackOverrun:
				j.Guardf(true, "crash: exception %#x (first chance %v) in thread %d", code, first, ev.TID)
				if !dumped && proc != 0 {
					dumped = true
					dump(j, proc, pid, ev.TID, crashDir)
				}
				cont = dbgNotHandled
			default:
				// First-chance exceptions belong to Blite (the Go runtime
				// turns some into panics).
				cont = dbgNotHandled
			}
		case exitProcessEvent:
			code := *(*uint32)(unsafe.Pointer(&ev.U[0]))
			procContinueDebug.Call(uintptr(ev.PID), uintptr(ev.TID), dbgContinue)
			return code
		}
		procContinueDebug.Call(uintptr(ev.PID), uintptr(ev.TID), cont)
	}
}

func dump(j *journal, proc windows.Handle, pid int, tid uint32, dir string) {
	prune(dir, "blite-*.dmp", keepDump-1)
	path := filepath.Join(dir, "blite-"+time.Now().Format("20060102-150405")+".dmp")
	f, err := os.Create(path)
	if err != nil {
		j.Guardf(true, "dump: %v", err)
		return
	}
	defer f.Close()
	r, _, err := procMiniDumpWrite.Call(uintptr(proc), uintptr(pid), f.Fd(), dumpType, 0, 0, 0)
	if r == 0 {
		j.Guardf(true, "MiniDumpWriteDump: %v", err)
		return
	}
	st, _ := f.Stat()
	j.Guardf(true, "dump written: %s (%d MB, faulting thread %d)", path, st.Size()>>20, tid)
}

func handleAt(b []byte, off int) windows.Handle {
	return *(*windows.Handle)(unsafe.Pointer(&b[off]))
}

func closeHandle(h windows.Handle) {
	if h != 0 && h != windows.InvalidHandle {
		windows.CloseHandle(h)
	}
}

func pathOf(f windows.Handle) string {
	if f == 0 || f == windows.InvalidHandle {
		return "?"
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetFinalPathNameByHandle(f, &buf[0], uint32(len(buf)), 0)
	if err != nil || n == 0 {
		return "?"
	}
	return strings.TrimPrefix(windows.UTF16ToString(buf[:n]), `\\?\`)
}

func foreign(path, root string) bool {
	p := strings.ToLower(path)
	if sys := strings.ToLower(os.Getenv("SystemRoot")); sys != "" && strings.HasPrefix(p, sys) {
		return false
	}
	return !strings.HasPrefix(p, strings.ToLower(root))
}

// prune keeps the newest keep files matching pattern in dir.
func prune(dir, pattern string, keep int) {
	m, _ := filepath.Glob(filepath.Join(dir, pattern))
	if len(m) <= keep {
		return
	}
	sort.Strings(m) // names carry a sortable timestamp
	for _, p := range m[:len(m)-keep] {
		if err := os.Remove(p); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}
}
