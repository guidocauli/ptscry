// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// termview.go: the terminal widget: it draws a termModel with Gio and turns
// keyboard and mouse events into what the program expects.

package main

import (
	"image"
	"image/color"
	"io"
	"strings"
	"time"

	"gioui.org/f32"
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
	"gioui.org/text"
	"gioui.org/unit"
	uv "github.com/charmbracelet/ultraviolet"
	"golang.org/x/image/math/fixed"
)

const termPad = 4 // dp of margin around the text

// termCommands are the things a terminal asks of the application.
type termCommands struct {
	newTab, closeTab, nextTab, prevTab func()
	sessions, workspaces, rename, env  func()
	detach, freeze                     func()          // disconnect from the session / freeze-thaw it
	zoom                               func(delta int) // font size, +1 / -1 / 0 = reset
	invalidate                         func()
	pane                               func(action string) bool                     // panes (Alt+Shift+...): true = done
	app                                func(name key.Name, mods key.Modifiers) bool // keys of the window (side bar, activity, F11): true = done
}

// termView is one terminal on screen.
type termView struct {
	model      *termModel
	shaper     *text.Shaper
	cmd        termCommands
	onSize     func(cols, rows int)
	fontPx     float32 // text size in pixels per em, before scaling
	cellW      int     // pixels
	cellH      int
	ascent     int
	scale      float32
	fontSet    bool
	measuredPx float32
	face       string // typeface chosen in the settings ("" = Go Mono)

	focusTag    struct{}
	focused     bool
	lastClick   time.Time
	clicks      int
	dragging    bool
	sbDrag      bool
	cursorOn    bool
	cursorShape int // 0 block, 1 underline, 2 bar (set by the layout from the program's request)
	cursorAt    time.Time
	zoomAcc     float32

	// the text sent to the program, from keys and paste
	input func([]byte)
	// onPress: a click in the terminal (it takes the keyboard among the panes)
	onPress func()
	// mirrors: the other terminals that get what is typed here (input sincronizzato)
	mirrors func() []*termView

	size image.Point // pixels of the area last laid out
}

func newTermView(model *termModel, shaper *text.Shaper, fontPx float32) *termView {
	return &termView{model: model, shaper: shaper, fontPx: fontPx, cursorOn: true, scale: 1}
}

func (t *termView) monoFont(bold, italic bool) font.Font {
	f := font.Font{Typeface: "Go Mono"}
	if t.face != "" && !strings.ContainsAny(t.face, "\r\n") {
		f.Typeface = font.Typeface(t.face + ", Go Mono") // the chosen font, then ours for what it lacks
	}
	if bold {
		f.Weight = font.Bold
	}
	if italic {
		f.Style = font.Italic
	}
	return f
}

// measure computes the size of a cell from the font.
func (t *termView) measure(px float32) { // px: pixels per em, already scaled; fontPx stays in dp
	params := text.Parameters{Font: t.monoFont(false, false), PxPerEm: fixed.I(int(px + 0.5)), MaxLines: 1, MinWidth: 0, MaxWidth: 1 << 20}
	t.shaper.LayoutString(params, "M")
	g, ok := t.shaper.NextGlyph()
	if !ok {
		t.cellW, t.cellH, t.ascent = 9, 18, 14
		return
	}
	t.cellW = max(g.Advance.Ceil(), 1)
	t.ascent = g.Ascent.Ceil()
	t.cellH = max(g.Ascent.Ceil()+g.Descent.Ceil(), 1)
	t.cellH += t.cellH / 8 // a little air between lines
	for {
		if _, ok := t.shaper.NextGlyph(); !ok {
			break
		}
	}
}

