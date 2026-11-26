//go:build !windows

// ptscry-tui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// ui.go: the mouse, the questions on the status bar and the help.

package main

import (
	"strings"

	uv "github.com/charmbracelet/ultraviolet"
)

// ---------------------------------------------------------------- mouse

func (a *app) paneAt(x, y int) (*pane, *node) {
	if a.zoomed && a.focus != nil {
		if n := a.root.find(a.focus); n != nil {
			return a.focus, n
		}
	}
	for _, p := range a.panes() {
		if n := a.root.find(p); n != nil && uv.Pos(x, y).In(n.rect) {
			return p, n
		}
	}
	return nil, nil
}

func (a *app) dividerAt(n *node, x, y int) *node {
	if n == nil || n.p != nil {
		return nil
	}
	if uv.Pos(x, y).In(n.div) {
		return n
	}
	if d := a.dividerAt(n.a, x, y); d != nil {
		return d
	}
	return a.dividerAt(n.b, x, y)
}

// mouseEvent: a click takes the keyboard to the pane, a divider can be
// dragged, the wheel scrolls (copy mode) unless the program uses the mouse;
// with Shift the terminal's own selection works as usual.
func (a *app) mouseEvent(m uv.Mouse, kind string) {
	if a.root == nil || a.overlay != nil {
		return
	}
	if a.drag != nil {
		switch kind {
		case "motion":
			d := a.drag
			if d.side && d.rect.Dx() > 1 {
				d.ratio = min(max(float32(m.X-d.rect.Min.X)/float32(d.rect.Dx()), 0.05), 0.95)
			} else if d.rect.Dy() > 1 {
				d.ratio = min(max(float32(m.Y-d.rect.Min.Y)/float32(d.rect.Dy()), 0.05), 0.95)
			}
			a.layout()
		case "release":
			a.drag = nil
		}
		return
	}
	if kind == "click" && !a.zoomed {
		if d := a.dividerAt(a.root, m.X, m.Y); d != nil {
			a.drag = d
			return
		}
	}
	p, n := a.paneAt(m.X, m.Y)
	if p == nil {
		return
	}
	if kind == "click" && p != a.focus {
		a.last, a.focus = a.focus, p
		if p.copy == nil {
			return // the first click only chooses the pane
		}
	}
	if p.copy != nil && kind == "wheel" {
		k := uv.KeyPressEvent{Code: uv.KeyUp}
		if m.Button == uv.MouseWheelDown {
			k.Code = uv.KeyDown
		}
		for i := 0; i < 3; i++ {
			p.copyKey(k)
		}
		return
	}
	local := m
	local.X, local.Y = m.X-n.rect.Min.X, m.Y-n.rect.Min.Y
	if p.mouseReporting() {
		p.mu.Lock()
		switch kind {
		case "click":
			p.em.SendMouse(uv.MouseClickEvent(local))
		case "release":
			p.em.SendMouse(uv.MouseReleaseEvent(local))
		case "motion":
			p.em.SendMouse(uv.MouseMotionEvent(local))
		case "wheel":
			p.em.SendMouse(uv.MouseWheelEvent(local))
		}
		p.mu.Unlock()
		return
	}
	if kind != "wheel" {
		return
	}
	p.mu.Lock()
	alt := p.em.IsAltScreen()
	p.mu.Unlock()
	if alt { // a full-screen program without the mouse: the wheel is the arrows
		k := uv.KeyPressEvent{Code: uv.KeyUp}
		if m.Button == uv.MouseWheelDown {
			k.Code = uv.KeyDown
		}
		p.mu.Lock()
		for i := 0; i < 3; i++ {
			p.em.SendKey(k)
		}
		p.mu.Unlock()
		return
	}
	if m.Button == uv.MouseWheelUp {
		p.enterCopy()
		for i := 0; i < 3; i++ {
			p.copyKey(uv.KeyPressEvent{Code: uv.KeyUp})
		}
	}
}

// ---------------------------------------------------------------- a question

type question struct {
	label string
	text  string
	done  func(string)
}

func (q *question) key(a *app, k uv.KeyPressEvent) bool {
	switch {
	case k.Code == uv.KeyEscape || k.Mod == uv.ModCtrl && (k.Code == 'c' || k.Code == 'g'):
		return true
	case k.Code == uv.KeyEnter:
		if q.done != nil {
			q.done(strings.TrimSpace(q.text))
		}
		return true
	case k.Code == uv.KeyBackspace:
		if r := []rune(q.text); len(r) > 0 {
			q.text = string(r[:len(r)-1])
		}
	case k.Mod == uv.ModCtrl && k.Code == 'u':
		q.text = ""
	case k.Text != "" && k.Mod&(uv.ModCtrl|uv.ModAlt) == 0 && len(q.text) < 200:
		q.text += k.Text
	}
	a.redraw()
	return false
}

func (q *question) line(w int) string { return " " + q.label + q.text + "_  (Enter, Esc)" }

// ---------------------------------------------------------------- help

type help struct {
	prefix byte
	page   int
}

var helpLines = []string{
	"% split side by side   \" split one over the other   x close the pane (detach)",
	"arrows: move   o next   ; previous   z zoom   Space next layout   Alt/Ctrl+arrows resize",
	"S synchronized input   [ copy mode (arrows, / ? n N search, Space select, Enter copy, q)",
	"d detach all   W save the layout   O open a layout   mouse: click, drag the dividers, wheel",
	"to the pane's session: s list  ( ) prev/next  L last  c new  $ rename  f freeze  m M alerts",
}

func (h *help) key(a *app, k uv.KeyPressEvent) bool {
	h.page++
	if h.page >= len(helpLines) {
		return true
	}
	a.redraw()
	return false
}

func (h *help) line(w int) string {
	return " " + prefixLabel(h.prefix) + ": " + helpLines[h.page] + "  [any key: more]"
}
