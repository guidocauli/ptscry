//go:build !windows

// ptscry-tui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// copy.go: copy mode (prefix [): move through what the pane printed, search
// it (/ ? n N), select (Space or v) and copy (Enter or y) to the clipboard of
// the terminal (OSC 52, which works through SSH too).

package main

import (
	"encoding/base64"
	"fmt"
	"strings"

	uv "github.com/charmbracelet/ultraviolet"
)

type copyMode struct {
	top    int // first line on screen (absolute: 0 = oldest of the scrollback)
	cx, cy int // cursor (column, absolute line)
	sel    bool
	sx, sy int // where the selection started
	find   string
	back   bool // the last search went backwards
	msg    string
}

// lines and the text of a line, with p.mu held.
func (p *pane) totalLines() int { return p.em.ScrollbackLen() + p.rows }

func (p *pane) cellAt(x, line int) *uv.Cell {
	sb := p.em.ScrollbackLen()
	if line < sb {
		return p.em.ScrollbackCellAt(x, line)
	}
	return p.em.CellAt(x, line-sb)
}

func (p *pane) lineText(line int) string {
	var b strings.Builder
	for x := 0; x < p.cols; x++ {
		c := p.cellAt(x, line)
		switch {
		case c == nil:
			b.WriteByte(' ')
		case c.Content == "" && c.Width == 0:
			// the second half of a wide character
		case c.Content == "":
			b.WriteByte(' ')
		default:
			b.WriteString(c.Content)
		}
	}
	return b.String()
}

func (p *pane) enterCopy() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.copy != nil {
		return
	}
	total := p.totalLines()
	pos := p.em.CursorPosition()
	p.copy = &copyMode{top: total - p.rows, cx: pos.X, cy: total - p.rows + pos.Y}
}

func (p *pane) copyStatus() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.copy
	if c == nil {
		return ""
	}
	s := fmt.Sprintf("COPY %d/%d", c.cy+1, p.totalLines())
	if c.sel {
		s += " SELECTING"
	}
	if c.msg != "" {
		s += " · " + c.msg
	}
	return s
}

// copyKey: the keys of copy mode.
func (p *pane) copyKey(k uv.KeyPressEvent) {
	a := p.app
	p.mu.Lock()
	c := p.copy
	if c == nil {
		p.mu.Unlock()
		return
	}
	c.msg = ""
	total := p.totalLines()
	page := max(p.rows-1, 1)
	exit, copyNow := false, false
	switch {
	case k.Code == uv.KeyEscape || k.Text == "q" || (k.Mod == uv.ModCtrl && k.Code == 'c'):
		exit = true
	case k.Code == uv.KeyUp || k.Text == "k":
		c.cy--
	case k.Code == uv.KeyDown || k.Text == "j":
		c.cy++
	case k.Code == uv.KeyLeft || k.Text == "h":
		c.cx--
	case k.Code == uv.KeyRight || k.Text == "l":
		c.cx++
	case k.Code == uv.KeyPgUp || (k.Mod == uv.ModCtrl && k.Code == 'u'):
		c.cy -= page
		c.top -= page
	case k.Code == uv.KeyPgDown || (k.Mod == uv.ModCtrl && k.Code == 'd'):
		c.cy += page
		c.top += page
	case k.Text == "g" || k.Code == uv.KeyHome && k.Mod == uv.ModCtrl:
		c.cy, c.cx = 0, 0
	case k.Text == "G" || k.Code == uv.KeyEnd && k.Mod == uv.ModCtrl:
		c.cy = total - 1
	case k.Text == "0" || k.Code == uv.KeyHome:
		c.cx = 0
	case k.Text == "$" || k.Code == uv.KeyEnd:
		c.cx = len([]rune(strings.TrimRight(p.lineText(c.cy), " "))) - 1
	case k.Text == "w":
		c.cx = nextWord(p.lineText(c.cy), c.cx)
	case k.Text == "b":
		c.cx = prevWord(p.lineText(c.cy), c.cx)
	case k.Text == " " || k.Text == "v":
		c.sel = !c.sel
		c.sx, c.sy = c.cx, c.cy
	case k.Code == uv.KeyEnter || k.Text == "y":
		copyNow, exit = true, true
	case k.Text == "/" || k.Text == "?":
		back := k.Text == "?"
		p.mu.Unlock()
		a.overlay = &question{label: k.Text, done: func(s string) { p.search(s, !back) }}
		a.redraw()
		return
	case k.Text == "n" || k.Text == "N":
		f, back := c.find, c.back
		p.mu.Unlock()
		if f != "" {
			p.search(f, (k.Text == "n") != back)
		}
		a.redraw()
		return
	}
	c.clamp(p, total)
	var text string
	if copyNow {
		text = p.selection()
	}
	if exit {
		p.copy = nil
	}
	p.mu.Unlock()
	if copyNow && text != "" {
		a.clipboard(text)
		a.say("copied %d characters", len([]rune(text)))
	}
	a.redraw()
}

