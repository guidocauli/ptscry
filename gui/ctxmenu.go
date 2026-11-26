// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// ctxmenu.go: the context menu (right click) of the text fields and of the
// terminal: cut, copy, paste, select all.

package main

import (
	"image"
	"image/color"
	"io"
	"strings"

	"gioui.org/font"
	"gioui.org/io/clipboard"
	"gioui.org/io/event"
	"gioui.org/io/key"
	"gioui.org/io/pointer"
	"gioui.org/io/transfer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

type menuItem struct {
	label   string
	enabled bool
	do      func(gtx layout.Context)
	key     string // shortcut shown on the right
	sep     bool   // a separator line (the other fields are ignored)
	head    bool   // a group title (not clickable)
}

type ctxMenu struct {
	open  bool
	pos   image.Point
	items []menuItem
	clk   []widget.Clickable
	width int  // dp, 0 = 150
	right bool // pos.X is the right edge (dropdown anchored to its button)
}

// menuState is what the window keeps for the context menus.
type menuState struct {
	menu        ctxMenu
	hoverEd     *widget.Editor // the text field under the pointer
	hoverTags   map[*widget.Editor]*byte
	rootTag     byte
	menuTag     byte
	pasteTarget func(string)
}

func (g *gui) hoverTag(ed *widget.Editor) *byte {
	if g.hoverTags == nil {
		g.hoverTags = map[*widget.Editor]*byte{}
	}
	t, ok := g.hoverTags[ed]
	if !ok {
		t = new(byte)
		g.hoverTags[ed] = t
	}
	return t
}

// trackHover wraps a text field so that the window knows which one the
// pointer is on when the right button is pressed. Events pass through.
func (g *gui) trackHover(gtx layout.Context, ed *widget.Editor, w layout.Widget) layout.Dimensions {
	dims := w(gtx)
	tag := g.hoverTag(ed)
	area := clip.Rect(image.Rectangle{Max: dims.Size}).Push(gtx.Ops)
	pass := pointer.PassOp{}.Push(gtx.Ops)
	event.Op(gtx.Ops, tag)
	pass.Pop()
	area.Pop()
	for {
		ev, ok := gtx.Event(pointer.Filter{Target: tag, Kinds: pointer.Enter | pointer.Leave})
		if !ok {
			break
		}
		if e, ok := ev.(pointer.Event); ok {
			switch e.Kind {
			case pointer.Enter:
				g.hoverEd = ed
			case pointer.Leave:
				if g.hoverEd == ed {
					g.hoverEd = nil
				}
			}
		}
	}
	return dims
}

func setClipboard(gtx layout.Context, s string) {
	if s != "" {
		gtx.Execute(clipboard.WriteCmd{Type: "application/text", Data: io.NopCloser(strings.NewReader(s))})
	}
}

// editorMenu builds the menu of a text field.
func (g *gui) editorMenu(ed *widget.Editor) []menuItem {
	secret := ed.Mask != 0
	sel := ed.SelectedText()
	return []menuItem{
		{label: "Taglia", enabled: sel != "" && !secret && !ed.ReadOnly, do: func(gtx layout.Context) {
			setClipboard(gtx, ed.SelectedText())
			ed.Delete(1)
		}},
		{label: "Copia", enabled: sel != "" && !secret, do: func(gtx layout.Context) { setClipboard(gtx, ed.SelectedText()) }},
		{label: "Incolla", enabled: !ed.ReadOnly, do: func(gtx layout.Context) {
			g.pasteTarget = func(s string) {
				if ed.SingleLine {
					s = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(s)
				}
				ed.Insert(s)
			}
			gtx.Execute(clipboard.ReadCmd{Tag: &g.menuTag})
		}},
		{label: "Seleziona tutto", enabled: ed.Text() != "", do: func(gtx layout.Context) { ed.SetCaret(0, len([]rune(ed.Text()))) }},
	}
}

// terminalMenu builds the menu of the terminal of the current tab.
func (g *gui) terminalMenu(t *tab) []menuItem {
	items := []menuItem{
		{label: "Copia", enabled: t.model.SelectedText() != "", do: func(gtx layout.Context) { t.view.copySelection(gtx) }},
		{label: "Incolla", enabled: true, do: func(gtx layout.Context) {
			g.pasteTarget = func(s string) { t.view.paste(s) }
			gtx.Execute(clipboard.ReadCmd{Tag: &g.menuTag})
		}},
		{sep: true},
	}
	return append(items, g.paneMenu(t)...)
}

// rightClick opens the menu for what is under the pointer (pos is in window coordinates).
func (g *gui) rightClick(pos image.Point, mods key.Modifiers, barH int, haveDialog bool) {
	var items []menuItem
	switch {
	case g.hoverEd != nil:
		items = g.editorMenu(g.hoverEd)
	case !haveDialog && len(g.tabs) > 0 && pos.Y > barH:
		cur := g.curTab()
		if cur.model.MouseReporting() && mods&key.ModShift == 0 {
			return // the program in the terminal uses the right button
		}
		items = g.terminalMenu(cur)
	default:
		return
	}
	g.menu = ctxMenu{open: true, pos: pos, items: items, clk: make([]widget.Clickable, len(items))}
	g.invalidate()
}

// layoutMenu handles the root pointer area, the clipboard answer and draws the menu.
func (g *gui) layoutMenu(gtx layout.Context, barH int, haveDialog bool) {
	// the answer of the clipboard to "Incolla"
	for {
		ev, ok := gtx.Event(transfer.TargetFilter{Target: &g.menuTag, Type: "application/text"})
		if !ok {
			break
		}
		if e, ok := ev.(transfer.DataEvent); ok {
			b, _ := io.ReadAll(io.LimitReader(e.Open(), 4<<20))
			if g.pasteTarget != nil && len(b) > 0 {
				g.pasteTarget(string(b))
			}
			g.pasteTarget = nil
		}
	}
	// a pointer area over the whole window that lets every event through
	area := clip.Rect(image.Rectangle{Max: gtx.Constraints.Max}).Push(gtx.Ops)
	pass := pointer.PassOp{}.Push(gtx.Ops)
	event.Op(gtx.Ops, &g.rootTag)
	pass.Pop()
	area.Pop()
	for {
		ev, ok := gtx.Event(pointer.Filter{Target: &g.rootTag, Kinds: pointer.Press})
		if !ok {
			break
		}
		e, ok := ev.(pointer.Event)
		if !ok || e.Kind != pointer.Press {
			continue
		}
		pos := image.Pt(int(e.Position.X), int(e.Position.Y))
		if e.Buttons.Contain(pointer.ButtonSecondary) {
			g.rightClick(pos, e.Modifiers, barH, haveDialog)
		} else if g.menu.open && !pos.In(g.menuHot) {
			// a click anywhere else closes it (the ☰ button toggles it by itself)
			g.menu.open = false
		}
	}
	if !g.menu.open {
		return
	}
	// while a menu is open the keys are its own: the terminal must not get
	// Escape (twice = completion in bash) or anything typed
	gtx.Execute(key.FocusCmd{})
	for {
		ev, ok := gtx.Event(key.Filter{Name: key.NameEscape})
		if !ok {
			break
		}
		if e, ok := ev.(key.Event); ok && e.State == key.Press {
			g.menu.open = false
			g.invalidate()
			return
		}
	}
	m := &g.menu
	w := gtx.Dp(unit.Dp(150))
	if m.width > 0 {
		w = gtx.Dp(unit.Dp(m.width))
	}
	rowH := gtx.Dp(unit.Dp(28))
	sepH := gtx.Dp(unit.Dp(9))
	h := gtx.Dp(8)
	for _, it := range m.items {
		if it.sep {
			h += sepH
		} else {
			h += rowH
		}
	}
	px := m.pos.X
	if m.right {
		px -= w
	}
	x := min(px, gtx.Constraints.Max.X-w-gtx.Dp(4))
	y := min(m.pos.Y, gtx.Constraints.Max.Y-h-gtx.Dp(4))
	x, y = max(x, 0), max(y, 0)
	r := image.Rect(x, y, x+w, y+h)
	paint.FillShape(gtx.Ops, theme.chromeText, clip.UniformRRect(r, gtx.Dp(6)).Op(gtx.Ops))
	in := r.Inset(1)
	paint.FillShape(gtx.Ops, theme.chrome, clip.UniformRRect(in, gtx.Dp(5)).Op(gtx.Ops))
	// the menu swallows the clicks that fall on it
	blk := clip.Rect(r).Push(gtx.Ops)
	pointer.CursorDefault.Add(gtx.Ops)
	blk.Pop()
	cy := y + gtx.Dp(4)
	for i := range m.items {
		it := m.items[i]
		if it.sep {
			ly := cy + sepH/2
			paint.FillShape(gtx.Ops, theme.chromeText, clip.Rect(image.Rect(x+gtx.Dp(8), ly, x+w-gtx.Dp(8), ly+1)).Op())
			cy += sepH
			continue
		}
		row := image.Rect(x+gtx.Dp(4), cy, x+w-gtx.Dp(4), cy+rowH)
		cy += rowH
		st := clip.Rect(row).Push(gtx.Ops)
		if it.enabled && !it.head && m.clk[i].Clicked(gtx) {
			m.open = false
			it.do(gtx)
			st.Pop()
			g.invalidate()
			return
		}
		rg := gtx
		rg.Constraints = layout.Exact(row.Size())
		// no gtx.Disabled(): it fades the text to the point of not being
		// readable on the dark background; a disabled item is only greyer
		trans := op_offset(rg, row.Min)
		m.clk[i].Layout(rg, func(gtx layout.Context) layout.Dimensions {
			hot := m.clk[i].Hovered() && it.enabled && !it.head
			if hot {
				paint.FillShape(gtx.Ops, theme.selection, clip.Rect{Max: gtx.Constraints.Max}.Op())
			}
			l := material.Label(g.th, 13, it.label)
			l.Color = theme.fg
			switch {
			case it.head:
				l.Color = theme.accent
				l.Font.Weight = font.Bold
			case !it.enabled:
				l.Color = theme.disabled
			case hot:
				l.Color = color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
			}
			if it.key == "" {
				return layout.Inset{Left: 8, Top: 5}.Layout(gtx, l.Layout)
			}
			k := material.Label(g.th, 12, it.key)
			k.Color = theme.chromeText
			if !it.enabled {
				k.Color = theme.disabled
			} else if hot {
				k.Color = l.Color
			}
			return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return layout.Inset{Left: 8, Top: 5}.Layout(gtx, l.Layout)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return layout.Inset{Right: 8, Top: 6}.Layout(gtx, k.Layout)
				}),
			)
		})
		trans()
		st.Pop()
	}
}

// op_offset moves the origin to p; the returned function restores it.
func op_offset(gtx layout.Context, p image.Point) func() {
	st := op.Offset(p).Push(gtx.Ops)
	return st.Pop
}
