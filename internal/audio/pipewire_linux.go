package audio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ShareSinkName is the virtual sink Blite captures from.
const ShareSinkName = "blite-share"

// pwObject is the subset of `pw-dump` output Blite reads.
type pwObject struct {
	ID   int    `json:"id"`
	Type string `json:"type"`
	Info struct {
		Props map[string]any `json:"props"`
	} `json:"info"`
}

func (o pwObject) str(k string) string {
	switch v := o.Info.Props[k].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

func (o pwObject) num(k string) int {
	n, _ := strconv.Atoi(o.str(k))
	return n
}

func run(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out, nil
}

func dump(ctx context.Context) ([]pwObject, error) {
	out, err := run(ctx, 5*time.Second, "pw-dump")
	if err != nil {
		return nil, err
	}
	var objs []pwObject
	return objs, json.Unmarshal(out, &objs)
}

func appKey(name string, pid int) string { return name + "#" + strconv.Itoa(pid) }

// appName is what the UI shows and what keys are built from.
func appName(o pwObject) string {
	if n := o.str("application.name"); n != "" {
		return n
	}
	return o.str("node.name")
}

// Sharer manages the blite-share sink and keeps the links into it matching the
// selection (re-checked every 2 s, so apps that restart their stream follow).
type Sharer struct {
	mu     sync.Mutex
	module string // pactl module index of the sink
	sel    Selection
	ownPID int
	cancel context.CancelFunc
	done   chan struct{}
}

// NewSharer creates the virtual sink (removing a stale one from a crashed run)
// and starts keeping links in line with the selection.
func NewSharer() (*Sharer, error) {
	ctx := context.Background()
	unloadStale(ctx)
	// Note: don't set priority.driver=0 here; the sink then never drives the
	// graph and the capture stays silent.
	out, err := run(ctx, 5*time.Second, "pactl", "load-module", "module-null-sink",
		"sink_name="+ShareSinkName,
		`sink_properties=node.description="Blite share" device.description="Blite share" priority.session=0`)
	if err != nil {
		return nil, fmt.Errorf("create %s sink (is pipewire-pulse running?): %w", ShareSinkName, err)
	}
	cctx, cancel := context.WithCancel(ctx)
	s := &Sharer{module: strings.TrimSpace(string(out)), sel: Selection{Mode: "none"},
		ownPID: os.Getpid(), cancel: cancel, done: make(chan struct{})}
	go s.loop(cctx)
	return s, nil
}

func unloadStale(ctx context.Context) {
	out, err := run(ctx, 5*time.Second, "pactl", "list", "short", "modules")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && f[1] == "module-null-sink" && strings.Contains(line, "sink_name="+ShareSinkName) {
			run(ctx, 5*time.Second, "pactl", "unload-module", f[0])
		}
	}
}

// Capture returns the GStreamer source that records the shared mix.
func (s *Sharer) Capture() CaptureSource {
	return CaptureSource(fmt.Sprintf("pipewiresrc target-object=%s stream-properties=\"props,stream.capture.sink=true,node.dont-reconnect=true\" do-timestamp=true", ShareSinkName))
}

// Select changes what is shared and applies it immediately.
func (s *Sharer) Select(sel Selection) error {
	switch sel.Mode {
	case "none", "system", "apps":
	default:
		return errors.New("unknown audio mode")
	}
	s.mu.Lock()
	s.sel = sel
	s.mu.Unlock()
	return s.sync(context.Background())
}

func (s *Sharer) loop(ctx context.Context) {
	defer close(s.done)
	t := time.NewTicker(2 * time.Second) // picks up apps that start or restart their stream
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sync(ctx)
		}
	}
}

