package nui

import (
	"fmt"
	"image/color"
	"math"
	"strconv"
	"strings"
	"sync"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"blite/internal/app"
	"blite/internal/i18n"
	"blite/internal/session"
)

// shareSheet is the Share options dialog (main controls + expandable Advanced).
type shareSheet struct {
	u         *UI
	root      fyne.CanvasObject
	content   *fyne.Container
	cur       app.ShareConfig
	setting   bool
	mu        sync.Mutex
	optsRunMu sync.Mutex
	secRunMu  sync.Mutex
	latestOpt app.ShareConfig
	secReq    bool
	secPw     string
	secE2EE   bool

	presets  *presetBar
	resNote  *widget.Label
	audioSeg *segmented
	// noWinAudioNote: the shared window's app audio can't be shared.
	noWinAudioNote *widget.Label
	// voice: "Include voice apps", under Whole system only.
	voice      *widget.Check
	voiceSlide *slideBox
	outSel     *widget.Select
	devByLabel map[string]string
	devKey     string

	advBtn        *widget.Button
	advOpen       bool
	advBox        *fyne.Container
	otherSrcSeg   *segmented
	fpsSlider     *widget.Slider
	fpsVal        *widget.Label
	draggingFPS   bool
	slowSeg       *segmented
	minResSeg     *segmented
	smallerSw     *toggleSwitch
	smallerLbl    *tapLabel
	keepSw        *toggleSwitch
	keepLbl       *tapLabel
	upModeSeg     *segmented
	upCustomBox   *fyne.Container
	upEntry       *widget.Entry
	upApply       *widget.Button
	portMapSw     *toggleSwitch
	portMapLbl    *tapLabel
	portMapNote   *widget.Label
	inCall        bool
	activePortMap bool
	pwSw          *toggleSwitch
	pwLbl         *tapLabel
	pwEntry       *widget.Entry
	e2eeSw        *toggleSwitch
	e2eeLbl       *tapLabel
	secNote       *widget.Label
}

func (u *UI) showShareSheet() {
	u.openShareSheet(false)
}

func (u *UI) openShareSheet(advanced bool) {
	u.openShareSheetAt(advanced, advanced)
}

// openShareSheetAt opens the sheet, Advanced expanded or not, scrolled to its
// end (the security options) or not (Advanced's sources, at its top).
func (u *UI) openShareSheetAt(advanced, toBottom bool) {
	s := newShareSheet(u)
	s.update(u.st)
	if advanced {
		s.setAdvancedOpen(true)
	}
	u.sheet = s
	d := dialog.NewCustom(i18n.L("Share options"), i18n.L("Done"), s.root, u.win)
	d.SetOnClosed(func() {
		if u.sheet == s {
			u.sheet = nil
		}
	})
	if advanced {
		d.Resize(fyne.NewSize(600, 920))
	} else {
		d.Resize(fyne.NewSize(580, 685))
	}
	d.Show()
	if toBottom {
		if sc, ok := s.root.(*container.Scroll); ok {
			sc.ScrollToBottom()
		}
	}
}