// Layout draws the terminal in the given area and handles its events. It
// returns the dimensions used (the whole area).
func (t *termView) Layout(gtx layout.Context, focus bool) layout.Dimensions {
	px := t.fontPx * float32(gtx.Metric.PxPerDp) // the font size is in dp
	if !t.fontSet || t.scale != gtx.Metric.PxPerDp || t.measuredPx != px {
		t.scale = gtx.Metric.PxPerDp
		t.measuredPx = px
		t.measure(px)
		t.fontSet = true
	}
	size := gtx.Constraints.Max
	t.size = size
	pad := gtx.Dp(unit.Dp(termPad))
	cols := max((size.X-2*pad)/t.cellW, 2)
	rows := max((size.Y-2*pad)/t.cellH, 1)
	cc, cr := t.model.Size()
	if cols != cc || rows != cr {
		t.model.Resize(cols, rows)
		if t.onSize != nil {
			t.onSize(cols, rows)
		}
	}

	t.events(gtx, pad)
	if focus {
		gtx.Execute(key.FocusCmd{Tag: &t.focusTag})
	}

	// background
	defer clip.Rect{Max: size}.Push(gtx.Ops).Pop()
	paint.Fill(gtx.Ops, theme.bg)

	// pointer area for the whole widget
	area := clip.Rect{Max: size}.Push(gtx.Ops)
	event.Op(gtx.Ops, &t.focusTag)
	area.Pop()

	t.paintCells(gtx, pad)
	t.paintScrollbar(gtx, size, pad)
	return layout.Dimensions{Size: size}
}

func (t *termView) cellAt(p f32.Point, pad int) (col, row int) {
	return int(p.X-float32(pad)) / t.cellW, int(p.Y-float32(pad)) / t.cellH
}

type runStyle struct {
	fg, bg color.NRGBA
	bold   bool
	italic bool
	under  bool
	strike bool
}

