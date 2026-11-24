package diag

import (
	"fmt"
	"strings"
)

// verdicts is one line per suspect, from what the steps measured. The
// caller holds the report's lock.
func verdicts(r *Report) []string {
	var out []string
	add := func(format string, a ...any) { out = append(out, fmt.Sprintf(format, a...)) }

	if s := r.System; s != nil {
		for _, k := range []string{"Hardware-accelerated GPU scheduling (HAGS)", "Game Mode", "Game Bar background recording", "Power plan"} {
			if v, ok := s.Settings[k]; ok {
				add("%s: %s", k, v)
			}
		}
		if v := s.Settings["Game Bar background recording"]; strings.HasPrefix(v, "on") {
			add("background recording is on: Windows keeps encoding the screen, competing for the encoder")
		}
		if !s.NextToApp {
			add("not run from the app's bin folder: the GStreamer measured may not be the one the app ships")
		}
	}
	for _, d := range r.Displays {
		if d.Primary {
			add("primary display %s: %dx%d @ %.0f Hz, scale %d%%, HDR %s, %s, on %s", d.Name, d.Width, d.Height, d.RefreshHz, d.ScalePct, orUnknown(d.HDR), d.Connection, orUnknown(d.Adapter))
			if d.HDR == "on" {
				add("HDR is on: every captured frame is converted from the HDR desktop, which costs the GPU more")
			}
		}
	}
	if p := r.Processes; p != nil {
		var names []string
		for _, f := range p.Found {
			if f.Exe != "nvcontainer.exe" {
				names = append(names, f.Exe)
			}
		}
		if len(names) > 0 {
			add("overlays, recorders or capture programs running: %s", strings.Join(names, ", "))
		} else {
			add("no known overlay, recorder or capture program running")
		}
		for _, u := range p.GPU {
			if u.Engine == "VideoEncode" && u.Percent >= 1 {
				add("%s (pid %d) was using the GPU's video encoder (%.0f%%) before the test", orUnknown(u.Exe), u.PID, u.Percent)
			}
		}
	}
	if e := r.Encode; e != nil {
		src := map[int]float64{}
		for _, s := range e.Sources {
			src[s.Height] = s.FPS
		}
		for _, run := range e.Runs {
			label := fmt.Sprintf("encoder %s %dp60", run.Encoder, run.Height)
			switch {
			case run.Error != "":
				add("%s: failed: %s", label, firstLine(run.Error))
			case run.OK:
				add("%s: %.0f fps OK", label, run.FPS)
			case src[run.Height] > 0 && src[run.Height] < okFPS*1.2 && run.FPS > 0.85*src[run.Height]:
				add("%s: %.0f fps, but the test source itself only makes %.0f: inconclusive", label, run.FPS, src[run.Height])
			default:
				add("%s: %.0f fps TOO SLOW for 60", label, run.FPS)
			}
		}
	}
	if c := r.Capture; c != nil {
		for _, run := range c.Runs {
			out = append(out, run.Verdict)
		}
	}
	if n := r.Net; n != nil && len(n.RequestMs) > 0 {
		add("network: round trip to the server median %.0f ms, max %.0f ms; upload %.1f Mbit/s", n.RequestMed, n.RequestMax, n.UploadMbps)
		if need := shareLayer(3840, 2160, 60).BitrateKbs; n.UploadMbps > 0 && n.UploadMbps*1000 < float64(need)*1.2 {
			add("upload %.1f Mbit/s is below what a 4K60 share sends (%d kbit/s plus overhead)", n.UploadMbps, need)
		}
	}
	return out
}
