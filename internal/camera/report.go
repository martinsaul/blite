package camera

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"
)

// Preset is a built-in share quality.
type Preset struct {
	Height, FPS int
}

func (p Preset) String() string { return fmt.Sprintf("%dp%d", p.Height, p.FPS) }

// Presets are the six built-in qualities (app/presets.go).
var Presets = []Preset{{720, 30}, {720, 60}, {1080, 30}, {1080, 60}, {2160, 30}, {2160, 60}}

// Report prints the cameras for `blite -cameras`: ID, name, backend,
// modes, the mode each built-in preset would use, and on Windows the
// camera privacy switches. It prints no device paths. It returns the
// exit code.
func Report(w io.Writer) int {
	cams, err := List()
	if err != nil {
		fmt.Fprintf(w, "Couldn't list cameras: %v\n", err)
		return 1
	}
	var b strings.Builder
	report(&b, cams, CheckPrivacy(), os.Getenv("BLITE_FAKE_CAMERAS") == "1")
	for _, l := range strings.SplitAfter(b.String(), "\n") {
		io.WriteString(w, strings.TrimRight(l, " \n")+strings.Repeat("\n", strings.Count(l, "\n")))
	}
	return 0
}

func report(w io.Writer, cams []Camera, priv Privacy, fakes bool) {
	switch len(cams) {
	case 0:
		fmt.Fprintln(w, "No cameras found.")
	case 1:
		fmt.Fprintln(w, "1 camera found.")
	default:
		fmt.Fprintf(w, "%d cameras found.\n", len(cams))
	}
	if fakes {
		fmt.Fprintln(w, "(BLITE_FAKE_CAMERAS=1: the test cameras are included.)")
	}
	for i, c := range cams {
		fmt.Fprintf(w, "\n%d. %s\n", i+1, c.Name)
		fmt.Fprintf(w, "   Source:  %s   (for -source)\n", c.Source())
		fmt.Fprintf(w, "   Backend: %s (%s)\n", c.Backend, BackendName(c.Backend))
		groups := modeGroups(c.Modes)
		if len(groups) == 0 {
			fmt.Fprintln(w, "   Modes:   none we can use (the camera reported no raw or MJPEG formats)")
		} else {
			fmt.Fprintf(w, "   Modes:   %d\n", len(c.Modes))
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			for _, g := range groups {
				fmt.Fprintf(tw, "     %s\t%s\t%s\n", g.label, g.size, g.rates)
			}
			tw.Flush()
		}
		fmt.Fprintln(w, "   Presets:")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "     preset\tcamera mode\tshares at\tnote")
		for _, p := range Presets {
			f := Fit(c, p.Height, p.FPS)
			mode := "-"
			if f.Mode != (Mode{}) {
				mode = f.Mode.String()
			}
			fmt.Fprintf(tw, "     %s\t%s\t%dp%d\t%s\n", p, mode, f.Height, f.FPS, f.Note)
		}
		tw.Flush()
	}
	if priv.Checked {
		fmt.Fprintln(w)
		state := "allowed"
		if priv.Blocked() {
			state = "BLOCKED: Bellion Share can't open a camera. Turn camera access on in Settings > Privacy & security > Camera (" + SettingsURI + ")."
		}
		fmt.Fprintln(w, "Windows camera privacy:", state)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintf(tw, "  Camera access (this device):\t%s\n", consentText(priv.Device))
		fmt.Fprintf(tw, "  Camera access (desktop apps, this device):\t%s\n", consentText(priv.DeviceDesktop))
		fmt.Fprintf(tw, "  Let apps access your camera:\t%s\n", consentText(priv.User))
		fmt.Fprintf(tw, "  Let desktop apps access your camera:\t%s\n", consentText(priv.UserDesktop))
		tw.Flush()
	}
	if runtime.GOOS == "windows" || len(cams) > 0 {
		fmt.Fprintln(w, "\nNo device paths or serial numbers are shown; the IDs are hashes.")
	}
}

func consentText(v string) string {
	switch strings.ToLower(v) {
	case "allow":
		return "on"
	case "deny":
		return "OFF"
	case "":
		return "not set (on)"
	}
	return v
}

type modeGroup struct {
	label, size, rates string
	h, w, rank         int
}

// modeGroups folds modes of one format and size into one line with
// their frame rates, biggest first.
func modeGroups(ms []Mode) []modeGroup {
	type key struct{ label, size string }
	idx := map[key]int{}
	var gs []modeGroup
	var rates [][]Mode
	for _, m := range ms {
		k := key{m.Label(), m.Size()}
		i, ok := idx[k]
		if !ok {
			i = len(gs)
			idx[k] = i
			r := 2
			switch {
			case m.JPEG():
				r = 0
			case m.Format == "NV12":
				r = 1
			}
			gs = append(gs, modeGroup{label: k.label, size: k.size, h: m.TopH(), w: max(m.W, m.MaxW), rank: r})
			rates = append(rates, nil)
		}
		rates[i] = append(rates[i], m)
	}
	for i := range gs {
		ms := rates[i]
		sort.SliceStable(ms, func(a, b int) bool { return ms[a].TopFPS() > ms[b].TopFPS() })
		var parts []string
		seen, variable := map[string]bool{}, false
		for _, m := range ms {
			if m.Variable() {
				variable = true
				continue
			}
			r := strings.TrimSuffix(m.Rate(), " fps")
			if !seen[r] {
				seen[r] = true
				parts = append(parts, r)
			}
		}
		var r string
		if len(parts) > 0 {
			r = strings.Join(parts, ", ") + " fps"
		}
		if variable {
			r = strings.TrimPrefix(r+", variable rate", ", ")
		}
		gs[i].rates = r
	}
	sort.SliceStable(gs, func(a, b int) bool {
		if gs[a].h != gs[b].h {
			return gs[a].h > gs[b].h
		}
		if gs[a].w != gs[b].w {
			return gs[a].w > gs[b].w
		}
		return gs[a].rank < gs[b].rank
	})
	return gs
}