func newShareSheet(u *UI) *shareSheet {
	s := &shareSheet{
		u:              u,
		resNote:        caption(""),
		noWinAudioNote: caption(i18n.L("This window's app audio isn't available.")),
		fpsVal:         caption(""),
		portMapNote:    caption(i18n.L("Applies to your next session.")),
		secNote:        caption(i18n.L("Applies when starting a new session.")),
		devByLabel:     map[string]string{},
	}
	s.portMapNote.Hide()
	s.fpsVal.Wrapping = fyne.TextWrapOff
	s.resNote.Importance = widget.WarningImportance

	// The source is picked on Home's cards, not here.
	pick := func(p app.QualityPreset) {
		if s.setting {
			return
		}
		s.cur.Resolution = p.Resolution
		s.showFPS(p.FPS)
		s.sendShareOpts(true) // resolution then frame rate, in one goroutine

	}
	s.presets = newPresetBar(pick, u.removePreset, func() { u.presetDialog(pick) })

	s.audioSeg = newSegmentedCols(2, audioSegOptions(false), func(val string) {
		if s.setting {
			return
		}
		if val == "apps" {
			s.syncAudioSeg(s.cur)
			u.audioDialog()
			return
		}
		s.cur.Audio.Mode = val
		s.syncAudioSeg(s.cur)
		apps := s.cur.Audio.Apps
		u.do(func() error { return u.a.SetAudio(val, apps) })
	})

	s.voice = widget.NewCheck(i18n.L("Include voice apps (Discord, TeamSpeak...)"), func(on bool) {
		if s.setting {
			return
		}
		u.do(func() error { return u.a.SetVoiceInSystem(on) })
	})
	indent := canvas.NewRectangle(color.Transparent)
	indent.SetMinSize(fyne.NewSize(12, 0))
	s.voiceSlide = newSlideBox(container.NewBorder(nil, nil, indent, nil, s.voice))

	s.outSel = widget.NewSelect(nil, func(sel string) {
		if s.setting {
			return
		}
		id := s.devByLabel[sel]
		s.cur.Output = id
		u.do(func() error { return u.a.SetOutput(id) })
	})
	outRefresh := widget.NewButton(i18n.L("Refresh"), func() { go u.loadDevices() })
	outRow := container.NewBorder(nil, nil, nil, outRefresh, s.outSel)

	// Advanced controls. Home's cards pick the screen or a window; the
	// other sources are here.
	s.otherSrcSeg = newSegmentedCols(3, []segOption{
		{Value: "screen", Label: i18n.L("All screens combined")},
		{Value: "test", Label: i18n.L("Test pattern")},
		{Value: "none", Label: i18n.L("Nothing (watch only)")},
	}, func(val string) {
		if s.setting {
			return
		}
		s.cur.Source = val
		u.do(func() error { return u.a.SetSource(val) })
	})

	s.fpsSlider = widget.NewSlider(10, 60)
	s.fpsSlider.Step = 5
	s.fpsSlider.OnChanged = func(v float64) {
		if s.setting {
			return
		}
		s.draggingFPS = true
		fps := snapFPS(v)
		s.fpsVal.SetText(i18n.L("{{.FPS}} fps", i18n.D{"FPS": fps}))
		c := s.cur
		c.FPS = fps
		s.presets.update(c)
	}
	s.fpsSlider.OnChangeEnded = func(v float64) {
		s.draggingFPS = false
		if s.setting {
			return
		}
		s.applyFPS(snapFPS(v))
	}

	s.slowSeg = newSegmented([]segOption{
		{Value: session.SlowResolution, Label: i18n.L("Lower the resolution first")},
		{Value: session.SlowFPS, Label: i18n.L("Lower the frame rate first")},
	}, func(val string) {
		if s.setting {
			return
		}
		s.cur.SlowViewer = val
		s.cur.EffectiveSlow = val
		s.pushShareOpts()
	})

	s.minResSeg = newSegmented([]segOption{
		{Value: "360", Label: "360p"},
		{Value: "540", Label: "540p"},
		{Value: "720", Label: "720p"},
	}, func(val string) {
		if s.setting {
			return
		}
		mh, err := strconv.Atoi(val)
		if err != nil {
			return
		}
		s.cur.MinHeight = mh
		s.pushShareOpts()
	})

	var smallerRow fyne.CanvasObject
	s.smallerSw, s.smallerLbl, smallerRow = newSwitchRow(i18n.L("Send a smaller copy to slow viewers"), func(on bool) {
		if s.setting {
			return
		}
		s.cur.SmallerCopy = on
		s.pushShareOpts()
	})

	var keepRow fyne.CanvasObject
	s.keepSw, s.keepLbl, keepRow = newSwitchRow(i18n.L("Keep my chosen resolution"), func(on bool) {
		if s.setting {
			return
		}
		s.cur.KeepQuality = on
		s.pushShareOpts()
	})

	s.upEntry = widget.NewEntry()
	s.upEntry.SetPlaceHolder("10")
	applyCustomUpload := func() {
		if s.setting {
			return
		}
		kbps, err := parseUploadMbps(s.upEntry.Text)
		if err != nil {
			u.showErr(err)
			return
		}
		s.cur.UploadKbps = kbps
		s.pushShareOpts()
	}
	s.upEntry.OnSubmitted = func(string) { applyCustomUpload() }
	s.upApply = widget.NewButton(i18n.L("Apply"), applyCustomUpload)
	s.upCustomBox = container.NewBorder(nil, nil, nil,
		container.NewHBox(widget.NewLabel("Mbit/s"), s.upApply),
		s.upEntry)
	s.upCustomBox.Hide()

	s.upModeSeg = newSegmented([]segOption{
		{Value: "auto", Label: i18n.L("Automatic")},
		{Value: "custom", Label: i18n.L("Custom")},
	}, func(val string) {
		if s.setting {
			return
		}
		if val == "auto" {
			s.upCustomBox.Hide()
			s.cur.UploadKbps = 0
			s.pushShareOpts()
			return
		}
		if strings.TrimSpace(s.upEntry.Text) == "" {
			s.upEntry.SetText("10")
		}
		s.upCustomBox.Show()
		applyCustomUpload()
	})

	pushSecurity := func() {
		if s.setting {
			return
		}
		req, pw, e2ee := s.cur.RequirePassword, strings.TrimSpace(s.pwEntry.Text), s.cur.E2EE
		s.cur.RoomPassword = pw
		s.mu.Lock()
		s.secReq, s.secPw, s.secE2EE = req, pw, e2ee
		s.mu.Unlock()
		u.do(func() error {
			s.secRunMu.Lock()
			defer s.secRunMu.Unlock()
			s.mu.Lock()
			r, p, e := s.secReq, s.secPw, s.secE2EE
			s.mu.Unlock()
			return u.a.SetRoomSecurity(r, p, e)
		})
	}

	s.pwEntry = widget.NewEntry()
	s.pwEntry.SetPlaceHolder(i18n.L("Password"))
	s.pwEntry.Hide()
	s.pwEntry.OnChanged = func(string) {
		if !s.setting {
			pushSecurity()
		}
	}

	var pwRow fyne.CanvasObject
	s.pwSw, s.pwLbl, pwRow = newSwitchRow(i18n.L("Require a password"), func(on bool) {
		if s.setting {
			return
		}
		s.cur.RequirePassword = on
		if on {
			s.pwEntry.Show()
		} else {
			s.pwEntry.Hide()
		}
		pushSecurity()
	})

	var e2eeRow fyne.CanvasObject
	s.e2eeSw, s.e2eeLbl, e2eeRow = newSwitchRow(i18n.L("Encrypt end to end (everyone needs build 72 or newer)"), func(on bool) {
		if s.setting {
			return
		}
		s.cur.E2EE = on
		pushSecurity()
	})

	var portMapRow fyne.CanvasObject
	s.portMapSw, s.portMapLbl, portMapRow = newSwitchRow(i18n.L("Open a port on my router automatically"), func(on bool) {
		if s.setting {
			return
		}
		s.cur.PortMap = on
		s.cur.PortMapNextSession = s.inCall && on != s.activePortMap
		if s.cur.PortMapNextSession {
			s.portMapNote.Show()
		} else {
			s.portMapNote.Hide()
		}
		s.advBox.Refresh()
		u.do(func() error { return u.a.SetPortMap(on) })
	})

	fpsHdr := container.NewBorder(nil, nil, bold(i18n.L("Custom frame rate")), s.fpsVal)
	s.advBox = container.NewVBox(
		widget.NewSeparator(),
		bold(i18n.L("Source")),
		s.otherSrcSeg,
		fpsHdr,
		s.fpsSlider,
		bold(i18n.L("When a viewer's connection is slow")),
		s.slowSeg,
		bold(i18n.L("Lowest resolution allowed")),
		s.minResSeg,
		smallerRow,
		keepRow,
		caption(i18n.L("Don't lower the resolution when the computer seems slow")),
		bold(i18n.L("Upload limit")),
		s.upModeSeg,
		s.upCustomBox,
		portMapRow,
		s.portMapNote,
		widget.NewSeparator(),
		pwRow,
		s.pwEntry,
		e2eeRow,
		s.secNote,
	)
	s.advBox.Hide()

	s.advBtn = widget.NewButton(i18n.L("Advanced..."), func() {
		s.setAdvancedOpen(!s.advOpen)
	})
	s.advBtn.Importance = widget.LowImportance

	s.content = container.NewVBox(
		bold(i18n.L("Quality")),
		s.presets.root,
		s.resNote,
		bold(i18n.L("Audio")),
		s.audioSeg,
		s.voiceSlide,
		s.noWinAudioNote,
		bold(i18n.L("Play the call on")),
		outRow,
		container.NewHBox(s.advBtn),
		s.advBox,
	)
	s.voiceSlide.relayout = s.content.Refresh
	s.root = container.NewVScroll(container.New(&insetLayout{pad: 4, padRight: 16}, s.content))
	return s
}