func (t *termView) paintCells(gtx layout.Context, pad int) {
	ops := gtx.Ops
	m := t.model
	type cellRec struct {
		text  string
		style runStyle
		wide  bool
		skip  bool
	}
	cols, rows := m.Size()
	grid := make([]cellRec, cols*rows)
	var curX, curY int
	var curVisible bool
	m.Paint(func(r, c int, cv cellView) {
		if r >= rows || c >= cols {
			return
		}
		fg := toNRGBA(cv.fg, theme.fg)
		bg := toNRGBA(cv.bg, theme.bg)
		if cv.attrs&uv.AttrReverse != 0 {
			fg, bg = bg, fg
		}
		if cv.selected {
			bg = theme.selection
		}
		if cv.attrs&uv.AttrFaint != 0 {
			fg = color.NRGBA{R: fg.R/2 + bg.R/2, G: fg.G/2 + bg.G/2, B: fg.B/2 + bg.B/2, A: 0xff}
		}
		if cv.attrs&uv.AttrConceal != 0 {
			fg = bg
		}
		grid[r*cols+c] = cellRec{text: cv.text, wide: cv.width == 2, skip: cv.cont,
			style: runStyle{fg: fg, bg: bg, bold: cv.attrs&uv.AttrBold != 0, italic: cv.attrs&uv.AttrItalic != 0,
				under: cv.underl, strike: cv.attrs&uv.AttrStrikethrough != 0}}
	}, func(x, y int, vis bool) { curX, curY, curVisible = x, y, vis })

	// 1: backgrounds, merged along each row
	for r := 0; r < rows; r++ {
		c := 0
		for c < cols {
			bg := grid[r*cols+c].style.bg
			e := c + 1
			for e < cols && grid[r*cols+e].style.bg == bg {
				e++
			}
			if bg != theme.bg {
				rect := image.Rect(pad+c*t.cellW, pad+r*t.cellH, pad+e*t.cellW, pad+(r+1)*t.cellH)
				paint.FillShape(ops, bg, clip.Rect(rect).Op())
			}
			c = e
		}
	}
	// 2: the cursor under the text when it is a block
	block := t.cursorShape == 0
	if curVisible && curX < cols && curY < rows {
		rect := image.Rect(pad+curX*t.cellW, pad+curY*t.cellH, pad+(curX+1)*t.cellW, pad+(curY+1)*t.cellH)
		if t.focused {
			if t.cursorOn {
				switch t.cursorShape {
				case 1: // underline
					rect.Min.Y = rect.Max.Y - max(2, t.cellH/8)
				case 2: // bar
					rect.Max.X = rect.Min.X + max(2, t.cellW/6)
				}
				paint.FillShape(ops, theme.cursor, clip.Rect(rect).Op())
			}
		} else { // an empty box when the window has not the focus
			paint.FillShape(ops, theme.cursor, clip.Stroke{Path: clip.Rect(rect).Path(), Width: 1}.Op())
		}
	}
	// 3: text, in runs of equal style made of plain ASCII; anything else cell by cell
	for r := 0; r < rows; r++ {
		c := 0
		for c < cols {
			cell := grid[r*cols+c]
			if cell.skip || cell.text == "" || cell.text == " " {
				c++
				continue
			}
			style := cell.style
			if block && curVisible && t.focused && t.cursorOn && c == curX && r == curY {
				style.fg = theme.bg // text under a block cursor
			}
			if rs := []rune(cell.text); len(rs) == 1 && isBoxOrBlock(rs[0]) {
				drawBox(gtx, rs[0], pad+c*t.cellW, pad+r*t.cellH, t.cellW, t.cellH, style.fg)
				c++
				continue
			}
			if !isPlain(cell.text) || cell.wide {
				t.drawText(gtx, cell.text, style, pad+c*t.cellW, pad+r*t.cellH, 0)
				c++
				continue
			}
			var sb strings.Builder
			e := c
			for e < cols {
				ce := grid[r*cols+e]
				st := ce.style
				if block && curVisible && t.focused && t.cursorOn && e == curX && r == curY {
					st.fg = theme.bg
				}
				if ce.skip || ce.wide || st != style || (ce.text != "" && !isPlain(ce.text)) {
					break
				}
				if ce.text == "" {
					sb.WriteByte(' ')
				} else {
					sb.WriteString(ce.text)
				}
				e++
			}
			t.drawText(gtx, strings.TrimRight(sb.String(), " "), style, pad+c*t.cellW, pad+r*t.cellH, e-c)
			c = e
		}
	}
	// underline and strike-through
	for r := 0; r < rows; r++ {
		for c := 0; c < cols; c++ {
			s := grid[r*cols+c].style
			if s.under {
				y := pad + r*t.cellH + t.ascent + 2
				paint.FillShape(ops, s.fg, clip.Rect(image.Rect(pad+c*t.cellW, y, pad+(c+1)*t.cellW, y+1)).Op())
			}
			if s.strike {
				y := pad + r*t.cellH + t.cellH/2
				paint.FillShape(ops, s.fg, clip.Rect(image.Rect(pad+c*t.cellW, y, pad+(c+1)*t.cellW, y+1)).Op())
			}
		}
	}
}

func isPlain(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] >= 0x7f {
			return false
		}
	}
	return true
}

// drawText draws s with its top-left corner at (x, y).
func (t *termView) drawText(gtx layout.Context, s string, st runStyle, x, y, _ int) {
	if s == "" {
		return
	}
	params := text.Parameters{Font: t.monoFont(st.bold, st.italic), PxPerEm: fixed.I(int(t.measuredPx + 0.5)), MaxLines: 1, MaxWidth: 1 << 20}
	t.shaper.LayoutString(params, s)
	var gs []text.Glyph
	for {
		g, ok := t.shaper.NextGlyph()
		if !ok {
			break
		}
		gs = append(gs, g)
	}
	if len(gs) == 0 {
		return
	}
	if n := len([]rune(s)); n == len(gs) {
		// Put every glyph on the cell grid: the font advance is fractional, the
		// cell is whole pixels, and the difference would add up along a run.
		x0 := gs[0].X
		for i := range gs {
			gs[i].X = x0 + fixed.I(i*t.cellW)
		}
	}
	path := t.shaper.Shape(gs)
	defer op.Offset(image.Pt(x, y+t.ascent+(t.cellH-t.cellH*8/9)/2)).Push(gtx.Ops).Pop()
	defer clip.Outline{Path: path}.Op().Push(gtx.Ops).Pop()
	paint.Fill(gtx.Ops, st.fg)
}