// sync makes the links into blite-share match the selection.
func (s *Sharer) sync(ctx context.Context) error {
	objs, err := dump(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	sel := s.sel
	s.mu.Unlock()

	nodes := map[int]pwObject{}
	shareID := -1
	for _, o := range objs {
		if o.Type == "PipeWire:Interface:Node" {
			nodes[o.ID] = o
			if o.str("node.name") == ShareSinkName {
				shareID = o.ID
			}
		}
	}
	if shareID < 0 {
		return errors.New("blite-share sink is gone")
	}
	want := map[string]bool{}
	for _, k := range sel.Apps {
		want[k] = true
	}
	included := func(n pwObject) bool {
		if n.str("media.class") != "Stream/Output/Audio" || n.num("application.process.id") == s.ownPID {
			return false
		}
		switch sel.Mode {
		case "system":
			return true
		case "apps":
			return want[appKey(appName(n), n.num("application.process.id"))]
		}
		return false
	}

	ins := map[string]int{} // channel -> input port of blite-share
	shareIn := map[int]bool{}
	type outPort struct {
		id int
		ch string
	}
	var outs []outPort
	for _, o := range objs {
		if o.Type != "PipeWire:Interface:Port" {
			continue
		}
		node := o.num("node.id")
		switch {
		case node == shareID && o.str("port.direction") == "in":
			ins[o.str("audio.channel")] = o.ID
			shareIn[o.ID] = true
		case o.str("port.direction") == "out":
			if n, ok := nodes[node]; ok && included(n) {
				outs = append(outs, outPort{o.ID, o.str("audio.channel")})
			}
		}
	}
	// The graph is the source of truth: existing links into blite-share.
	have := map[[2]int]bool{}
	for _, o := range objs {
		if o.Type == "PipeWire:Interface:Link" && shareIn[o.num("link.input.port")] {
			have[[2]int{o.num("link.output.port"), o.num("link.input.port")}] = true
		}
	}
	wanted := map[[2]int]bool{}
	for _, p := range outs {
		chans := []string{p.ch}
		if _, ok := ins[p.ch]; !ok { // MONO or unusual layouts: feed both sides
			chans = []string{"FL", "FR"}
		}
		for _, ch := range chans {
			if in, ok := ins[ch]; ok {
				wanted[[2]int{p.id, in}] = true
			}
		}
	}

	var firstErr error
	for l := range wanted {
		if have[l] {
			continue
		}
		// pw-link waits for the link to become active; don't wait long, the next
		// sync sees whether it exists.
		_, err := run(ctx, 2*time.Second, "pw-link", strconv.Itoa(l[0]), strconv.Itoa(l[1]))
		if err != nil && !errors.Is(err, context.DeadlineExceeded) && firstErr == nil {
			firstErr = err
		}
	}
	for l := range have {
		if !wanted[l] {
			run(ctx, 2*time.Second, "pw-link", "-d", strconv.Itoa(l[0]), strconv.Itoa(l[1]))
		}
	}
	return firstErr
}

// Close removes the virtual sink, which also removes every link into it.
func (s *Sharer) Close() {
	s.cancel()
	<-s.done
	if s.module != "" {
		run(context.Background(), 5*time.Second, "pactl", "unload-module", s.module)
	}
}

// ListApps returns programs currently playing audio, excluding Blite itself.
func ListApps() ([]App, error) {
	objs, err := dump(context.Background())
	if err != nil {
		return nil, err
	}
	own := os.Getpid()
	seen := map[string]bool{}
	var apps []App
	for _, o := range objs {
		if o.Type != "PipeWire:Interface:Node" || o.str("media.class") != "Stream/Output/Audio" {
			continue
		}
		name, pid := appName(o), o.num("application.process.id")
		if pid == own {
			continue
		}
		k := appKey(name, pid)
		if seen[k] {
			continue
		}
		seen[k] = true
		apps = append(apps, App{Key: k, Name: name, PID: pid})
	}
	sort.Slice(apps, func(i, j int) bool { return strings.ToLower(apps[i].Name) < strings.ToLower(apps[j].Name) })
	return apps, nil
}

// ListDevices returns output devices (sinks), excluding blite-share.
func ListDevices() ([]Device, error) {
	objs, err := dump(context.Background())
	if err != nil {
		return nil, err
	}
	var ds []Device
	for _, o := range objs {
		if o.Type != "PipeWire:Interface:Node" || o.str("media.class") != "Audio/Sink" || o.str("node.name") == ShareSinkName {
			continue
		}
		name := o.str("node.description")
		if name == "" {
			name = o.str("node.name")
		}
		ds = append(ds, Device{ID: o.str("node.name"), Name: name})
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i].Name < ds[j].Name })
	return ds, nil
}

// PlaybackSink returns the GStreamer sink that plays the call on device ("" =
// system default).
func PlaybackSink(device string) string {
	if device == "" {
		return "pipewiresink"
	}
	return fmt.Sprintf("pipewiresink target-object=%q", device)
}
