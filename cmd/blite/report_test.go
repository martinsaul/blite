package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"blite/internal/proto"
	"blite/internal/redact"
	"blite/internal/session"
)

func TestCrashedLogs(t *testing.T) {
	dir := t.TempDir()
	w := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w("blite-20261026-010000-100.log", "a\n")                     // pid 100, earlier segment
	w("blite-20261026-020000-100.log", "b\nmsg=\"clean exit\"\n") // pid 100 ended cleanly
	w("blite-20261026-030000-200.log", "c\n")                     // pid 200 just stopped: unclean
	w("blite-20261026-040000-300.log", "panic: boom\n")           // crash mark
	w("blite-20261026-050000.log", "guard: started\n")            // guard, quiet
	w("blite-20261026-060000-event.log", "guard: crash: exception 0xc0000005\n")
	w("blite-20261026-070000-400.log", "current\n")
	w("sent.txt", "blite-20261026-040000-300.log\n")
	got := crashedLogs(dir, filepath.Join(dir, "blite-20261026-070000-400.log"), time.Now())
	want := []string{"blite-20261026-030000-200.log", "blite-20261026-060000-event.log"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestReportURL(t *testing.T) {
	if u, _ := reportURL("wss://blite.example.com/ws"); u != "https://blite.example.com/v1/report" {
		t.Fatal(u)
	}
}

func TestNormalizeServerFlag(t *testing.T) {
	for in, want := range map[string]string{
		"share.example.com":          "wss://share.example.com/ws",
		"https://Share.Example.com/": "wss://share.example.com/ws",
		"ws://localhost:8080/ws":     "ws://localhost:8080/ws",
		"http://127.0.0.1:8080":      "ws://127.0.0.1:8080/ws",
	} {
		got, err := normalizeServerFlag(in)
		if err != nil || got != want {
			t.Fatalf("normalizeServerFlag(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"wss://alice:s3cretSrvPw@share.example.com/ws",
		"wss://share.example.com/ws?token=s3cretSrvTok",
		"wss://share.example.com/ws#s3cretSrvFrag",
	} {
		_, err := normalizeServerFlag(bad)
		if err == nil {
			t.Fatalf("normalizeServerFlag(%q) succeeded, want error", bad)
		}
		for _, sec := range []string{"s3cretSrvPw", "s3cretSrvTok", "s3cretSrvFrag"} {
			if strings.Contains(err.Error(), sec) {
				t.Fatalf("normalizeServerFlag(%q) error leaked %q: %v", bad, sec, err)
			}
		}
	}
}

func TestPickUserLogs(t *testing.T) {
	// Windows under the guard: the running tray's journal is the oldest;
	// two blite:// links each left a short -event journal after it.
	names := []string{
		"blite-20261103-120000-event.log",
		"blite-20261103-123000.log",
		"blite-20261103-123706-event.log",
		"blite-20261103-123810-event.log",
	}
	got := pickUserLogs(names, "")
	want := []string{"blite-20261103-123000.log", "blite-20261103-123706-event.log", "blite-20261103-123810-event.log"}
	if !slices.Equal(got, want) {
		t.Fatalf("guard: got %v, want %v", got, want)
	}
	// Our own segments: current is named.
	names = []string{"blite-20261103-100000-11.log", "blite-20261103-110000-22.log", "blite-20261103-120000-33.log", "blite-20261103-130000-44.log"}
	got = pickUserLogs(names, "blite-20261103-100000-11.log")
	want = []string{"blite-20261103-100000-11.log", "blite-20261103-120000-33.log", "blite-20261103-130000-44.log"}
	if !slices.Equal(got, want) {
		t.Fatalf("own: got %v, want %v", got, want)
	}
}

// The Send logs zip holds system.txt and the newest zipFiles logs, whole.
func TestBuildZip(t *testing.T) {
	dir := t.TempDir()
	var names []string
	for i := range 8 {
		n := fmt.Sprintf("blite-2026110%d-010000-1.log", i)
		os.WriteFile(filepath.Join(dir, n), []byte(strings.Repeat("line\n", 1000*(i+1))), 0o600)
		names = append(names, n)
	}
	b, got, err := buildZip(dir, names, "cpus: 8\n")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, names[2:]) {
		t.Fatalf("zipped %v", got)
	}
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) != 1+zipFiles || zr.File[0].Name != "system.txt" {
		t.Fatalf("entries %d, first %q", len(zr.File), zr.File[0].Name)
	}
	f, _ := zr.File[len(zr.File)-1].Open()
	whole, _ := io.ReadAll(f)
	if len(whole) != 5*1000*8 {
		t.Fatalf("newest log: %d bytes, want it whole", len(whole))
	}
}

func TestReportUploadRedactsSecrets(t *testing.T) {
	var (
		mu       sync.Mutex
		received []proto.Report
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/report" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var rep proto.Report
		if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		received = append(received, rep)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	dir := t.TempDir()
	crashLog := "blite-20261115-010000-event.log"
	curLog := "blite-20261115-020000-444.log"

	crashBody := strings.Join([]string{
		`2026-11-15T01:00:00Z guard: started C:\App\bin\blite.exe pid 444 args ["bshare://join/K7PQ2M4X#pwGuard11" "-background"]`,
		`time=2026-11-15T01:00:01Z level=INFO msg="room open" room=7K3Q-X9MD link=https://share.bellion.io/j/7K3QX9MD#pwLink22 password=pwUnquoted33 pw="pw Quoted 44" token=tok55 RoomPassword=rp56 room_password=rp57 Pass=pass58 Key=key59 SECRET=sec60 key=true key=12 peer=alice#7F2A name=martin#0A1B ch=chan#3`,
		`time=2026-11-15T01:00:02Z level=WARN msg="join retry https://share.bellion.io/j/MAMACO#pwMsg66 and blite://join/ABCD1234?server=s.example.com#pwBlite77 by alice#7F2A"`,
		`time=2026-11-15T01:00:03Z level=WARN msg="forms" raw="https://share.bellion.io/custom/7K3QX9MD#pwEsc\"Quote61" pct="https://share.bellion.io/r/7K3QX9MD%23pwPct62" bare="K7PQ-2M4X#pwBareCode63" uinfo="wss://us%40er:p@ssUinfo64@share.example.com/ws?token=pwQTok65&password=pwQPw66&key=pwQKey67" struct={Room:7K3QX9MD Password:pwStruct68}`,
		`err="dial failed\n\tbshare://join/K7PQ2M4X#pwNL2" detail="a\nK7PQ-2M4X#pwNL3" tab="a\tblite://join/ABCD#pwNL4" msg="line1\nhttps://h.example.com/j/ABCD#pw77\nline3"`,
		`joining k7pq-2m4x #pw14b`,
		`joining   k7pq  2m4x  #pw14`,
		`msg="paste k7pq 2m4x #pwSp5"`,
		`u1=https://share.bellion.io/j/ABCD#pw'22' u2=https://share.bellion.io/j/ABCD#pw[25] u3=https://share.bellion.io/j/ABCD#pw<26> msg="x password=pw\"58 more" url="https://h/x?token=a b41"`,
		`password: pwColon69 Authorization: Bearer pwBearer70 cmd="-password pwFlag71" api_key=pwAk72 apikey=pwAk73 access_token=pwAt74 x-token=pwXt75 passwd=pwPwd76 jsonNum={"password":98765}`,
		`guard: crash: exception 0xc0000005`,
		"",
	}, "\n")

	curBody := strings.Join([]string{
		`{"time":"2026-11-15T02:00:00Z","level":"INFO","msg":"join","password":"pwJson88","roomPassword":"pwJsonRp89","key":"pwJsonKey90","secret":"pwJsonSec91","link":"https://share.bellion.io/j/9999AAAA#pwJsonLink99","peer":"bob#9A1C"}`,
		`time=2026-11-15T02:00:01Z level=INFO msg="pasted share.bellion.io/invite/ZXCV1234#pwBare100 and k7pq 2m4x#pwSpaced103"`,
		`time=2026-11-15T02:00:02Z level=INFO msg="healthy frame" fps=60`,
		"",
	}, "\n")

	if err := os.WriteFile(filepath.Join(dir, crashLog), []byte(crashBody), 0o600); err != nil {
		t.Fatal(err)
	}
	curPath := filepath.Join(dir, curLog)
	if err := os.WriteFile(curPath, []byte(curBody), 0o600); err != nil {
		t.Fatal(err)
	}

	prevLogs := logState()
	setLogs(func(l *logInfo) {
		*l = logInfo{Dir: dir, File: curPath}
	})
	defer setLogs(func(l *logInfo) { *l = prevLogs })

	secrets := []string{
		"pwGuard11", "pwLink22", "pwUnquoted33", "pw Quoted 44", "tok55",
		"rp56", "rp57", "pass58", "key59", "sec60",
		"pwEsc", "Quote61", "pwPct62", "pwBareCode63", "ssUinfo64", "pwQTok65", "pwQPw66", "pwQKey67", "pwStruct68",
		"pwNL2", "pwNL3", "pwNL4", "pw77", "pw14b", "pw14", "pwSp5",
		"pw'22'", "pw[25]", "pw<26>", "pw\\\"58", "a b41", "b41",
		"pwColon69", "pwBearer70", "pwFlag71", "pwAk72", "pwAk73", "pwAt74", "pwXt75", "pwPwd76", "98765",
		"pwMsg66", "pwBlite77", "pwJson88", "pwJsonRp89", "pwJsonKey90", "pwJsonSec91",
		"pwJsonLink99", "pwBare100", "pwSys101", "pwFb102", "pwSpaced103",
	}
	assertClean := func(where, s string) {
		t.Helper()
		for _, sec := range secrets {
			if strings.Contains(s, sec) {
				t.Fatalf("%s leaked secret %q:\n%s", where, sec, s)
			}
		}
	}

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	rep := &reporter{
		server: wsURL,
		log:    quiet,
		name:   func() string { return "alice#7F2A" },
		base:   proto.Report{Platform: "linux", Version: "dev", Build: 1},
		system: func() string {
			return "version: dev\nlastError: bshare://join/K7PQ2M4X#pwSys101\n"
		},
	}

	// 1. Automatic crash-report upload.
	rep.previous()
	mu.Lock()
	if len(received) != 1 {
		t.Fatalf("crash reports received = %d, want 1", len(received))
	}
	crashRep := received[0]
	mu.Unlock()

	assertClean("crash report Log", crashRep.Log)
	for _, keep := range []string{
		`args ["bshare://join/K7PQ2M4X#[cut]" "-background"]`,
		`room=7K3Q-X9MD link=https://share.bellion.io/j/7K3QX9MD#[cut] password=[cut] pw=[cut] token=[cut] RoomPassword=[cut] room_password=[cut] Pass=[cut] Key=[cut] SECRET=[cut] key=true key=12 peer=alice#7F2A name=martin#0A1B ch=chan#3`,
		`https://share.bellion.io/j/MAMACO#[cut]`,
		`blite://join/ABCD1234?server=s.example.com#[cut]`,
		`by alice#7F2A`,
		`raw="https://share.bellion.io/custom/7K3QX9MD#[cut]"`,
		`pct="https://share.bellion.io/r/7K3QX9MD#[cut]"`,
		`bare="K7PQ-2M4X#[cut]"`,
		`wss://[cut]@share.example.com/ws?token=[cut]&password=[cut]&key=[cut]`,
		`struct={Room:7K3QX9MD Password:[cut]}`,
		`guard: crash: exception 0xc0000005`,
	} {
		if !strings.Contains(crashRep.Log, keep) {
			t.Fatalf("crash report lost expected content %q:\n%s", keep, crashRep.Log)
		}
	}

	// 2. User "Send logs" upload (both inline Log and Zip entries).
	if err := rep.send(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(received) != 2 {
		t.Fatalf("reports after send = %d, want 2", len(received))
	}
	userRep := received[1]
	mu.Unlock()

	assertClean("Send logs inline Log", userRep.Log)
	for _, keep := range []string{
		`"link":"https://share.bellion.io/j/9999AAAA#[cut]"`,
		`"peer":"bob#9A1C"`,
		`share.bellion.io/invite/ZXCV1234#[cut]`,
		`k7pq 2m4x#[cut]`,
		`msg="healthy frame" fps=60`,
	} {
		if !strings.Contains(userRep.Log, keep) {
			t.Fatalf("Send logs lost expected content %q:\n%s", keep, userRep.Log)
		}
	}
	if len(userRep.Zip) == 0 {
		t.Fatal("expected non-empty Zip in Send logs report")
	}
	zr, err := zip.NewReader(bytes.NewReader(userRep.Zip), int64(len(userRep.Zip)))
	if err != nil {
		t.Fatal(err)
	}
	seenZip := map[string]string{}
	for _, zf := range zr.File {
		rc, err := zf.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		txt := string(b)
		assertClean("zip entry "+zf.Name, txt)
		seenZip[zf.Name] = txt
	}
	if !strings.Contains(seenZip["system.txt"], "bshare://join/K7PQ2M4X#[cut]") {
		t.Fatalf("system.txt unexpected: %q", seenZip["system.txt"])
	}
	if !strings.Contains(seenZip[crashLog], `K7PQ2M4X#[cut]`) || !strings.Contains(seenZip[curLog], `ZXCV1234#[cut]`) {
		t.Fatalf("zip logs missing preserved room codes: %v", seenZip)
	}

	// 3. Feedback upload.
	if err := rep.feedback(context.Background(), "problem", "could not join https://share.bellion.io/room/7K3QX9MD%23pwFb102 or MAMACO#pwFb102", "Password: pwFb102\n"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	fbRep := received[2]
	mu.Unlock()
	assertClean("feedback Log", fbRep.Log)
	if !strings.Contains(fbRep.Log, "https://share.bellion.io/room/7K3QX9MD#[cut]") || !strings.Contains(fbRep.Log, "MAMACO#[cut]") {
		t.Fatalf("feedback lost room code:\n%s", fbRep.Log)
	}
}

// TestOnDiskLoggerAndUploadRedactsAllForms verifies write-time redaction:
// realistic M9 and M13 log lines written via newLogger (Text and JSON) and
// redact.NewWriter (guard journal path) never hit disk unredacted, and survive
// intact through the upload path as well.
func TestOnDiskLoggerAndUploadRedactsAllForms(t *testing.T) {
	dir := t.TempDir()
	guardLog := "blite-20261116-010000-event.log"
	textLog := "blite-20261116-020000-555.log"
	jsonLog := "blite-20261116-030000-666.log"

	// 1. Guard journal path (redact.NewWriter in front of journal file).
	gf, err := os.OpenFile(filepath.Join(dir, guardLog), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	grw := redact.NewWriter(gf)
	guardLines := strings.Join([]string{
		fmt.Sprintf("2026-11-16T01:00:00Z guard: started blite.exe pid 555 args %q", redact.Args([]string{`bshare://join/K7PQ2M4X#diskGuard1`, "-Password", "diskCliPw2"})),
		`time=2026-11-16T01:00:01Z level=WARN msg="child stderr: wss://us%40er:p@ssDisk3@share.example.com/ws?token=diskQTok4&password=diskQPw5&key=diskQKey6"`,
		`time=2026-11-16T01:00:02Z level=WARN msg="child bare: K7PQ-2M4X#diskBare7 k7pq 2m4x #diskSpace8 https://share.bellion.io/open/ABCD%23diskPct9 struct={Room:ABCD Password:diskStruct10}"`,
		strings.Repeat("a", 65533) + " password=pwLong6",
		`guard: crash: exception 0xc0000005`,
		"",
	}, "\n")
	for i := 0; i < len(guardLines); i += 4096 {
		if _, err := grw.Write([]byte(guardLines[i:min(i+4096, len(guardLines))])); err != nil {
			t.Fatal(err)
		}
	}
	if err := grw.Flush(); err != nil {
		t.Fatal(err)
	}
	gf.Close()

	emitRound2Cases := func(l *slog.Logger) {
		// Round 2 A: secret key right after opening quote and case-insensitive / numeric attrs.
		l.Warn("x", "err", errors.New("password=hunter2"))
		l.Warn("x",
			"e1", errors.New("PASSWORD=r2UpPw1"),
			"e2", errors.New("Pw=r2Pw2"),
			"e3", errors.New("api_key=r2Ak3"),
			"e4", errors.New("token=12345"),
			"PASSWORD", "r2AttrUpPw4",
			"Pw", "r2AttrPw5",
			"api_key", "r2AttrAk6",
			"token", 12345,
			"joinPassword", "r2AttrJp7",
			"passwords", "r2AttrPws8",
			"room.password", "r2AttrRpw9",
		)
		l.Info(`msg="password=qS6"`)
		l.Info(`{"msg":"password=qS7"}`)
		l.Info(`detail="token=qS8 x"`)

		// Round 2 B: double escapes + every Round-1 Item-1 line logged as msg.
		l.Warn("x", "err", fmt.Errorf("bad link %q", "a\nhttps://share.bellion.io/j/ABCD#pwQQ"))
		l.Info("join failed:\nhttps://share.bellion.io/j/ABCD#pwNL1")
		l.Info(`err="dial failed\n\tbshare://join/K7PQ2M4X#pwNL2"`)
		l.Info(`detail="a\nK7PQ-2M4X#pwNL3"`)
		l.Info(`tab="a\tblite://join/ABCD#pwNL4"`)
		l.Info(`msg="line1\nhttps://h.example.com/j/ABCD#pw77\nline3"`)

		// Round 2 C: %+v struct with Link field (session.State.Link) and colon forms.
		st := session.State{Room: "K7PQ-2M4X", Link: "https://share.bellion.io/j/K7PQ2M4X#pwState"}
		l.Info("state", "stFmt", fmt.Sprintf("%+v", st), "stObj", st)
		l.Info("link:K7PQ2M4X#pwLinkC code:K7PQ2M4X#pwCodeC see:https://h/j/ABCD#pwSeeC")

		// Round 2 D: unicode, control bytes, and literal escapes before link or code.
		l.Info("x\u00a0https://h.example.com/j/ABCD#pwJ1")
		l.Info("x\u200bhttps://h.example.com/j/ABCD#pwJ2")
		l.Info("x\u2028K7PQ-2M4X#pwJ3")
		l.Info("x\vhttps://h.example.com/j/ABCD#pwJ4")
		l.Info("x\fK7PQ2M4X#pwJ5")
		l.Info("x\x00https://h.example.com/j/ABCD#pwJ6")
		l.Info(`x\u000ahttps://h.example.com/j/ABCD#pwJ7 x\x0aK7PQ-2M4X#pwJ8`)

		// Round 2 F, H, Also:
		l.Info(`cfg {"password":4821}`)
		l.Info(`pass=482913 Pass:true key=0xDEADBEEF key=Inf key=NaN key=1_000 key=100000`)
		l.Info(`password = r2SpEq10 password :r2SpCol11 joinPassword=r2Jp12 passwords=r2Pws13 room.password=r2Rpw14 password=pw"58 ok`)
	}

	// 2. Blite text logger (ReplaceAttr + redact.NewWriter).
	tf, err := os.OpenFile(filepath.Join(dir, textLog), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	tlog, trw := newLogger(tf, false, "dev")
	tlog.Info("session attrs",
		"Link", "https://share.bellion.io/any/7K3QX9MD#diskAttrLink11",
		"Password", "diskAttrPw12",
		"roomPassword", "diskAttrRp13",
		"room_password", "diskAttrRp14",
		"PW", "diskAttrPw15",
		"Pass", "diskAttrPass16",
		"Token", "diskAttrTok17",
		"Key", "diskAttrKey18",
		"SECRET", "diskAttrSec19",
		"api_key", "diskAttrAk27",
		"access_token", "diskAttrAt28",
		"x-token", "diskAttrXt29",
		"passwd", "diskAttrPwd30",
		"key", true,
		"pass", 482913,
		"peer", "alice#7F2A",
		"name", "martin#0A1B",
		"ch", "chan#3",
		"rawLink", `https://share.bellion.io/r/7K3QX9MD#my "escaped" diskQuote20`,
		"req", fmt.Sprintf("%+v", proto.Join{T: "join", Room: "7K3QX9MD", Password: "diskJoinStruct21"}),
	)
	emitRound2Cases(tlog)
	tlog.Info(strings.Repeat("a", 65520) + ` pw="secretPartOne two"`)
	tlog.Info(strings.Repeat("a", 65455) + ` pw="secretPartMid three"`)
	tlog.Info(cleanExit)
	trw.Flush()
	tf.Close()

	// 3. Blite JSON logger (ReplaceAttr + redact.NewWriter).
	jfPath := filepath.Join(dir, jsonLog)
	jf, err := os.OpenFile(jfPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	jlog, jrw := newLogger(jf, true, "dev")
	jlog.Warn("json check",
		"ROOM_PASSWORD", "diskJsonRp22",
		"secret", "diskJsonSec23",
		"password", 123456,
		"key", 12,
		"pass", true,
		"link", "https://share.bellion.io/j/7K3QX9MD#diskJsonLink31",
		"detail", "inputs: share.bellion.io/custom/MAMACO%23diskJsonPct24 and GAME#diskJsonBare25",
		"joinObj", proto.Join{Room: "MAMACO", Password: "diskJsonObjPw26"},
	)
	emitRound2Cases(jlog)
	jlog.Info("pad-"+strings.Repeat("a", 65500), "password", "pw Js Tr jsonTruncSecret")
	jrw.Flush()
	jf.Close()

	secrets := []string{
		"diskGuard1", "diskCliPw2", "ssDisk3", "diskQTok4", "diskQPw5", "diskQKey6",
		"diskBare7", "diskSpace8", "diskPct9", "diskStruct10", "pwLong6",
		"diskAttrLink11", "diskAttrPw12", "diskAttrRp13", "diskAttrRp14", "diskAttrPw15",
		"diskAttrPass16", "diskAttrTok17", "diskAttrKey18", "diskAttrSec19", "diskQuote20", "diskJoinStruct21",
		"diskAttrAk27", "diskAttrAt28", "diskAttrXt29", "diskAttrPwd30",
		"diskJsonRp22", "diskJsonSec23", "123456", "diskJsonLink31", "diskJsonPct24", "diskJsonBare25", "diskJsonObjPw26",
		"hunter2", "r2UpPw1", "r2Pw2", "r2Ak3", "12345", "r2AttrUpPw4", "r2AttrPw5", "r2AttrAk6",
		"r2AttrJp7", "r2AttrPws8", "r2AttrRpw9", "qS6", "qS7", "qS8",
		"pwQQ", "pwNL1", "pwNL2", "pwNL3", "pwNL4", "pw77",
		"pwState", "pwLinkC", "pwCodeC", "pwSeeC",
		"pwJ1", "pwJ2", "pwJ3", "pwJ4", "pwJ5", "pwJ6", "pwJ7", "pwJ8",
		"4821", "482913", "0xDEADBEEF", "DEADBEEF", "1_000", "100000",
		"r2SpEq10", "r2SpCol11", "r2Jp12", "r2Pws13", "r2Rpw14", `pw"58`, `pw\"58`,
		"secretPartOne", "secretPartMid", "jsonTruncSecret", "pw Js Tr",
	}

	// Verify EVERY file on disk has zero secrets before any upload runs.
	diskTexts := map[string]string{}
	for _, name := range []string{guardLog, textLog, jsonLog} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		txt := string(raw)
		diskTexts[name] = txt
		for _, sec := range secrets {
			if strings.Contains(txt, sec) {
				t.Fatalf("on-disk file %s leaked secret %q:\n%s", name, sec, txt)
			}
		}
	}
	for _, keep := range []string{
		"Link=https://share.bellion.io/any/7K3QX9MD#[cut]",
		"key=true", "pass=[cut]", `err="password=[cut]"`, "peer=alice#7F2A", "name=martin#0A1B", "ch=chan#3",
		`msg="join failed:\nhttps://share.bellion.io/j/ABCD#[cut]"`,
		"https://share.bellion.io/j/K7PQ2M4X#[cut]",
		"https://h/j/ABCD#[cut]",
	} {
		if !strings.Contains(diskTexts[textLog], keep) {
			t.Fatalf("textLog missing %q:\n%s", keep, diskTexts[textLog])
		}
	}
	for _, keep := range []string{
		`"key":12`, `"pass":"[cut]"`, `"err":"password=[cut]"`, `"link":"https://share.bellion.io/j/7K3QX9MD#[cut]"`,
		`"msg":"join failed:\nhttps://share.bellion.io/j/ABCD#[cut]"`,
		"https://share.bellion.io/j/K7PQ2M4X#[cut]",
		"https://h/j/ABCD#[cut]",
	} {
		if !strings.Contains(diskTexts[jsonLog], keep) {
			t.Fatalf("jsonLog missing %q:\n%s", keep, diskTexts[jsonLog])
		}
	}

	// Verify upload path over those on-disk logs also succeeds and keeps room codes.
	var (
		mu       sync.Mutex
		received []proto.Report
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rep proto.Report
		if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		received = append(received, rep)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ts.Close()

	prevLogs := logState()
	setLogs(func(l *logInfo) { *l = logInfo{Dir: dir, File: jfPath} })
	defer setLogs(func(l *logInfo) { *l = prevLogs })

	rep := &reporter{
		server: "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws",
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		name:   func() string { return "bob#9A1C" },
		base:   proto.Report{Platform: "linux", Version: "dev", Build: 1},
	}
	rep.previous()
	if err := rep.send(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 {
		t.Fatalf("uploaded reports = %d, want 2", len(received))
	}
	for i, r := range received {
		for _, sec := range secrets {
			if strings.Contains(r.Log, sec) {
				t.Fatalf("uploaded report %d leaked secret %q:\n%s", i, sec, r.Log)
			}
		}
	}
	if !strings.Contains(received[1].Log, "share.bellion.io/custom/MAMACO#[cut]") || !strings.Contains(received[1].Log, "GAME#[cut]") {
		t.Fatalf("uploaded user report lost preserved room codes:\n%s", received[1].Log)
	}
}

// crashTestLog is a crashed run's log: appBytes of app lines, a panic, the
// crashing goroutine, main, similar goroutines with one stack, distinct
// ones, and the guard's lines.
func crashTestLog(appBytes, similar, distinct int) string {
	var b strings.Builder
	for i := 0; b.Len() < appBytes; i++ {
		fmt.Fprintf(&b, "time=2026-11-15T01:00:00Z level=INFO msg=\"app line %d\" room=7K3QX9MD\n", i)
	}
	b.WriteString("time=2026-11-15T01:00:01Z level=INFO msg=\"joined https://share.bellion.io/j/7K3QX9MD#pwCrashSecret99\"\n")
	b.WriteString("time=2026-11-15T01:00:02Z level=INFO msg=\"LAST APP LINE\"\n")
	b.WriteString("panic: runtime error: invalid memory address or nil pointer dereference\n")
	b.WriteString("[signal 0xc0000005 code=0x0 addr=0x0 pc=0x7ff6a1b2c3d4]\n\n")
	b.WriteString("goroutine 4242 [running]:\nblite/internal/nui.(*UI).CRASHFRAME(0xc000123456)\n\tC:/src/blite/internal/nui/nui.go:200 +0x1d\ncreated by blite/internal/nui.Run in goroutine 1\n\tC:/src/blite/internal/nui/nui.go:100 +0x2a\n\n")
	b.WriteString("goroutine 1 [syscall, locked to thread]:\nmain.MAINFRAME()\n\tC:/src/blite/cmd/blite/main.go:409 +0x99\n\n")
	for i := range similar {
		fmt.Fprintf(&b, "goroutine %d [chan receive, %d minutes]:\nblite/internal/rtc.(*peer).readLoop(0xc%09x, {0x%x, 0x1})\n\tC:/src/blite/internal/rtc/peer.go:88 +0x%x\ncreated by blite/internal/rtc.newPeer in goroutine %d\n\tC:/src/blite/internal/rtc/peer.go:40 +0x55\n\n", 100+i, i%7, i, i*16, i, 5+i)
	}
	for i := range distinct {
		fmt.Fprintf(&b, "goroutine %d [select]:\nblite/internal/x.worker%d(0xc000%06x)\n\tC:/src/blite/internal/x/x.go:%d +0x10\ncreated by blite/internal/x.start in goroutine 1\n\tC:/src/blite/internal/x/x.go:9 +0x20\n\n", 10000+i, i, i, i)
	}
	b.WriteString("2026-11-15T01:00:03.000Z guard: crash: exit code 2\n")
	b.WriteString("2026-11-15T01:00:04.000Z guard: restarting blite.exe\n")
	return b.String()
}

// a crash report keeps the panic, the crashing goroutine, main,
// the app's last lines and the guard's, whatever the goroutine count.
func TestCrashLogSections(t *testing.T) {
	log := crashTestLog(2<<20, 500, 3000)
	got := crashLog(log, reportTail)
	if len(got) > reportTail {
		t.Fatalf("report is %d bytes, over %d", len(got), reportTail)
	}
	for _, want := range []string{
		"panic: runtime error: invalid memory address",
		"[signal 0xc0000005",
		"goroutine 4242 [running]:\nblite/internal/nui.(*UI).CRASHFRAME(",
		"goroutine 1 [syscall, locked to thread]:\nmain.MAINFRAME()",
		"LAST APP LINE",
		"guard: crash: exit code 2",
		"guard: restarting blite.exe",
		"(500 goroutines with this stack)\ngoroutine 100 [chan receive",
		"distinct stacks dropped to fit the report)",
		"x.worker0(",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q", want)
		}
	}
	if strings.Contains(got, "goroutine 101 [") {
		t.Error("a duplicate goroutine was sent in full")
	}
	if strings.Contains(got, "pwCrashSecret99") {
		t.Error("share link password not redacted")
	}
	if !strings.Contains(got, "share.bellion.io/j/7K3QX9MD#") {
		t.Error("the redacted share link line is missing from the app lines")
	}
	// The app section is the end of the app log, about crashAppLines of it.
	i, j := strings.Index(got, "LAST APP LINE"), strings.Index(got, "panic:")
	if i < 0 || j < i {
		t.Fatal("app lines don't come before the traceback")
	}
	if app := got[:i]; len(app) < crashAppLines-512 || len(app) > crashAppLines+512 {
		t.Errorf("app section %d bytes, want about %d", len(app), crashAppLines)
	}
	// The guard's lines are last.
	if !strings.HasSuffix(got, "guard: restarting blite.exe\n") {
		t.Errorf("report doesn't end with the guard lines: %q", got[max(0, len(got)-200):])
	}
}

// A dump small enough to fit is sent whole, without a "dropped" note.
func TestCrashLogFitsWhole(t *testing.T) {
	got := crashLog(crashTestLog(10<<10, 20, 5), reportTail)
	if strings.Contains(got, "dropped") {
		t.Error("goroutines dropped from a small dump")
	}
	for i := range 5 {
		if !strings.Contains(got, fmt.Sprintf("x.worker%d(", i)) {
			t.Errorf("worker%d missing", i)
		}
	}
	if !strings.Contains(got, "(20 goroutines with this stack)") {
		t.Error("no dedup note")
	}
}

// Without a traceback (killed, a guard-only crash) the report is the log's
// last whole lines, redacted, within the limit.
func TestCrashLogNoTraceback(t *testing.T) {
	var b strings.Builder
	for i := 0; b.Len() < 600<<10; i++ {
		fmt.Fprintf(&b, "time=2026-11-15T01:00:00Z level=INFO msg=\"line %d\"\n", i)
	}
	b.WriteString("time=2026-11-15T01:00:01Z level=INFO msg=\"joined https://share.bellion.io/j/7K3QX9MD#pwTailSecret88\"\n")
	b.WriteString("2026-11-15T01:00:03.000Z guard: crash: exception 0xc0000005\n")
	got := crashLog(b.String(), reportTail)
	if len(got) > reportTail {
		t.Fatalf("%d bytes, over %d", len(got), reportTail)
	}
	if !strings.HasPrefix(got, "time=") || !strings.HasSuffix(got, "guard: crash: exception 0xc0000005\n") {
		t.Fatalf("not the log's last whole lines: %q ... %q", got[:40], got[len(got)-40:])
	}
	if strings.Contains(got, "pwTailSecret88") {
		t.Fatal("tail not redacted")
	}
}

// The fallback's cut used to land inside a share link: "9MD#pw..." alone
// isn't a link any more and its password survived redaction. Redaction can
// also make the text longer than the limit.
func TestCrashLogFallbackCutInLink(t *testing.T) {
	link := `time=2026-11-15T01:00:01Z level=INFO msg="joined https://share.bellion.io/j/7K3QX9MD#pwMidLink77"` + "\n"
	k := strings.Index(link, "9MD#")
	var b strings.Builder
	for i := 0; b.Len() < 300<<10; i++ {
		fmt.Fprintf(&b, "time=2026-11-15T01:00:00Z level=INFO msg=\"before %d\"\n", i)
	}
	b.WriteString(link)
	after := reportTail - (len(link) - k) // bytes after the link line, so the cut falls on "9MD#"
	var f strings.Builder
	for f.Len() < after {
		f.WriteString("time=2026-11-15T01:00:02Z level=INFO msg=\"after\"\n")
	}
	filler := f.String()[:after-1] + "\n"
	b.WriteString(filler)
	log := b.String()
	if !strings.HasPrefix(log[len(log)-reportTail:], "9MD#pwMidLink77") {
		t.Fatalf("test setup: cut at %q", log[len(log)-reportTail:][:20])
	}
	if naive := redact.Text(log[len(log)-reportTail:]); !strings.Contains(naive, "pwMidLink77") {
		t.Log("note: the redactor now catches the cut link on its own")
	}
	got := crashLog(log, reportTail)
	if strings.Contains(got, "pwMidLink77") {
		t.Fatal("password from a cut link survived")
	}
	if len(got) > reportTail || !strings.HasPrefix(got, "time=") {
		t.Fatalf("%d bytes, starts %q", len(got), got[:min(40, len(got))])
	}

	// Redaction that grows the text ("#p" -> "#[cut]") still fits.
	var g strings.Builder
	for g.Len() < 2*reportTail {
		g.WriteString("x")
		for range 40 {
			g.WriteString(" https://share.bellion.io/j/7K3QX9MD#p")
		}
		g.WriteString("\n")
	}
	got = crashLog(g.String(), reportTail)
	if len(got) > reportTail {
		t.Fatalf("redacted fallback is %d bytes, over %d", len(got), reportTail)
	}
	if strings.Contains(got, "#p ") || strings.Contains(got, "#p\n") {
		t.Fatal("fragments not redacted")
	}
}

// app is n bytes of app log lines ending with "LAST APP LINE".
func appLines(n int) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "time=2026-11-15T01:00:00Z level=INFO msg=\"app line %d\"\n", i)
	}
	b.WriteString("time=2026-11-15T01:00:02Z level=INFO msg=\"LAST APP LINE\"\n")
	return b.String()
}

// A Windows exception (Go's winthrow, the guard's crash class) prints its
// header and the crashing frames without a goroutine line, and no
// goroutine is [running].
func TestCrashLogWindowsException(t *testing.T) {
	var b strings.Builder
	b.WriteString(appLines(1 << 20))
	b.WriteString("Exception 0xc0000005 0x0 0x18 0x7ffb12345678\nPC=0x7ffb12345678\nsignal arrived during external code execution\n\n")
	b.WriteString("runtime.cgocall(0x7ff6a1000000, 0xc000123456)\n\tC:/go/src/runtime/cgocall.go:167 +0x3e\ngithub.com/go-gl/glfw/v3.3/glfw.GetWin32Window.CRASHFRAME(0x0)\n\tC:/m/glfw/native_windows.go:20 +0x2b\n\n")
	b.WriteString("goroutine 1 gp=0xc000002380 m=0 mp=0x7ff6a2000000 [syscall, locked to thread]:\nmain.MAINFRAME()\n\tC:/src/blite/cmd/blite/main.go:409 +0x99\n\n")
	for i := range 4000 {
		if i%2 == 0 {
			fmt.Fprintf(&b, "goroutine %d gp=0xc%09x m=nil [chan receive]:\nblite/internal/rtc.(*peer).readLoop(0xc%09x)\n\tC:/src/blite/internal/rtc/peer.go:88 +0x%x\ncreated by blite/internal/rtc.newPeer in goroutine 5\n\tC:/src/blite/internal/rtc/peer.go:40 +0x55\n\n", 100+i, i, i, i)
		} else {
			fmt.Fprintf(&b, "goroutine %d gp=0xc%09x m=nil [select]:\nblite/internal/x.worker%d(0xc000%06x)\n\tC:/src/blite/internal/x/x.go:%d +0x10\n\n", 100+i, i, i, i, i)
		}
	}
	b.WriteString("rax     0x0\nrbx     0xc000123456\n")
	b.WriteString("2026-11-15T01:00:03.000Z guard: crash: exception 0xc0000005\n")
	got := crashLog(b.String(), reportTail)
	if len(got) > reportTail {
		t.Fatalf("%d bytes, over %d", len(got), reportTail)
	}
	for _, want := range []string{
		"===== traceback: header, crashing goroutine, main\nException 0xc0000005",
		"PC=0x7ffb12345678\nsignal arrived during external code execution\n",
		"runtime.cgocall(", "GetWin32Window.CRASHFRAME(0x0)",
		"main.MAINFRAME()",
		"LAST APP LINE",
		"(2000 goroutines with this stack)",
		"distinct stacks dropped to fit the report)",
		"guard: crash: exception 0xc0000005",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q", want)
		}
	}
	// The crash frames are in the traceback section, before the others.
	if strings.Index(got, "CRASHFRAME") > strings.Index(got, "===== other goroutines") {
		t.Error("crash frames not in the traceback section")
	}
}

// A recovered panic earlier in the run (net/http's "http: panic serving"
// with a [running] goroutine) must not be taken for the crash.
func TestCrashLogAnchorsOnLastCrash(t *testing.T) {
	var b strings.Builder
	b.WriteString(appLines(100 << 10))
	b.WriteString("2026/11/15 01:00:00 http: panic serving 127.0.0.1:5555: early boom\ngoroutine 55 [running]:\nnet/http.(*conn).serve.func1()\n\tC:/go/src/net/http/server.go:1947 +0xbe\npanic({0x7ff6a1, 0xc0001})\n\tC:/go/src/runtime/panic.go:792 +0x132\n")
	b.WriteString(appLines(200 << 10))
	b.WriteString("panic: REAL CRASH\n\ngoroutine 9 [running]:\nblite/internal/x.CRASHFRAME()\n\tC:/x.go:1 +0x1\n\ngoroutine 1 [select]:\nmain.MAINFRAME()\n\tC:/main.go:2 +0x2\n\n")
	got := crashLog(b.String(), reportTail)
	for _, want := range []string{"panic: REAL CRASH", "CRASHFRAME", "MAINFRAME", "LAST APP LINE"} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q", want)
		}
	}
	if strings.Contains(got, "early boom") || strings.Contains(got, "goroutine 55") {
		t.Error("the earlier recovered panic was taken for the crash")
	}
	if i := strings.Index(got, "panic: REAL CRASH"); i < crashAppLines-512 {
		t.Errorf("only %d bytes of app log before the crash", i)
	}
}

// A huge panic message is capped so the crashing goroutine and main fit.
func TestCrashLogHugePanicMessage(t *testing.T) {
	var b strings.Builder
	b.WriteString(appLines(100 << 10))
	b.WriteString("panic: " + strings.Repeat("x", 300<<10) + "\n\n")
	b.WriteString("goroutine 9 [running]:\nblite/internal/x.CRASHFRAME()\n\tC:/x.go:1 +0x1\n\ngoroutine 1 [select]:\nmain.MAINFRAME()\n\tC:/main.go:2 +0x2\n\n")
	b.WriteString("2026-11-15T01:00:03.000Z guard: crash: exit code 2\n")
	got := crashLog(b.String(), reportTail)
	if len(got) > reportTail {
		t.Fatalf("%d bytes, over %d", len(got), reportTail)
	}
	for _, want := range []string{"panic: xxxx", "bytes cut)", "CRASHFRAME", "MAINFRAME", "LAST APP LINE", "guard: crash: exit code 2"} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q", want)
		}
	}
}
