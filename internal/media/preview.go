package media

import (
	"fmt"
	"sync/atomic"
)

// The self-preview branch of VideoSource feeds a grid tile (or pop-out) at
// PreviewW x PreviewH and PreviewFPS while our preview eye is open: enough
// for the small picture in the main window's corner. As a tile or in its
// own window it gets PreviewBigW x PreviewBigH (SetPreviewBig).
const (
	PreviewW, PreviewH       = 640, 360
	PreviewBigW, PreviewBigH = 1280, 720
	PreviewFPS               = 15
)

// previewCaps are the preview branch's caps: big or not, never more than
// the capture's HIGH layer (w x h at fps).
func previewCaps(big bool, w, h, fps int) string {
	pw, ph := PreviewW, PreviewH
	if big {
		pw, ph = PreviewBigW, PreviewBigH
	}
	if h > 0 && ph > h { // no bigger than what we send
		pw, ph = w&^1, h&^1
	}
	return fmt.Sprintf("video/x-raw,width=%d,height=%d,framerate=%d/1", pw, ph, min(PreviewFPS, fps))
}

// previewSeq numbers the self-preview channels.
var previewSeq atomic.Uint64

// NewPreviewChannel names a fresh intervideo channel for a self-preview.
func NewPreviewChannel() string { return fmt.Sprintf("blite-self-%d", previewSeq.Add(1)) }

// StartPattern feeds intervideo channel ch with a moving test pattern
// (videotestsrc's pattern names: "ball", "smpte", ...) of w x h, for
// blite-uidemo's video window. Stop it when done.
func StartPattern(ch, pattern string, w, h int) (*Pipeline, error) {
	p, err := launch(fmt.Sprintf("videotestsrc is-live=true pattern=%s ! video/x-raw,width=%d,height=%d,framerate=30/1 ! intervideosink channel=%s sync=false",
		pattern, w, h, ch))
	if err != nil {
		return nil, err
	}
	if err := p.start(); err != nil {
		p.Stop()
		return nil, err
	}
	return p, nil
}
