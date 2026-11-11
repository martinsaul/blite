package nui

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

// columns lays cells out side by side with widths in proportion to weights,
// so the People rows line up with their header.
type columns struct{ weights []float32 }

func (c *columns) total() float32 {
	var t float32
	for _, w := range c.weights {
		t += w
	}
	return t
}

func (c *columns) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	pad := theme.Padding()
	free := size.Width - pad*float32(len(objs)-1)
	x := float32(0)
	for i, o := range objs {
		w := free * c.weights[i] / c.total()
		h := min(o.MinSize().Height, size.Height)
		o.Resize(fyne.NewSize(w, h))
		o.Move(fyne.NewPos(x, (size.Height-h)/2))
		x += w + pad
	}
}

func (c *columns) MinSize(objs []fyne.CanvasObject) fyne.Size {
	var w, h float32
	for i, o := range objs {
		m := o.MinSize()
		// The narrowest a column may get: its share of the widest need.
		w = max(w, m.Width*c.total()/c.weights[i])
		h = max(h, m.Height)
	}
	return fyne.NewSize(w+theme.Padding()*float32(len(objs)-1), h)
}
