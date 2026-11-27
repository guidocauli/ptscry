// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// activity.go: the "Attività" panel at the bottom of the window, as in
// RexDesk: connections, lost connections, errors and notices, the newest
// first. A click on a line copies it.

package main

import (
	"fmt"
	"image"
	"strings"
	"sync"
	"time"

	"gioui.org/font"
	"gioui.org/io/event"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

const maxActivity = 500

type actEntry struct {
	at   time.Time
	text string
	warn bool
}

type activityState struct {
	mu      sync.Mutex
	entries []actEntry
	list    widget.List
	clk     map[int]*widget.Clickable // by the position from the oldest (stable while scrolling)
	first   int                       // how many were dropped from the head (keeps the keys stable)
	clear   widget.Clickable
	hide    widget.Clickable
	copyAll widget.Clickable
	divTag  byte
	drag    bool
	dragY   float32
	dragH   int
}

// logAct adds a line to the activity panel; any goroutine.
func (g *gui) logAct(warn bool, format string, a ...any) {
	s := cleanLine(fmt.Sprintf(format, a...), 600)
	if s == "" {
		return
	}
	g.act.mu.Lock()
	n := len(g.act.entries)
	if n > 0 && g.act.entries[n-1].text == s && time.Since(g.act.entries[n-1].at) < 2*time.Second {
		g.act.mu.Unlock()
		return // the same notice twice in a row
	}
	g.act.entries = append(g.act.entries, actEntry{at: time.Now(), text: s, warn: warn})
	if len(g.act.entries) > maxActivity {
		drop := len(g.act.entries) - maxActivity
		g.act.entries = append([]actEntry(nil), g.act.entries[drop:]...)
		g.act.first += drop
	}
	g.act.mu.Unlock()
	g.invalidate()
}

func (g *gui) activityOn() bool {
	g.setMu.Lock()
	defer g.setMu.Unlock()
	return g.set.ActLog != "off"
}

func (g *gui) toggleActivity() {
	g.setMu.Lock()
	if g.set.ActLog == "off" {
		g.set.ActLog = "on"
	} else {
		g.set.ActLog = "off"
	}
	g.setMu.Unlock()
	g.saveWorkspace()
	g.invalidate()
}

// activityHeight is the height of the panel in px (0 = hidden).
func (g *gui) activityHeight(gtx layout.Context, avail int) int {
	if !g.activityOn() {
		return 0
	}
	g.setMu.Lock()
	h := g.set.ActLogH
	g.setMu.Unlock()
	if h == 0 {
		h = 110
	}
	px := gtx.Dp(dpOf(h))
	// the terminals keep at least half of the space
	return max(min(px, avail/2), gtx.Dp(50))
}

// activityPanel draws the panel; gtx.Constraints are its exact size.
func (g *gui) activityPanel(gtx layout.Context) {
	a := &g.act
	sz := gtx.Constraints.Max
	paint.FillShape(gtx.Ops, theme.chrome, clip.Rect{Max: sz}.Op())
	paint.FillShape(gtx.Ops, theme.line, clip.Rect{Max: image.Pt(sz.X, gtx.Dp(1))}.Op())
	// the top edge moves with the mouse
	div := image.Rect(0, 0, sz.X, gtx.Dp(5))
	st := clip.Rect(div).Push(gtx.Ops)
	pointer.CursorRowResize.Add(gtx.Ops)
	event.Op(gtx.Ops, &a.divTag)
	st.Pop()
	for {
		ev, ok := gtx.Event(pointer.Filter{Target: &a.divTag, Kinds: pointer.Press | pointer.Drag | pointer.Release | pointer.Cancel})
		if !ok {
			break
		}
		e, ok := ev.(pointer.Event)
		if !ok {
			continue
		}
		switch e.Kind {
		case pointer.Press:
			a.drag, a.dragY, a.dragH = true, e.Position.Y, sz.Y
		case pointer.Drag:
			if a.drag {
				h := a.dragH - int(e.Position.Y-a.dragY)
				g.setMu.Lock()
				g.set.ActLogH = min(max(int(float32(h)/gtx.Metric.PxPerDp), 50), 600)
				g.setMu.Unlock()
				g.invalidate()
			}
		case pointer.Release, pointer.Cancel:
			if a.drag {
				a.drag = false
				g.saveWorkspace()
			}
		}
	}
	if a.clear.Clicked(gtx) {
		a.mu.Lock()
		a.first += len(a.entries)
		a.entries = nil
		a.mu.Unlock()
	}
	if a.hide.Clicked(gtx) {
		g.toggleActivity()
	}
	a.mu.Lock()
	entries := append([]actEntry(nil), a.entries...)
	first := a.first
	a.mu.Unlock()
	if a.copyAll.Clicked(gtx) {
		var b strings.Builder
		for _, e := range entries {
			b.WriteString(e.at.Format("15:04:05") + " " + e.text + "\n")
		}
		setClipboard(gtx, b.String())
		g.flashQuiet("attività copiate negli appunti")
	}
	if a.clk == nil {
		a.clk = map[int]*widget.Clickable{}
	}
	layout.Inset{Left: 8, Right: 4, Top: 4}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						l := material.Label(g.th, 13, "Attività")
						l.Font.Weight = font.Bold
						l.Color = theme.fg
						return l.Layout(gtx)
					}),
					layout.Rigid(g.flatButton(&a.copyAll, "Copia tutto")),
					layout.Rigid(g.flatButton(&a.clear, "Pulisci")),
					layout.Rigid(g.flatButton(&a.hide, "Nascondi")),
				)
			}),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				if len(entries) == 0 {
					return layout.Inset{Top: 6}.Layout(gtx, small(g, "Nessuna attività: qui compaiono connessioni, disconnessioni, errori e avvisi."))
				}
				a.list.Axis = layout.Vertical
				return material.List(g.th, &a.list).Layout(gtx, len(entries), func(gtx layout.Context, i int) layout.Dimensions {
					idx := len(entries) - 1 - i // the newest first
					e := entries[idx]
					c, ok := a.clk[first+idx]
					if !ok {
						c = new(widget.Clickable)
						a.clk[first+idx] = c
					}
					if c.Clicked(gtx) {
						setClipboard(gtx, e.at.Format("15:04:05")+" "+e.text)
						g.flashQuiet("riga copiata negli appunti")
					}
					return c.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						gtx.Constraints.Min.X = gtx.Constraints.Max.X
						l := material.Label(g.th, 12, e.at.Format("15:04:05")+"  "+e.text)
						l.Font.Typeface = "Go Mono"
						l.Color = theme.chromeText
						if e.warn {
							l.Color = theme.warn
						}
						if c.Hovered() {
							l.Color = theme.fg
						}
						return layout.Inset{Top: 2, Bottom: 2}.Layout(gtx, l.Layout)
					})
				})
			}),
		)
	})
	// forget the buttons of lines no longer kept
	if len(a.clk) > maxActivity*2 {
		for k := range a.clk {
			if k < first {
				delete(a.clk, k)
			}
		}
	}
}

// flatButton is a small text button without a background (bars and panels).
func (g *gui) flatButton(c *widget.Clickable, label string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return c.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Stack{}.Layout(gtx,
				layout.Expanded(func(gtx layout.Context) layout.Dimensions {
					if c.Hovered() {
						paint.FillShape(gtx.Ops, theme.hover, clip.UniformRRect(image.Rectangle{Max: gtx.Constraints.Min}, gtx.Dp(4)).Op(gtx.Ops))
					}
					return layout.Dimensions{Size: gtx.Constraints.Min}
				}),
				layout.Stacked(func(gtx layout.Context) layout.Dimensions {
					l := material.Label(g.th, 12, label)
					l.Color = theme.chromeText
					if c.Hovered() {
						l.Color = theme.fg
					}
					return layout.Inset{Left: 8, Right: 8, Top: 3, Bottom: 3}.Layout(gtx, l.Layout)
				}),
			)
		})
	}
}
