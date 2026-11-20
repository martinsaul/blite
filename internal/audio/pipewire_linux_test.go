package audio

import (
	"bytes"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// logBuf is a log the sharer's loop and the test share.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Needs a PipeWire session with two players: application.name AppOne (440 Hz)
// and AppTwo (1000 Hz). Run via scripts/pw-test-session.sh; skipped otherwise.
func TestShareOnlySelectedApp(t *testing.T) {
	if os.Getenv("BLITE_PW_TEST") == "" {
		t.Skip("set BLITE_PW_TEST=1 inside a PipeWire test session")
	}
	apps, err := ListApps()
	if err != nil {
		t.Fatal(err)
	}
	var one, two string
	for _, a := range apps {
		switch a.Name {
		case "AppOne":
			one = a.Key
		case "AppTwo":
			two = a.Key
		}
	}
	if one == "" || two == "" {
		t.Fatalf("test apps not found: %+v", apps)
	}
	var log logBuf
	s, err := NewSharer(slog.New(slog.NewTextHandler(&log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	measure := func() (p440, p1000 float64) {
		raw := capture(t, string(s.Capture()))
		return tone(raw, 440), tone(raw, 1000)
	}
	check := func(label string, want440, want1000 bool) {
		time.Sleep(time.Second)
		a, b := measure()
		t.Logf("%s: 440Hz=%.3g 1000Hz=%.3g", label, a, b)
		const on = 1e8
		if (a > on) != want440 || (b > on) != want1000 {
			t.Fatalf("%s: wrong mix", label)
		}
	}
	if err := s.Select(Selection{Mode: "apps", Apps: []string{one}}); err != nil {
		t.Fatal(err)
	}
	check("AppOne only", true, false)
	if err := s.Select(Selection{Mode: "apps", Apps: []string{two}}); err != nil {
		t.Fatal(err)
	}
	check("AppTwo only", false, true)
	if err := s.Select(Selection{Mode: "system"}); err != nil {
		t.Fatal(err)
	}
	check("system", true, true)
	if err := s.Select(Selection{Mode: "none"}); err != nil {
		t.Fatal(err)
	}
	check("none", false, false)
	// An app that restarted (another pid): silence, and a warning naming
	// what plays.
	if err := s.Select(Selection{Mode: "apps", Apps: []string{"AppOne#1"}}); err != nil {
		t.Fatal(err)
	}
	check("stale app", false, false)
	got := log.String()
	t.Log(got)
	for _, want := range []string{`mode=apps streams="AppOne pid=`, `mode=system streams="AppOne pid=`,
		`msg="shared audio matches no app stream" mode=apps apps=[AppOne#1] pid=0 playing="[AppOne#`} {
		if !strings.Contains(got, want) {
			t.Errorf("log lacks %q", want)
		}
	}
}

func capture(t *testing.T, src string) []int16 {
	t.Helper()
	f := t.TempDir() + "/cap.raw"
	desc := src + " num-buffers=40 ! audioconvert ! audioresample ! audio/x-raw,format=S16LE,channels=1,rate=48000 ! filesink location=" + f
	cmd := exec.Command("timeout", "8", "sh", "-c", "gst-launch-1.0 -q "+desc)
	if out, err := cmd.CombinedOutput(); err != nil && !strings.Contains(err.Error(), "exit status") {
		t.Fatalf("capture: %v %s", err, out)
	}
	b, err := os.ReadFile(f)
	if err != nil || len(b) < 9600 {
		t.Fatalf("capture produced %d bytes: %v", len(b), err)
	}
	s := make([]int16, len(b)/2)
	for i := range s {
		s[i] = int16(uint16(b[2*i]) | uint16(b[2*i+1])<<8)
	}
	return s[len(s)/4:]
}

// tone is the Goertzel power of frequency f in 48 kHz samples.
func tone(s []int16, f float64) float64 {
	k := 2 * math.Cos(2*math.Pi*f/48000)
	var a, b float64
	for _, x := range s {
		a, b = float64(x)+k*a-b, a
	}
	return (a*a + b*b - k*a*b) / float64(len(s))
}

func TestInProcessTree(t *testing.T) {
	me, parent := os.Getpid(), os.Getppid()
	if !inProcessTree(me, me) || !inProcessTree(me, parent) {
		t.Fatal("own process should be in its own and its parent's tree")
	}
	if inProcessTree(parent, me) || inProcessTree(me, 0) {
		t.Fatal("parent is not in the child's tree")
	}
}