func (t *termView) paintScrollbar(gtx layout.Context, size image.Point, pad int) {
	off, total := t.model.ScrollState()
	if total == 0 || off == 0 {
		return
	}
	_, rows := t.model.Size()
	h := size.Y - 2*pad
	thumb := max(h*rows/(total+rows), 12)
	pos := (total - off) * (h - thumb) / max(total, 1)
	w := gtx.Dp(6)
	rect := image.Rect(size.X-w-2, pad+pos, size.X-2, pad+pos+thumb)
	paint.FillShape(gtx.Ops, color.NRGBA{R: 0x80, G: 0x88, B: 0x98, A: 0xa0}, clip.UniformRRect(rect, w/2).Op(gtx.Ops))
}

// events handles input for this frame.
func (t *termView) events(gtx layout.Context, pad int) {
	tag := &t.focusTag
	mods := key.ModCtrl | key.ModAlt | key.ModShift | key.ModSuper | key.ModCommand
	var filters []event.Filter
	filters = append(filters,
		key.FocusFilter{Target: tag},
		transfer.TargetFilter{Target: tag, Type: "application/text"},
		pointer.Filter{Target: tag, Kinds: pointer.Press | pointer.Release | pointer.Drag | pointer.Move | pointer.Scroll,
			ScrollY: pointer.ScrollRange{Min: -1 << 20, Max: 1 << 20}, ScrollX: pointer.ScrollRange{Min: -1 << 20, Max: 1 << 20}},
		key.Filter{Focus: tag, Optional: mods},
	)
	for _, n := range []key.Name{key.NameTab, key.NameLeftArrow, key.NameRightArrow, key.NameUpArrow, key.NameDownArrow,
		key.NameEscape, key.NameReturn, key.NameEnter, key.NameDeleteBackward, key.NameDeleteForward, key.NameHome, key.NameEnd,
		key.NamePageUp, key.NamePageDown, key.NameSpace, key.NameF1, key.NameF2, key.NameF3, key.NameF4, key.NameF5, key.NameF6,
		key.NameF7, key.NameF8, key.NameF9, key.NameF10, key.NameF11, key.NameF12, "Insert"} {
		filters = append(filters, key.Filter{Focus: tag, Optional: mods, Name: n})
	}
	for {
		ev, ok := gtx.Event(filters...)
		if !ok {
			break
		}
		switch e := ev.(type) {
		case key.FocusEvent:
			t.focused = e.Focus
			t.cursorAt = time.Now()
			t.cursorOn = true
		case key.EditEvent:
			if e.Text != "" {
				t.model.SelectClear()
				t.model.ScrollTo(false)
				t.send([]byte(e.Text))
				for _, m := range t.mirrorViews() {
					m.send([]byte(e.Text))
				}
			}
		case key.Event:
			if e.State == key.Press {
				t.keyPress(gtx, e)
			}
		case transfer.DataEvent:
			r := e.Open()
			b, _ := io.ReadAll(io.LimitReader(r, 4<<20))
			r.Close()
			t.paste(string(b))
		case pointer.Event:
			t.pointer(gtx, e, pad)
		}
	}
}

func (t *termView) send(b []byte) {
	if t.input != nil && len(b) > 0 {
		t.input(b)
	}
}

func (t *termView) paste(s string) {
	if s == "" {
		return
	}
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\r"), "\n", "\r")
	for _, v := range append([]*termView{t}, t.mirrorViews()...) {
		v.model.mu.Lock()
		v.model.em.Paste(s)
		v.model.mu.Unlock()
	}
}