func newSwitchRow(text string, onChange func(bool)) (*toggleSwitch, *tapLabel, fyne.CanvasObject) {
	sw := newToggleSwitch(onChange)
	lbl := &tapLabel{tap: func() {
		if !sw.disabled {
			want := !sw.checked
			sw.SetChecked(want)
			if onChange != nil {
				onChange(want)
			}
		}
	}}
	lbl.Text = text
	lbl.Wrapping = fyne.TextWrapWord
	lbl.ExtendBaseWidget(lbl)
	return sw, lbl, container.NewBorder(nil, nil, sw, nil, container.New(&insetLayout{pad: 4}, lbl))
}

func snapFPS(v float64) int {
	n := int(math.Round(v/5.0)) * 5
	return min(max(n, 10), 60)
}

// audioSegOptions are the audio choices; sharing a window adds its app's.
// "Test tone" sends beeps, to check that viewers hear us.
func audioSegOptions(window bool) []segOption {
	opts := []segOption{{Value: "none", Label: i18n.L("No audio")}}
	if window {
		opts = append(opts, segOption{Value: "window", Label: i18n.L("This window's app")})
	}
	return append(opts,
		segOption{Value: "system", Label: i18n.L("Whole system")},
		segOption{Value: "apps", Label: i18n.L("Only these apps..."), Retappable: true},
		segOption{Value: "test", Label: i18n.L("Test tone")})
}

