//go:build !windows

// ptscry-tui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// render.go: drawing the panes, the dividers, the status bar and the
// overlays; the terminal library sends only what changed.

package main

import (
	"fmt"
	"image/color"
	"strings"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// clipped is a screen that keeps the drawing inside a rectangle (an emulator
// a step behind a resize must not paint over its neighbour).
type clipped struct {
	uv.Screen
	r uv.Rectangle
}

func (c clipped) SetCell(x, y int, cell *uv.Cell) {
	if x >= c.r.Min.X && x < c.r.Max.X && y >= c.r.Min.Y && y < c.r.Max.Y {
		if cell != nil && cell.Width > 1 && x+cell.Width > c.r.Max.X {
			cell = &uv.Cell{Content: " ", Width: 1, Style: cell.Style} // a wide character cut at the edge
		}
		c.Screen.SetCell(x, y, cell)
	}
}

func (a *app) render() {
	if a.w <= 0 || a.h <= 0 || a.root == nil {
		return
	}
	// the cursor: the focused pane's, where its program left it
	hide := true
	if p := a.focus; p != nil && a.overlay == nil && p.copy == nil {
		if n := a.root.find(p); n != nil {
			p.mu.Lock()
			pos, hidden := p.em.CursorPosition(), p.em.CursorHidden()
			shape, blink := p.em.CursorStyle()
			p.mu.Unlock()
			if !hidden && pos.X < n.rect.Dx() && pos.Y < n.rect.Dy() {
				_ = a.scr.SetCursorPosition(n.rect.Min.X+pos.X, n.rect.Min.Y+pos.Y)
				hide = false
			}
			a.cursorStyle(int(shape), blink)
		}
	}
	if hide {
		_ = a.scr.HideCursor()
	} else {
		_ = a.scr.ShowCursor()
	}
	_ = a.scr.Display(uv.DrawableFunc(a.draw))
}

func (a *app) draw(scr uv.Screen, _ uv.Rectangle) {
	if a.zoomed && a.focus != nil {
		a.drawPane(scr, a.focus, a.area())
	} else {
		a.drawNode(scr, a.root)
	}
	if a.status {
		a.drawStatus(scr)
	}
}

func (a *app) drawNode(scr uv.Screen, n *node) {
	if n == nil {
		return
	}
	if n.p != nil {
		a.drawPane(scr, n.p, n.rect)
		return
	}
	a.drawNode(scr, n.a)
	a.drawNode(scr, n.b)
	// the divider: green next to the focused pane, red with the synchronized input
	c := borderColor
	if a.sync {
		c = syncColor
	}
	fr := uv.Rectangle{}
	if a.focus != nil {
		if fn := n.find(a.focus); fn != nil {
			fr = fn.rect
		}
	}
	for y := n.div.Min.Y; y < n.div.Max.Y; y++ {
		for x := n.div.Min.X; x < n.div.Max.X; x++ {
			ch := "│"
			if !n.side {
				ch = "─"
			}
			col := c
			if !a.sync && !fr.Empty() && touches(fr, x, y, n.side) {
				col = focusColor
			}
			scr.SetCell(x, y, &uv.Cell{Content: ch, Width: 1, Style: uv.Style{Fg: col}})
		}
	}
}

// touches tells whether the divider cell (x, y) runs along the rectangle r.
func touches(r uv.Rectangle, x, y int, side bool) bool {
	if side {
		return (x == r.Max.X || x == r.Min.X-1) && y >= r.Min.Y && y < r.Max.Y
	}
	return (y == r.Max.Y || y == r.Min.Y-1) && x >= r.Min.X && x < r.Max.X
}

func (a *app) drawPane(scr uv.Screen, p *pane, r uv.Rectangle) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cs := clipped{scr, r}
	if p.copy != nil {
		p.drawCopy(cs, r)
		return
	}
	p.em.Draw(cs, r)
}

