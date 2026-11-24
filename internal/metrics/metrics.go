// Package metrics keeps a share's per-second readings in small binary
// files next to the logs (metrics-<time>-<pid>.bin), for "Send logs": the
// log's 30 s share stats show that frames went missing, these show the
// second it happened.
//
// A file is a header, then fixed-size little-endian records:
//
//	magic   [8]byte "BLMETRIC"
//	version uint16
//	size    uint16 bytes per record
//	fields  uint16 number of fields, then per field:
//	        kind uint8 (byte size, |0x80 when signed), name length uint8, name
//
// The field list makes a file readable without this package's version:
// Decode reads any of them, and a field it doesn't know is skipped.
package metrics

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
)

const (
	Magic   = "BLMETRIC"
	Version = 1
)

// Unknown is a count's value when the source doesn't have that counter.
const Unknown = 0xffff

// Flags bits.
const (
	FlagSharing = 1 << iota
	FlagPreview // our own preview shows
	FlagIdle    // the capture is paused: nobody watches
)

// MaxPeers is how many peers' upload rates a record has room for.
const MaxPeers = 4

// Record is one second. Frame counts are for that second; Unknown when
// not known (or not sharing).
type Record struct {
	TimeMs       int64  `m:"timeMs"` // Unix milliseconds
	Captured     uint16 `m:"captured"`
	Offered      uint16 `m:"offered"`
	QueueIn      uint16 `m:"queueIn"`
	QueueOut     uint16 `m:"queueOut"`
	Encoded      uint16 `m:"encoded"`
	MaxSendUs    uint32 `m:"maxSendUs"` // the slowest frame handed to the peers
	CPUBusy      uint8  `m:"cpuBusy"`   // percent
	HighFPS      uint8  `m:"highFPS"`   // the frame rate the share aims for
	Height       uint16 `m:"height"`    // the share's resolution (720, 1080, 2160)
	Flags        uint8  `m:"flags"`
	VideoWindows uint8  `m:"videoWindows"`
	LowKbps      uint16 `m:"lowKbps"`
	UpKbps       uint32 `m:"upKbps"` // all peers
	Peers        uint8  `m:"peers"`
	Peer0Kbps    uint16 `m:"peer0Kbps"` // per peer (by ID), the first MaxPeers
	Peer1Kbps    uint16 `m:"peer1Kbps"`
	Peer2Kbps    uint16 `m:"peer2Kbps"`
	Peer3Kbps    uint16 `m:"peer3Kbps"`
}

// SetPeer sets peer i's upload rate (i < MaxPeers).
func (r *Record) SetPeer(i int, kbps int) {
	v := Clamp16(kbps)
	switch i {
	case 0:
		r.Peer0Kbps = v
	case 1:
		r.Peer1Kbps = v
	case 2:
		r.Peer2Kbps = v
	case 3:
		r.Peer3Kbps = v
	}
}

// Clamp16 fits n into a uint16 field (negative: Unknown).
func Clamp16(n int) uint16 {
	if n < 0 || n >= Unknown {
		return Unknown
	}
	return uint16(n)
}

// Field is one column of a file.
type Field struct {
	Name   string
	Size   int
	Signed bool
}

var (
	fields     []Field
	recordSize int
	header     []byte
)

func init() {
	t := reflect.TypeFor[Record]()
	for i := range t.NumField() {
		f := t.Field(i)
		fields = append(fields, Field{Name: f.Tag.Get("m"), Size: int(f.Type.Size()), Signed: f.Type.Kind() >= reflect.Int && f.Type.Kind() <= reflect.Int64})
		recordSize += int(f.Type.Size())
	}
	var b bytes.Buffer
	b.WriteString(Magic)
	binary.Write(&b, binary.LittleEndian, [3]uint16{Version, uint16(recordSize), uint16(len(fields))})
	for _, f := range fields {
		k := byte(f.Size)
		if f.Signed {
			k |= 0x80
		}
		b.Write([]byte{k, byte(len(f.Name))})
		b.WriteString(f.Name)
	}
	header = b.Bytes()
}

// Fields is this version's field list.
func Fields() []Field { return append([]Field(nil), fields...) }