func parseUploadMbps(raw string) (int, error) {
	s := strings.ReplaceAll(strings.TrimSpace(raw), ",", ".")
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 || v > 1000 {
		return 0, fmt.Errorf("enter an upload limit between 0.5 and 1000 Mbit/s")
	}
	return max(500, int(math.Round(v*1000))), nil
}

func formatMbps(kbps int) string {
	v := float64(kbps) / 1000.0
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func (s *shareSheet) setAdvancedOpen(open bool) {
	s.advOpen = open
	if open {
		s.advBox.Show()
	} else {
		s.advBox.Hide()
	}
	s.content.Refresh()
}

func (s *shareSheet) applyFPS(fps int) {
	s.showFPS(fps)
	s.pushShareOpts()
}

// showFPS makes fps the sheet's frame rate, without telling the app.
func (s *shareSheet) showFPS(fps int) {
	s.cur.FPS = fps
	if s.cur.SlowViewer == session.SlowAuto {
		if fps >= 45 {
			s.cur.EffectiveSlow = session.SlowResolution
		} else {
			s.cur.EffectiveSlow = session.SlowFPS
		}
	}
	s.setting = true
	s.presets.update(s.cur)
	s.fpsSlider.SetValue(float64(fps))
	s.fpsVal.SetText(i18n.L("{{.FPS}} fps", i18n.D{"FPS": fps}))
	s.slowSeg.SetSelected(s.cur.EffectiveSlow)
	s.setting = false
}

func (s *shareSheet) pushShareOpts() { s.sendShareOpts(false) }

// sendShareOpts sends the latest share options, and first the latest
// resolution if withRes (a preset). Both are read under optsRunMu, so quick
// taps never mix one preset's resolution with another's frame rate.
func (s *shareSheet) sendShareOpts(withRes bool) {
	s.mu.Lock()
	s.latestOpt = s.cur
	s.mu.Unlock()
	s.u.do(func() error {
		s.optsRunMu.Lock()
		defer s.optsRunMu.Unlock()
		s.mu.Lock()
		c := s.latestOpt
		s.mu.Unlock()
		if withRes {
			if err := s.u.a.SetResolution(c.Resolution); err != nil {
				return err
			}
		}
		return s.u.a.SetShareOptions(c.FPS, c.SlowViewer, c.MinHeight, c.SmallerCopy, c.KeepQuality, c.UploadKbps)
	})
}

// otherSource is src when it's one of Advanced's sources, else "".
func otherSource(src string) string {
	switch src {
	case "screen", "test", "none":
		return src
	}
	return ""
}

// syncAudioSeg shows sh's audio choice, in two columns for long languages;
// "This window's app" is offered while a window is the source, unless we
// can't find its process: then a note says so.
func (s *shareSheet) syncAudioSeg(sh app.ShareConfig) {
	window := (strings.HasPrefix(sh.Source, "window:") && !sh.NoWindowAudio) || sh.Audio.Mode == "window"
	if sh.NoWindowAudio {
		s.noWinAudioNote.Show()
	} else {
		s.noWinAudioNote.Hide()
	}
	sel := sh.Audio.Mode
	switch sel {
	case "system", "apps", "window", "test":
	default:
		sel = "none"
	}
	s.audioSeg.cols = 2
	s.audioSeg.SetOptions(audioSegOptions(window), sel)
	s.voiceSlide.SetOpen(sel == "system")
}

func normalizeShareState(st app.UIState) app.ShareConfig {
	sh := st.Share
	if st.Session != nil {
		ss := st.Session
		if ss.Source == "" {
			sh.Source = "none"
		} else {
			sh.Source = ss.Source
		}
		if ss.Resolution != "" {
			sh.Resolution = ss.Resolution
		}
		rawOpts := ss.ShareOptions
		if rawOpts.FPS == 0 && rawOpts.MinHeight == 0 && !rawOpts.SmallerCopy {
			rawOpts = session.DefaultShareOptions()
		}
		opts := session.NormalizeShareOptions(rawOpts)
		sh.FPS = opts.FPS
		sh.SlowViewer = opts.SlowViewer
		sh.EffectiveSlow = opts.EffectiveSlowViewer()
		sh.MinHeight = opts.MinHeight
		sh.SmallerCopy = opts.SmallerCopy
		sh.KeepQuality = opts.KeepQuality
		sh.UploadKbps = opts.UploadKbps
		sh.Audio = ss.Audio
		sh.Output = ss.Output
	}
	if sh.Source == "" {
		if st.Session != nil {
			sh.Source = "none"
		} else {
			sh.Source = "primary"
		}
	}
	if sh.Resolution == "" {
		sh.Resolution = "720p"
	}
	if sh.FPS == 0 && sh.MinHeight == 0 && !sh.SmallerCopy {
		// Uninitialized zero ShareConfig (e.g. synthetic test states).
		def := session.DefaultShareOptions()
		sh.FPS = def.FPS
		sh.MinHeight = def.MinHeight
		sh.SmallerCopy = def.SmallerCopy
		sh.EffectiveSlow = def.EffectiveSlowViewer()
		if st.Session == nil && !sh.E2EE {
			sh.E2EE = true
		}
		if !sh.PortMap && !sh.PortMapNextSession {
			sh.PortMap = true
		}
	} else {
		opts := session.NormalizeShareOptions(session.ShareOptions{
			FPS:         sh.FPS,
			SlowViewer:  sh.SlowViewer,
			MinHeight:   sh.MinHeight,
			SmallerCopy: sh.SmallerCopy,
			UploadKbps:  sh.UploadKbps,
		})
		sh.FPS = opts.FPS
		sh.SlowViewer = opts.SlowViewer
		sh.EffectiveSlow = opts.EffectiveSlowViewer()
		sh.MinHeight = opts.MinHeight
		sh.UploadKbps = opts.UploadKbps
	}
	if sh.Audio.Mode == "" {
		sh.Audio.Mode = "none"
	}
	return sh
}

func (s *shareSheet) update(st app.UIState) {
	sh := normalizeShareState(st)
	s.cur = sh
	s.setting = true
	defer func() { s.setting = false }()

	if !s.draggingFPS {
		s.presets.update(sh)
	}
	s.otherSrcSeg.SetSelected(otherSource(sh.Source))
	if st.Session != nil && st.Session.ResolutionNote != "" {
		s.resNote.SetText(i18n.Msg(st.Session.ResolutionNote))
		s.resNote.Show()
	} else {
		s.resNote.Hide()
	}

	if !s.draggingFPS {
		s.fpsSlider.SetValue(float64(sh.FPS))
		s.fpsVal.SetText(i18n.L("{{.FPS}} fps", i18n.D{"FPS": sh.FPS}))
	}

	s.syncAudioSeg(sh)
	s.voice.SetChecked(st.VoiceInSystem)

	// Output devices dropdown.
	defLbl := i18n.L("System default")
	var keyParts []string
	for _, d := range s.u.devices {
		keyParts = append(keyParts, d.ID+"="+d.Name)
	}
	key := defLbl + "|" + strings.Join(keyParts, "|")
	if key != s.devKey {
		s.devKey = key
		s.devByLabel = map[string]string{defLbl: ""}
		opts := []string{defLbl}
		for _, d := range s.u.devices {
			lbl := d.Name
			if _, exists := s.devByLabel[lbl]; exists {
				lbl = d.Name + " (" + d.ID + ")"
			}
			s.devByLabel[lbl] = d.ID
			opts = append(opts, lbl)
		}
		s.outSel.Options = opts
		s.outSel.Refresh()
	}
	selOut := defLbl
	for lbl, id := range s.devByLabel {
		if id == sh.Output {
			selOut = lbl
			break
		}
	}
	s.outSel.SetSelected(selOut)

	// Advanced controls.
	s.slowSeg.SetSelected(sh.EffectiveSlow)
	s.minResSeg.SetSelected(strconv.Itoa(sh.MinHeight))
	s.smallerSw.SetChecked(sh.SmallerCopy)
	s.keepSw.SetChecked(sh.KeepQuality)

	if sh.UploadKbps > 0 {
		s.upModeSeg.SetSelected("custom")
		if s.u.win == nil || s.u.win.Canvas().Focused() != s.upEntry {
			s.upEntry.SetText(formatMbps(sh.UploadKbps))
		}
		s.upCustomBox.Show()
	} else {
		s.upModeSeg.SetSelected("auto")
		s.upCustomBox.Hide()
	}

	s.pwSw.SetChecked(sh.RequirePassword)
	if sh.RequirePassword {
		if s.u.win == nil || s.u.win.Canvas().Focused() != s.pwEntry {
			s.pwEntry.SetText(sh.RoomPassword)
		}
		s.pwEntry.Show()
	} else {
		s.pwEntry.Hide()
	}
	s.e2eeSw.SetChecked(sh.E2EE)

	s.inCall = st.Session != nil
	if s.inCall && sh.PortMapNextSession {
		s.activePortMap = !sh.PortMap
	} else {
		s.activePortMap = sh.PortMap
	}
	s.portMapSw.SetChecked(sh.PortMap)
	if s.inCall && sh.PortMapNextSession {
		s.portMapNote.Show()
	} else {
		s.portMapNote.Hide()
	}

	if st.Session != nil {
		s.secNote.Show()
	} else {
		s.secNote.Hide()
	}
}
