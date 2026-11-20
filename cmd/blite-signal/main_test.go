package main

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"blite/internal/server"
)

// The deploy hooks end to end, on the real binary: a deferred green
// passes the healthcheck and the canary without touching the state file,
// adopts on SIGHUP or -adopt, and once drained refuses the canary and
// saves nothing, not even on SIGTERM.
func TestBlueGreenHooks(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "blite-signal")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	state := filepath.Join(dir, "state", "rooms.json")
	os.MkdirAll(filepath.Dir(state), 0o700)
	// Blue's state: one room.
	blue := server.New(server.Config{StateFile: state, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if _, err := blue.Rooms().Create("", "alice", "", "", 0, false, nopOut{}); err != nil {
		t.Fatal(err)
	}
	if err := blue.SaveState(); err != nil {
		t.Fatal(err)
	}
	defer1 := filepath.Join(filepath.Dir(state), ".ci-deploy-defer-green")

	type proc struct {
		cmd  *exec.Cmd
		env  []string
		port string
		log  *syncBuf
	}
	run := func(p *proc, args ...string) (int, string) {
		t.Helper()
		c := exec.Command(bin, args...)
		c.Env = p.env
		out, err := c.CombinedOutput()
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), string(out)
		} else if err != nil {
			t.Fatal(err)
		}
		return 0, string(out)
	}
	startProc := func(color string) *proc {
		t.Helper()
		p := &proc{port: freePort(t), log: &syncBuf{}}
		p.env = append(os.Environ(), "BLITE_ADDR=:"+p.port, "BLITE_ADMIN_ADDR=127.0.0.1:"+freePort(t),
			"BLITE_STATE_FILE="+state, "BLITE_COLOR="+color)
		p.cmd = exec.Command(bin)
		p.cmd.Env = p.env
		p.cmd.Stdout, p.cmd.Stderr = p.log, p.log
		if err := p.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { p.cmd.Process.Kill(); p.cmd.Wait() })
		for i := 0; ; i++ {
			if code, _ := run(p, "-healthcheck"); code == 0 {
				break
			}
			if i == 100 {
				t.Fatalf("never healthy:\n%s", p.log)
			}
			time.Sleep(50 * time.Millisecond)
		}
		return p
	}
	wsURL := func(p *proc) string { return "ws://127.0.0.1:" + p.port + "/ws" }
	read := func() []byte { b, _ := os.ReadFile(state); return b }
	stateBefore := read()

	// Green, deferred.
	os.WriteFile(defer1, nil, 0o600)
	g := startProc("green")
	if code, out := run(g, "-status"); code != 0 || !strings.Contains(out, "mode=deferred rooms=0") {
		t.Fatalf("status: %d %s", code, out)
	}
	if code, out := run(g, "-canary", wsURL(g)); code != 0 || !strings.Contains(out, "canary ok") {
		t.Fatalf("canary: %d %s", code, out)
	}
	if code, out := run(g, "-drain"); code == 0 {
		t.Fatalf("drain while deferred: %s", out)
	}
	if !bytes.Equal(read(), stateBefore) {
		t.Fatal("deferred green wrote the state file")
	}
	if code, out := run(g, "-adopt"); code != 0 || !strings.Contains(out, "adopted state: rooms=1") {
		t.Fatalf("adopt: %d %s", code, out)
	}
	if code, out := run(g, "-adopt"); code != 0 || !strings.Contains(out, "already") {
		t.Fatalf("adopt again: %d %s", code, out)
	}
	if !strings.Contains(g.log.String(), `"msg":"adopted state"`) {
		t.Fatalf("no adopted-state log line:\n%s", g.log)
	}

	// Drain: flush, idempotent, canary fails, no save on SIGTERM.
	if code, out := run(g, "-drain"); code != 0 || !strings.Contains(out, "drained") {
		t.Fatalf("drain: %d %s", code, out)
	}
	flushed := read()
	if code, out := run(g, "-drain"); code != 0 || !strings.Contains(out, "already drained") {
		t.Fatalf("drain again: %d %s", code, out)
	}
	if code, out := run(g, "-canary", wsURL(g)); code != 1 || !strings.Contains(out, "canary failed") {
		t.Fatalf("canary on a drained server: %d %s", code, out)
	}
	g.cmd.Process.Signal(syscall.SIGTERM)
	g.cmd.Wait()
	if !bytes.Equal(read(), flushed) {
		t.Fatal("drained server wrote the state file on shutdown")
	}

	// SIGHUP doesn't adopt (a stray statesync HUP mustn't); -adopt does,
	// even once the defer file is gone (it's only read at startup).
	g2 := startProc("green")
	os.Remove(defer1)
	g2.cmd.Process.Signal(syscall.SIGHUP)
	time.Sleep(300 * time.Millisecond)
	if _, out := run(g2, "-status"); !strings.Contains(out, "mode=deferred") {
		t.Fatalf("SIGHUP adopted a deferred server: %s", out)
	}
	if code, out := run(g2, "-adopt"); code != 0 {
		t.Fatalf("adopt after SIGHUP: %d %s", code, out)
	}
	if _, out := run(g2, "-status"); !strings.Contains(out, "mode=normal rooms=1") {
		t.Fatalf("status after adopt: %s", out)
	}

	// No defer file (or no color): today's behaviour.
	b := startProc("blue")
	if code, out := run(b, "-status"); code != 0 || !strings.Contains(out, "mode=normal rooms=1") {
		t.Fatalf("status without a defer file: %d %s", code, out)
	}
	if code, _ := run(b, "-adopt"); code == 0 {
		t.Fatal("adopt on a server that didn't defer")
	}
	if code, out := run(b, "-canary"); code != 2 {
		t.Fatalf("canary without a URL: %d %s", code, out)
	}
}

// syncBuf is a bytes.Buffer the child's output goroutine and the test
// may use at once.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) { b.mu.Lock(); defer b.mu.Unlock(); return b.b.Write(p) }
func (b *syncBuf) String() string              { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }

type nopOut struct{}

func (nopOut) Send(any) bool { return true }

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}