// drawStatus: where, the session and pane with the keyboard, the warnings,
// the time (or the question / the notice of the moment).
func (a *app) drawStatus(scr uv.Screen) {
	y := a.h - 1
	st := uv.Style{Fg: barFg, Bg: barBg}
	for x := 0; x < a.w; x++ {
		scr.SetCell(x, y, &uv.Cell{Content: " ", Width: 1, Style: st})
	}
	if a.overlay != nil {
		putText(scr, 0, y, a.w, a.overlay.line(a.w), uv.Style{Fg: color.RGBA{A: 0xff}, Bg: warnColor})
		return
	}
	left := " " + a.be.where()
	if a.focus != nil {
		left += " · " + a.focus.session()
		ps := a.panes()
		if len(ps) > 1 {
			for i, p := range ps {
				if p == a.focus {
					left += fmt.Sprintf(" · pane %d/%d", i+1, len(ps))
				}
			}
		}
	}
	if a.pending {
		left += " · " + ansi.Strip(prefixLabel(a.prefix)) + " …"
	}
	var warn []string
	if a.sync {
		warn = append(warn, "SYNC INPUT")
	}
	if a.zoomed {
		warn = append(warn, "ZOOM")
	}
	if a.focus != nil && a.focus.copy != nil {
		warn = append(warn, a.focus.copyStatus())
	}
	right := time.Now().Format("15:04") + " "
	notice, noticeAt, bellAt := a.notices()
	if time.Since(bellAt) < 2*time.Second {
		right = "♪ " + right
	}
	x := putText(scr, 0, y, a.w, left, st)
	if time.Since(noticeAt) < 5*time.Second && notice != "" {
		x = putText(scr, x+2, y, a.w-len(right)-1, notice, uv.Style{Fg: warnColor, Bg: barBg})
	}
	if len(warn) > 0 {
		w := strings.Join(warn, " · ")
		wc := warnColor
		if a.sync {
			wc = syncColor
		}
		putText(scr, max(x+2, a.w-len(right)-len([]rune(w))-2), y, a.w-len(right), w, uv.Style{Fg: wc, Bg: barBg, Attrs: uv.AttrBold})
	}
	putText(scr, max(0, a.w-len([]rune(right))), y, a.w, right, st)
}

// putText writes s from x, up to (not including) column limit; it returns
// where it stopped.
func putText(scr uv.Screen, x, y, limit int, s string, st uv.Style) int {
	for _, r := range s {
		if x >= limit {
			break
		}
		if r < 0x20 || r == 0x7f {
			r = '?'
		}
		scr.SetCell(x, y, &uv.Cell{Content: string(r), Width: 1, Style: st})
		x++
	}
	return x
}

func prefixLabel(b byte) string {
	switch {
	case b >= 1 && b <= 26:
		return "Ctrl-" + string(rune('A'+b-1))
	case b >= 0x1b && b <= 0x1f:
		return "Ctrl-" + string("[\\]^_"[b-0x1b])
	}
	return "prefix"
}

// cursorStyle gives the real terminal the cursor the program of the focused
// pane asked for (DECSCUSR). A program that asked nothing (a blinking block,
// the emulator's default) gets the terminal's own default (DECSCUSR 0), so
// that the person's settings stay; client.conf "cursor" can force the blink.
func (a *app) cursorStyle(shape int, blink bool) {
	if shape < 0 || shape > 2 { // DECSCUSR 7 and up: not a shape
		shape = 0
	}
	n := 0
	if shape != 0 || !blink || a.cursorMode != "" {
		switch a.cursorMode {
		case "blink":
			blink = true
		case "steady":
			blink = false
		}
		n = shape*2 + 1 // 1 block, 3 underline, 5 bar: blinking
		if !blink {
			n++
		}
	}
	if n == a.lastCursor {
		return
	}
	a.lastCursor = n
	_, _ = a.scr.WriteString(fmt.Sprintf("\x1b[%d q", n))
}
