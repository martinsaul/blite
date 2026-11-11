package nui

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

// columns lays cells out side by side with widths in proportion to weights,
// but no column narrower than its widest cell needs, so the People rows
// line up with their header. The header and every row share one columns:
// mins holds each column's widest need over all of them.
type columns struct {
	weights []float32
	mins    []float32
}

func newColumns(weights ...float32) *columns {
	return &columns{weights: weights, mins: make([]float32, len(weights))}
}

// reset forgets the rows' needs (the rows changed).
func (c *columns) reset() { clear(c.mins) }

// widths shares free out: proportionally, then columns that would get
// less than they need get their need and the others share the rest.
func (c *columns) widths(free float32) []float32 {
	w := make([]float32, len(c.weights))
	fixed := make([]bool, len(c.weights))
	rest, weights := free, float32(0)
	for _, x := range c.weights {
		weights += x
	}
	for changed := true; changed && weights > 0; {
		changed = false
		for i, x := range c.weights {
			if !fixed[i] && rest*x/weights < c.mins[i] {
				w[i], fixed[i] = c.mins[i], true
				rest -= c.mins[i]
				weights -= x
				changed = true
			}
		}
	}
	for i, x := range c.weights {
		if !fixed[i] {
			w[i] = rest * x / weights
		}
	}
	return w
}

func (c *columns) need(objs []fyne.CanvasObject) {
	for i, o := range objs {
		c.mins[i] = max(c.mins[i], o.MinSize().Width)
	}
}

func (c *columns) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	c.need(objs)
	pad := theme.Padding()
	x := float32(0)
	for i, w := range c.widths(size.Width - pad*float32(len(objs)-1)) {
		o := objs[i]
		h := min(o.MinSize().Height, size.Height)
		o.Resize(fyne.NewSize(w, h))
		o.Move(fyne.NewPos(x, (size.Height-h)/2))
		x += w + pad
	}
}

func (c *columns) MinSize(objs []fyne.CanvasObject) fyne.Size {
	c.need(objs)
	var w, h float32
	for i, o := range objs {
		w += c.mins[i]
		h = max(h, o.MinSize().Height)
	}
	return fyne.NewSize(w+theme.Padding()*float32(len(objs)-1), h)
}