func (t *termView) copySelection(gtx layout.Context) {
	if s := t.model.SelectedText(); s != "" {
		gtx.Execute(clipboard.WriteCmd{Type: "application/text", Data: io.NopCloser(strings.NewReader(s))})
	}
}

// zoomKey tells what a key does to the font size: +1, -1 or 0 (reset).
func zoomKey(name key.Name, shift bool) (int, bool) {
	switch name {
	case "+", "*":
		return 1, true
	case "=":
		if shift {
			return 0, true // Shift+0 on an Italian keyboard
		}
		return 1, true
	case "-", "_":
		return -1, true
	case "0":
		return 0, true
	}
	return 0, false
}

func (t *termView) keyPress(gtx layout.Context, e key.Event) {
	ctrl := e.Modifiers&key.ModCtrl != 0
	shift := e.Modifiers&key.ModShift != 0
	super := e.Modifiers&(key.ModCommand|key.ModSuper) != 0
	// application shortcuts: Ctrl+Shift on Linux and Windows, Command on macOS
	cs := (ctrl && shift) || e.Modifiers&key.ModCommand != 0
	if ctrl && !shift && e.Name == "Insert" {
		t.copySelection(gtx)
		return
	}
	// panes, as in Windows Terminal: Alt+Shift with + - arrows O Z S L Space
	alt := e.Modifiers&key.ModAlt != 0
	if alt && t.cmd.pane != nil {
		act := ""
		switch {
		case shift && !ctrl && (e.Name == "+" || e.Name == "=" || e.Name == "*"):
			act = "split-lr"
		case shift && !ctrl && (e.Name == "-" || e.Name == "_"):
			act = "split-tb"
		case !shift && !ctrl && e.Name == key.NameLeftArrow:
			act = "focus-left"
		case !shift && !ctrl && e.Name == key.NameRightArrow:
			act = "focus-right"
		case !shift && !ctrl && e.Name == key.NameUpArrow:
			act = "focus-up"
		case !shift && !ctrl && e.Name == key.NameDownArrow:
			act = "focus-down"
		case shift && !ctrl && e.Name == key.NameLeftArrow:
			act = "grow-left"
		case shift && !ctrl && e.Name == key.NameRightArrow:
			act = "grow-right"
		case shift && !ctrl && e.Name == key.NameUpArrow:
			act = "grow-up"
		case shift && !ctrl && e.Name == key.NameDownArrow:
			act = "grow-down"
		case shift && !ctrl && e.Name == "O":
			act = "next"
		case shift && !ctrl && e.Name == "Z":
			act = "zoom"
		case shift && !ctrl && e.Name == "S":
			act = "sync"
		case shift && !ctrl && e.Name == "L":
			act = "layouts"
		case shift && !ctrl && e.Name == key.NameSpace:
			act = "preset"
		}
		if act != "" && t.cmd.pane(act) {
			return
		}
	}
	// the window's own keys: Ctrl+Shift+B / J / N and F11
	if t.cmd.app != nil && ((cs && (e.Name == "B" || e.Name == "J" || e.Name == "N")) || e.Name == key.NameF11 && e.Modifiers == 0) {
		if t.cmd.app(e.Name, e.Modifiers) {
			return
		}
	}
	if cs || (shift && e.Name == "Insert") {
		switch e.Name {
		case "C":
			t.copySelection(gtx)
			return
		case "V", "Insert":
			gtx.Execute(clipboard.ReadCmd{Tag: &t.focusTag})
			return
		case "T":
			if t.cmd.newTab != nil {
				t.cmd.newTab()
			}
			return
		case "S":
			if t.cmd.sessions != nil {
				t.cmd.sessions()
			}
			return
		case "O":
			if t.cmd.workspaces != nil {
				t.cmd.workspaces()
			}
			return
		case "E":
			if t.cmd.env != nil {
				t.cmd.env()
			}
			return
		case "R":
			if t.cmd.rename != nil {
				t.cmd.rename()
			}
			return
		case "D":
			if t.cmd.detach != nil {
				t.cmd.detach()
			}
			return
		case "F":
			if t.cmd.freeze != nil {
				t.cmd.freeze()
			}
			return
		case "W":
			if t.cmd.closeTab != nil {
				t.cmd.closeTab()
			}
			return
		}
	}
	// font size: Ctrl (or Ctrl+Shift, or Command) with + - 0, on any keyboard layout
	// (with Shift an Italian keyboard gives "*" for +, "_" for - and "=" for 0)
	if ctrl || e.Modifiers&key.ModCommand != 0 {
		if d, ok := zoomKey(e.Name, shift); ok {
			t.cmd.zoom(d)
			return
		}
	}
	if ctrl && (e.Name == key.NamePageDown || e.Name == key.NameTab && !shift) {
		if t.cmd.nextTab != nil {
			t.cmd.nextTab()
		}
		return
	}
	if ctrl && (e.Name == key.NamePageUp || e.Name == key.NameTab && shift) {
		if t.cmd.prevTab != nil {
			t.cmd.prevTab()
		}
		return
	}
	if e.Name == key.NameTab && (e.Modifiers&(key.ModAlt|key.ModSuper) != 0) {
		return // Alt+Tab, Super+Tab: the window manager's, never the shell's (it would complete)
	}
	_, rows := t.model.Size()
	if shift && !ctrl {
		switch e.Name {
		case key.NamePageUp:
			t.model.Scroll(rows - 1)
			return
		case key.NamePageDown:
			t.model.Scroll(-(rows - 1))
			return
		case key.NameHome:
			t.model.ScrollTo(true)
			return
		case key.NameEnd:
			t.model.ScrollTo(false)
			return
		}
	}
	if super {
		return
	}
	ke, ok := keyEvent(e.Name, e.Modifiers)
	if !ok {
		return
	}
	t.model.SelectClear()
	t.model.ScrollTo(false)
	t.model.mu.Lock()
	t.model.em.SendKey(ke)
	t.model.mu.Unlock()
	// each pane encodes the key for its own program (cursor keys mode...)
	for _, m := range t.mirrorViews() {
		m.model.mu.Lock()
		m.model.em.SendKey(ke)
		m.model.mu.Unlock()
	}
}

