package media

import (
	"fmt"
	"sync/atomic"
)

// The self-preview branch of VideoSource feeds a grid tile (or pop-out) at
// PreviewW x PreviewH and PreviewFPS while our preview eye is open.
const (
	PreviewW, PreviewH = 640, 360
	PreviewFPS         = 15
)

// previewSeq numbers the self-preview channels.
var previewSeq atomic.Uint64

// NewPreviewChannel names a fresh intervideo channel for a self-preview.
func NewPreviewChannel() string { return fmt.Sprintf("blite-self-%d", previewSeq.Add(1)) }
