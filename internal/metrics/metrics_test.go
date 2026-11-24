package metrics

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sample(i int) Record {
	r := Record{TimeMs: 1_700_000_000_000 + int64(i)*1000, Captured: uint16(60 - i%3), Offered: 58, QueueIn: 57, QueueOut: 57,
		Encoded: uint16(16 + i), MaxSendUs: 123456, CPUBusy: 87, HighFPS: 60, Height: 1080, Flags: FlagSharing | FlagPreview,
		VideoWindows: 2, LowKbps: 300, UpKbps: 9000, Peers: 5}
	for p := range MaxPeers {
		r.SetPeer(p, 1000*(p+1))
	}
	return r
}

func TestRoundTrip(t *testing.T) {
	var want []Record
	for i := range 10 {
		want = append(want, sample(i))
	}
	want[3].Captured = Unknown
	b := append(Header(), Encode(want)...)
	if len(b) != len(Header())+10*RecordSize() {
		t.Fatalf("%d bytes", len(b))
	}
	if RecordSize() > 64 {
		t.Fatalf("records grew to %d bytes", RecordSize())
	}
	f, err := Decode(bytes.NewReader(b[:len(b)-5])) // the last record cut short
	if err != nil {
		t.Fatal(err)
	}
	got := f.Records()
	if f.Version != Version || len(got) != 9 {
		t.Fatalf("version %d, %d records", f.Version, len(got))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("record %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
	var csv strings.Builder
	f.CSV(&csv)
	lines := strings.Split(strings.TrimSpace(csv.String()), "\n")
	if len(lines) != 10 || !strings.HasPrefix(lines[0], "timeMs,captured,") || !strings.HasPrefix(lines[1], "1700000000000,60,58,") {
		t.Fatalf("csv:\n%s", csv.String())
	}
}

// TestOtherVersion: a file with fields this version doesn't know (and
// without some it does) still decodes by name.
func TestOtherVersion(t *testing.T) {
	var b bytes.Buffer
	b.WriteString(Magic)
	binary.Write(&b, binary.LittleEndian, [3]uint16{2, 8 + 2 + 1, 3})
	for _, f := range []struct {
		k    byte
		name string
	}{{0x88, "timeMs"}, {2, "encoded"}, {0x81, "future"}} {
		b.Write([]byte{f.k, byte(len(f.name))})
		b.WriteString(f.name)
	}
	binary.Write(&b, binary.LittleEndian, int64(5))
	binary.Write(&b, binary.LittleEndian, uint16(30))
	b.WriteByte(0xff)
	f, err := Decode(&b)
	if err != nil {
		t.Fatal(err)
	}
	if f.Rows[0][2] != -1 {
		t.Fatalf("signed byte: %d", f.Rows[0][2])
	}
	r := f.Records()[0]
	if r.TimeMs != 5 || r.Encoded != 30 || r.Captured != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestDecodeRejects(t *testing.T) {
	if _, err := Decode(strings.NewReader("hello world, not metrics")); err == nil {
		t.Fatal("decoded garbage")
	}
	h := Header()
	h[10]++ // record size no longer matches the fields
	if _, err := Decode(bytes.NewReader(h)); err == nil {
		t.Fatal("decoded a bad header")
	}
}

func TestRecorder(t *testing.T) {
	dir := t.TempDir()
	r := NewRecorder(dir)
	for i := range 5 {
		r.Add(sample(i))
	}
	if r.Path() != "" {
		t.Fatal("wrote before a flush")
	}
	if err := r.Flush(); err != nil {
		t.Fatal(err)
	}
	r.Add(sample(5))
	r.Close()
	r.Add(sample(6)) // dropped
	r.Flush()
	names := Newest(dir, 5)
	if len(names) != 1 || !IsFile(names[0]) || !strings.Contains(names[0], fmt.Sprint(os.Getpid())) {
		t.Fatalf("files %v", names)
	}
	fh, _ := os.Open(filepath.Join(dir, names[0]))
	defer fh.Close()
	f, err := Decode(fh)
	if err != nil {
		t.Fatal(err)
	}
	if rs := f.Records(); len(rs) != 6 || rs[5] != sample(5) {
		t.Fatalf("%d records", len(rs))
	}
	var nilRec *Recorder
	nilRec.Add(sample(0))
	nilRec.Flush()
	nilRec.Close()
}

func TestPrune(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	touch := func(name string, age time.Duration) {
		p := filepath.Join(dir, name)
		os.WriteFile(p, nil, 0o600)
		os.Chtimes(p, now.Add(-age), now.Add(-age))
	}
	touch("metrics-20200101-000000-1.bin", 8*24*time.Hour)
	touch("blite-x.log", 30*24*time.Hour)
	for i := range keepFiles + 2 {
		touch(fmt.Sprintf("metrics-20260101-%06d-1.bin", i), time.Minute)
	}
	Prune(dir, now)
	got := Newest(dir, 100)
	if len(got) != keepFiles || got[0] != "metrics-20260101-000002-1.bin" {
		t.Fatalf("kept %v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "blite-x.log")); err != nil {
		t.Fatal("pruned a log")
	}
}