func (t *termView) mirrorViews() []*termView {
	if t.mirrors == nil {
		return nil
	}
	return t.mirrors()
}

func (t *termView) pointer(gtx layout.Context, e pointer.Event, pad int) {
	if e.Kind == pointer.Press && t.onPress != nil {
		t.onPress()
	}
	col, row := t.cellAt(e.Position, pad)
	cols, rows := t.model.Size()
	col, row = min(max(col, 0), cols-1), min(max(row, 0), rows-1)
	reporting := t.model.MouseReporting() && e.Modifiers&key.ModShift == 0 // Shift forces a local selection
	btn := func(b pointer.Buttons) uv.MouseButton {
		switch {
		case b.Contain(pointer.ButtonPrimary):
			return uv.MouseLeft
		case b.Contain(pointer.ButtonTertiary):
			return uv.MouseMiddle
		case b.Contain(pointer.ButtonSecondary):
			return uv.MouseRight
		}
		return uv.MouseNone
	}
	switch e.Kind {
	case pointer.Scroll:
		if e.Modifiers&key.ModCtrl != 0 && t.cmd.zoom != nil { // Ctrl+wheel: font size
			t.zoomAcc += e.Scroll.Y
			if step := float32(gtx.Dp(24)); t.zoomAcc >= step || t.zoomAcc <= -step {
				if t.zoomAcc > 0 {
					t.cmd.zoom(-1)
				} else {
					t.cmd.zoom(1)
				}
				t.zoomAcc = 0
			}
			return
		}
		lines := int(e.Scroll.Y / float32(max(t.cellH, 1)) * 3)
		if lines == 0 && e.Scroll.Y != 0 {
			lines = int(sign(e.Scroll.Y))
		}
		if reporting || t.model.AltScreen() {
			b := uv.MouseWheelDown
			if lines < 0 {
				b = uv.MouseWheelUp
			}
			n := min(abs(lines), 5)
			for i := 0; i < max(n, 1); i++ {
				t.model.mu.Lock()
				if reporting {
					t.model.em.SendMouse(uv.MouseWheelEvent{X: col, Y: row, Button: b, Mod: uvMods(e.Modifiers)})
				} else { // full-screen program without mouse: wheel = arrow keys
					code := uv.KeyDown
					if lines < 0 {
						code = uv.KeyUp
					}
					t.model.em.SendKey(uv.KeyPressEvent{Code: code})
				}
				t.model.mu.Unlock()
			}
			return
		}
		t.model.Scroll(-lines)
	case pointer.Press:
		gtx.Execute(key.FocusCmd{Tag: &t.focusTag})
		if reporting {
			t.model.mu.Lock()
			t.model.em.SendMouse(uv.MouseClickEvent{X: col, Y: row, Button: btn(e.Buttons), Mod: uvMods(e.Modifiers)})
			t.model.mu.Unlock()
			t.dragging = false
			return
		}
		if e.Buttons.Contain(pointer.ButtonTertiary) {
			gtx.Execute(clipboard.ReadCmd{Tag: &t.focusTag})
			return
		}
		if !e.Buttons.Contain(pointer.ButtonPrimary) {
			return
		}
		now := time.Now()
		if now.Sub(t.lastClick) < 400*time.Millisecond {
			t.clicks++
		} else {
			t.clicks = 1
		}
		t.lastClick = now
		switch t.clicks {
		case 2:
			t.model.SelectWord(col, row)
			t.dragging = false
		case 3:
			t.model.SelectLine(row)
			t.dragging = false
			t.clicks = 0
		default:
			t.model.SelectBegin(col, row, e.Modifiers&key.ModAlt != 0)
			t.dragging = true
		}
	case pointer.Drag, pointer.Move:
		if reporting {
			t.model.mu.Lock()
			t.model.em.SendMouse(uv.MouseMotionEvent{X: col, Y: row, Button: btn(e.Buttons), Mod: uvMods(e.Modifiers)})
			t.model.mu.Unlock()
			return
		}
		if t.dragging && e.Kind == pointer.Drag {
			t.model.SelectUpdate(col, row)
			// dragging past the edge scrolls
			if e.Position.Y < float32(pad) {
				t.model.Scroll(1)
			} else if e.Position.Y > float32(t.size.Y-pad) {
				t.model.Scroll(-1)
			}
		}
	case pointer.Release:
		if reporting {
			t.model.mu.Lock()
			t.model.em.SendMouse(uv.MouseReleaseEvent{X: col, Y: row, Button: btn(e.Buttons), Mod: uvMods(e.Modifiers)})
			t.model.mu.Unlock()
			return
		}
		if t.dragging {
			t.dragging = false
			if t.model.SelectEnd() { // select to copy, as on X11
				t.copySelection(gtx)
			}
		}
	}
}

func sign(f float32) float32 {
	if f < 0 {
		return -1
	}
	return 1
}

func abs(i int) int {
	if i < 0 {
		return -i
	}
	return i
}

func uvMods(m key.Modifiers) uv.KeyMod {
	var r uv.KeyMod
	if m&key.ModShift != 0 {
		r |= uv.ModShift
	}
	if m&key.ModAlt != 0 {
		r |= uv.ModAlt
	}
	if m&key.ModCtrl != 0 {
		r |= uv.ModCtrl
	}
	return r
}