func (c *copyMode) clamp(p *pane, total int) {
	c.cy = min(max(c.cy, 0), total-1)
	c.cx = min(max(c.cx, 0), p.cols-1)
	if c.cy < c.top {
		c.top = c.cy
	}
	if c.cy >= c.top+p.rows {
		c.top = c.cy - p.rows + 1
	}
	c.top = min(max(c.top, 0), max(total-p.rows, 0))
}

// selection is the selected text (or the line of the cursor), p.mu held.
func (p *pane) selection() string {
	c := p.copy
	if !c.sel {
		return strings.TrimRight(p.lineText(c.cy), " ")
	}
	y0, x0, y1, x1 := c.sy, c.sx, c.cy, c.cx
	if y0 > y1 || y0 == y1 && x0 > x1 {
		y0, x0, y1, x1 = y1, x1, y0, x0
	}
	var lines []string
	for y := y0; y <= y1; y++ {
		r := []rune(p.lineText(y))
		from, to := 0, len(r)
		if y == y0 {
			from = min(x0, len(r))
		}
		if y == y1 {
			to = min(x1+1, len(r))
		}
		if from > to {
			from = to
		}
		lines = append(lines, strings.TrimRight(string(r[from:to]), " "))
	}
	return strings.Join(lines, "\n")
}

// search moves to the next (forward) or previous match of s.
func (p *pane) search(s string, forward bool) {
	if s == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.copy
	if c == nil {
		return
	}
	c.find, c.back = s, !forward
	total := p.totalLines()
	low := strings.ToLower(s)
	for i := 1; i <= total; i++ {
		y := c.cy + i
		if !forward {
			y = c.cy - i
		}
		y = (y%total + total) % total
		line := strings.ToLower(p.lineText(y))
		if k := strings.Index(line, low); k >= 0 {
			c.cy, c.cx = y, len([]rune(line[:k]))
			c.top = max(y-p.rows/2, 0)
			c.clamp(p, total)
			return
		}
	}
	c.msg = "not found: " + s
}

// drawCopy draws the pane from the scrollback, with the selection and the
// cursor of copy mode (p.mu held).
func (p *pane) drawCopy(scr uv.Screen, r uv.Rectangle) {
	c := p.copy
	y0, x0, y1, x1 := c.sy, c.sx, c.cy, c.cx
	if y0 > y1 || y0 == y1 && x0 > x1 {
		y0, x0, y1, x1 = y1, x1, y0, x0
	}
	inSel := func(x, y int) bool {
		if !c.sel || y < y0 || y > y1 {
			return false
		}
		if y == y0 && x < x0 || y == y1 && x > x1 {
			return false
		}
		return true
	}
	for row := 0; row < r.Dy(); row++ {
		line := c.top + row
		for x := 0; x < r.Dx(); x++ {
			cell := uv.Cell{Content: " ", Width: 1}
			if line < p.totalLines() {
				if cc := p.cellAt(x, line); cc != nil {
					if cc.Width == 0 {
						continue // the second half of a wide character
					}
					cell = *cc
					if cell.Content == "" {
						cell.Content = " "
					}
				}
			}
			if inSel(x, line) || x == c.cx && line == c.cy {
				cell.Style.Attrs ^= uv.AttrReverse
			}
			scr.SetCell(r.Min.X+x, r.Min.Y+row, &cell)
		}
	}
	tag := fmt.Sprintf("[%d/%d]", c.cy+1, p.totalLines())
	putText(scr, r.Max.X-len(tag), r.Min.Y, r.Max.X, tag, uv.Style{Fg: barBg, Bg: warnColor})
}

// clipboard puts text in the clipboard of the terminal (OSC 52).
func (a *app) clipboard(text string) {
	if len(text) > 100000 {
		text = text[:100000]
	}
	_, _ = a.scr.WriteString("\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\x07")
	_ = a.scr.Flush()
}

func nextWord(s string, x int) int {
	r := []rune(s)
	i := x
	for i < len(r) && r[i] != ' ' {
		i++
	}
	for i < len(r) && r[i] == ' ' {
		i++
	}
	if i >= len(r) {
		return x
	}
	return i
}

func prevWord(s string, x int) int {
	r := []rune(s)
	i := min(x, len(r)) - 1
	for i > 0 && r[i] == ' ' {
		i--
	}
	for i > 0 && r[i-1] != ' ' {
		i--
	}
	return max(i, 0)
}