// RecordSize is the bytes per record.
func RecordSize() int { return recordSize }

// Header is what a file starts with.
func Header() []byte { return append([]byte(nil), header...) }

func (r *Record) encode(b []byte) []byte {
	b, _ = binary.Append(b, binary.LittleEndian, r)
	return b
}

// Encode is the records as they follow the header in a file.
func Encode(rs []Record) []byte {
	b := make([]byte, 0, len(rs)*recordSize)
	for i := range rs {
		b = rs[i].encode(b)
	}
	return b
}

// File is a decoded metrics file: its columns and, per record, a value per
// column.
type File struct {
	Version int
	Fields  []Field
	Rows    [][]int64
}

// Decode reads a metrics file. A record cut short at the end (a crash while
// writing) is left out.
func Decode(r io.Reader) (*File, error) {
	br := bufio.NewReader(r)
	var magic [8]byte
	if _, err := io.ReadFull(br, magic[:]); err != nil || string(magic[:]) != Magic {
		return nil, errors.New("not a metrics file")
	}
	var h [3]uint16
	if err := binary.Read(br, binary.LittleEndian, &h); err != nil {
		return nil, err
	}
	f := &File{Version: int(h[0])}
	size := 0
	for range h[2] {
		var kn [2]byte
		if _, err := io.ReadFull(br, kn[:]); err != nil {
			return nil, err
		}
		name := make([]byte, kn[1])
		if _, err := io.ReadFull(br, name); err != nil {
			return nil, err
		}
		fl := Field{Name: string(name), Size: int(kn[0] & 0x7f), Signed: kn[0]&0x80 != 0}
		switch fl.Size {
		case 1, 2, 4, 8:
		default:
			return nil, fmt.Errorf("field %s: size %d", fl.Name, fl.Size)
		}
		f.Fields = append(f.Fields, fl)
		size += fl.Size
	}
	if size != int(h[1]) {
		return nil, fmt.Errorf("fields add up to %d bytes, records have %d", size, h[1])
	}
	rec := make([]byte, size)
	for {
		if _, err := io.ReadFull(br, rec); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return f, nil
			}
			return f, err
		}
		row := make([]int64, len(f.Fields))
		off := 0
		for i, fl := range f.Fields {
			row[i] = value(rec[off:off+fl.Size], fl.Signed)
			off += fl.Size
		}
		f.Rows = append(f.Rows, row)
	}
}

func value(b []byte, signed bool) int64 {
	le := binary.LittleEndian
	switch len(b) {
	case 1:
		if signed {
			return int64(int8(b[0]))
		}
		return int64(b[0])
	case 2:
		if signed {
			return int64(int16(le.Uint16(b)))
		}
		return int64(le.Uint16(b))
	case 4:
		if signed {
			return int64(int32(le.Uint32(b)))
		}
		return int64(le.Uint32(b))
	}
	return int64(le.Uint64(b))
}

// Records is f's rows as Records, by field name (a field this version
// doesn't have is left out, one it has and f doesn't stays zero).
func (f *File) Records() []Record {
	idx := map[string]int{}
	for i, fl := range f.Fields {
		idx[fl.Name] = i
	}
	t := reflect.TypeFor[Record]()
	out := make([]Record, len(f.Rows))
	for ri, row := range f.Rows {
		v := reflect.ValueOf(&out[ri]).Elem()
		for i := range t.NumField() {
			if c, ok := idx[t.Field(i).Tag.Get("m")]; ok {
				if fv := v.Field(i); fv.CanInt() {
					fv.SetInt(row[c])
				} else {
					fv.SetUint(uint64(row[c]))
				}
			}
		}
	}
	return out
}

// CSV writes f as comma-separated values with a header line.
func (f *File) CSV(w io.Writer) error {
	bw := bufio.NewWriter(w)
	names := make([]string, len(f.Fields))
	for i, fl := range f.Fields {
		names[i] = fl.Name
	}
	fmt.Fprintln(bw, strings.Join(names, ","))
	for _, row := range f.Rows {
		for i, v := range row {
			if i > 0 {
				bw.WriteByte(',')
			}
			fmt.Fprint(bw, v)
		}
		bw.WriteByte('\n')
	}
	return bw.Flush()
}
