// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// model.go: the terminal as data (no graphics): the emulator, the scrollback
// view, the selection and the mouse modes. Everything here is testable without
// a window.

package main

import (
	"image/color"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

// Scrollback sizes: "unlimited" is a very large cap, not infinity (memory).
const (
	scrollDefault   = 10000
	scrollUnlimited = 1000000
)

// cellPos is a position in the text: abs counts lines from the oldest line of
// the scrollback (0) to the last line of the screen.
type cellPos struct{ abs, col int }

func (a cellPos) before(b cellPos) bool { return a.abs < b.abs || (a.abs == b.abs && a.col < b.col) }

type termModel struct {
	mu        sync.Mutex
	em        *vt.Emulator
	cols      int
	rows      int
	off       int // lines the view is scrolled back from the live screen
	lastSB    int // scrollback length at the last look, to keep the view still
	title     string
	bell      func()
	onTitle   func(string)
	mouseMode map[ansi.Mode]bool
	sel       struct {
		on         bool
		a, b       cellPos
		dragging   bool
		rectangle  bool
		hasContent bool
	}
	closed bool

	lastOut atomic.Int64   // unix nanoseconds of the last output (activity alerts)
	onCtl   func([]string) // a request of the program to the GUI (OSC 7799); called under mu
}

// CursorShape is the cursor the program asked for (DECSCUSR): 0 block,
// 1 underline, 2 bar, and whether it blinks.
func (m *termModel) CursorShape() (shape int, blink bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, b := m.em.CursorStyle()
	if st < 0 || st > 2 { // DECSCUSR 7 and up: not a shape
		return 0, b
	}
	return int(st), b
}

// LastOutput is when the program last printed something.
func (m *termModel) LastOutput() time.Time {
	n := m.lastOut.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// newTermModel builds a terminal of cols x rows. send receives what the
// emulator wants to say to the program (answers to queries, keys, mouse).
func newTermModel(cols, rows, scrollback int, send func([]byte)) *termModel {
	m := &termModel{cols: cols, rows: rows, mouseMode: map[ansi.Mode]bool{}}
	m.em = vt.NewEmulator(cols, rows)
	m.em.SetScrollbackSize(scrollback)
	m.em.SetDefaultForegroundColor(theme.fg)
	m.em.SetDefaultBackgroundColor(theme.bg)
	m.em.SetCallbacks(vt.Callbacks{
		Bell: func() { m.bellNow() },
		Title: func(t string) {
			t = cleanTitle(t)
			m.title = t
			if m.onTitle != nil {
				m.onTitle(t)
			}
		},
		EnableMode:  func(md ansi.Mode) { m.mouseMode[md] = true },
		DisableMode: func(md ansi.Mode) { delete(m.mouseMode, md) },
	})
	m.em.RegisterOscHandler(ctlOSC, func(data []byte) bool {
		if args := parseControl(data); args != nil && m.onCtl != nil {
			m.onCtl(args)
		}
		return true
	})
	// What the emulator answers (cursor reports, device attributes, encoded
	// keys) comes out of its pipe: forward it, or its writers would block.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := m.em.Read(buf)
			if n > 0 {
				send(append([]byte(nil), buf[:n]...))
			}
			if err != nil {
				return
			}
		}
	}()
	return m
}

func (m *termModel) bellNow() {
	if m.bell != nil {
		m.bell()
	}
}

// Feed gives the emulator the bytes that came from the server.
func (m *termModel) Feed(p []byte) {
	m.lastOut.Store(time.Now().UnixNano())
	// In slices, letting go of the lock between them: a big burst of output
	// must not keep the window (which paints under the same lock) waiting. The
	// emulator's parser keeps its state across the cuts.
	const slice = 4096
	for len(p) > 0 {
		n := min(len(p), slice)
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return
		}
		_, _ = m.em.Write(p[:n])
		m.afterOutput()
		m.mu.Unlock()
		p = p[n:]
	}
}

// afterOutput keeps a scrolled-back view where it is when new lines arrive and
// drops a selection that the output has moved.
func (m *termModel) afterOutput() {
	sb := m.em.ScrollbackLen()
	if m.em.IsAltScreen() {
		m.off = 0
	} else if m.off > 0 {
		m.off += sb - m.lastSB
		m.off = min(max(m.off, 0), sb)
	}
	if sb != m.lastSB {
		m.sel.on = false
	}
	m.lastSB = sb
}

// Resize changes the size of the screen (the program is told by the caller).
func (m *termModel) Resize(cols, rows int) {
	if cols < 2 || rows < 1 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if cols == m.cols && rows == m.rows {
		return
	}
	m.cols, m.rows = cols, rows
	m.em.Resize(cols, rows)
	m.sel.on = false
	m.off = min(m.off, m.em.ScrollbackLen())
	m.lastSB = m.em.ScrollbackLen()
}

// Size is the current screen size.
func (m *termModel) Size() (cols, rows int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cols, m.rows
}

// SetScrollback changes how many lines are kept (0 = default).
func (m *termModel) SetScrollback(n int) {
	if n <= 0 {
		n = scrollDefault
	}
	m.mu.Lock()
	m.em.SetScrollbackSize(n)
	m.off = min(m.off, m.em.ScrollbackLen())
	m.lastSB = m.em.ScrollbackLen()
	m.mu.Unlock()
}

// Scroll moves the view back (positive) or forward (negative) by n lines.
func (m *termModel) Scroll(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.em.IsAltScreen() {
		return
	}
	m.off = min(max(m.off+n, 0), m.em.ScrollbackLen())
}

// ScrollTo jumps to the live screen (0) or to the oldest line (-1).
func (m *termModel) ScrollTo(top bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.em.IsAltScreen() {
		return
	}
	if top {
		m.off = m.em.ScrollbackLen()
	} else {
		m.off = 0
	}
}

// ScrollState says how far back the view is and how many lines there are.
func (m *termModel) ScrollState() (off, total int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.off, m.em.ScrollbackLen()
}

// MouseReporting says whether the program asked for mouse events.
func (m *termModel) MouseReporting() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.mouseMode) > 0 && (m.mouseMode[ansi.ModeMouseX10] || m.mouseMode[ansi.ModeMouseNormal] ||
		m.mouseMode[ansi.ModeMouseHighlight] || m.mouseMode[ansi.ModeMouseButtonEvent] || m.mouseMode[ansi.ModeMouseAnyEvent])
}

// AltScreen says whether a full-screen program (vim, htop) is running.
func (m *termModel) AltScreen() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.em.IsAltScreen()
}

// cleanTitle is what a program may put in a tab and in the window title: one
// line, no control or format characters (bidi overrides could make
// "moc.knab" read as "bank.com"), at most 128 bytes.
func cleanTitle(t string) string {
	t = strings.Map(func(r rune) rune {
		if r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, t)
	return cleanLine(t, 128)
}

// Title is the title the program set.
func (m *termModel) Title() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.title
}

// cellView is what the painter needs for one cell.
type cellView struct {
	text     string
	fg, bg   color.Color // nil = the theme's
	attrs    uint8
	underl   bool
	width    int
	selected bool
	cont     bool // second half of a wide character
}

// lineAbs returns the line with absolute index abs (nil if there is none).
func (m *termModel) lineCell(abs, col int) *uv.Cell {
	sb := m.em.ScrollbackLen()
	if abs < 0 {
		return nil
	}
	if abs < sb {
		return m.em.ScrollbackCellAt(col, abs)
	}
	if abs-sb >= m.rows || col >= m.cols {
		return nil
	}
	return m.em.CellAt(col, abs-sb)
}

func (m *termModel) selected(abs, col int) bool {
	if !m.sel.on || !m.sel.hasContent {
		return false
	}
	a, b := m.sel.a, m.sel.b
	if b.before(a) {
		a, b = b, a
	}
	p := cellPos{abs, col}
	if m.sel.rectangle {
		c0, c1 := min(a.col, b.col), max(a.col, b.col)
		return abs >= a.abs && abs <= b.abs && col >= c0 && col <= c1
	}
	return !p.before(a) && !b.before(p)
}

// Paint hands every visible cell of the view to fn, row by row, with the lock
// held, plus the cursor (x, y, visible). It is the only way the painter reads
// the terminal.
func (m *termModel) Paint(fn func(row, col int, c cellView), cursor func(x, y int, visible bool)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sb := m.em.ScrollbackLen()
	top := sb - m.off // absolute line shown on the first row
	for r := 0; r < m.rows; r++ {
		for c := 0; c < m.cols; c++ {
			cv := cellView{width: 1, selected: m.selected(top+r, c)}
			if cell := m.lineCell(top+r, c); cell != nil {
				cv.text = cell.Content
				cv.fg, cv.bg = cell.Style.Fg, cell.Style.Bg
				cv.attrs = cell.Style.Attrs
				cv.underl = cell.Style.Underline != uv.UnderlineNone
				cv.width = cell.Width
				if cell.Width == 0 && cell.Content == "" && c > 0 {
					// the right half of a wide character
					if prev := m.lineCell(top+r, c-1); prev != nil && prev.Width == 2 {
						cv.cont = true
					}
				}
			}
			fn(r, c, cv)
		}
	}
	p := m.em.CursorPosition()
	cursor(p.X, p.Y, m.off == 0 && !m.em.CursorHidden())
}

// SelectBegin starts a selection at the view cell (col, row).
func (m *termModel) SelectBegin(col, row int, rectangle bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.viewPos(col, row)
	m.sel.on, m.sel.a, m.sel.b, m.sel.dragging, m.sel.rectangle, m.sel.hasContent = true, p, p, true, rectangle, false
}

// SelectUpdate extends it while the button is held.
func (m *termModel) SelectUpdate(col, row int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.sel.on || !m.sel.dragging {
		return
	}
	m.sel.b = m.viewPos(col, row)
	if m.sel.b != m.sel.a {
		m.sel.hasContent = true
	}
}

// SelectEnd stops dragging and says whether something is selected.
func (m *termModel) SelectEnd() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sel.dragging = false
	if !m.sel.hasContent {
		m.sel.on = false
	}
	return m.sel.on
}

// SelectClear removes the selection.
func (m *termModel) SelectClear() {
	m.mu.Lock()
	m.sel.on, m.sel.dragging, m.sel.hasContent = false, false, false
	m.mu.Unlock()
}

// SelectWord selects the word (or the run of blanks) under the cell.
func (m *termModel) SelectWord(col, row int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.viewPos(col, row)
	isWord := func(c int) bool {
		cell := m.lineCell(p.abs, c)
		if cell == nil || cell.Content == "" || cell.Content == " " {
			return false
		}
		r := []rune(cell.Content)[0]
		return !strings.ContainsRune(" \t\"'`()[]{}<>|;,", r)
	}
	if !isWord(p.col) {
		return
	}
	a, b := p.col, p.col
	for a > 0 && isWord(a-1) {
		a--
	}
	for b < m.cols-1 && isWord(b+1) {
		b++
	}
	m.sel.on, m.sel.dragging, m.sel.rectangle, m.sel.hasContent = true, false, false, true
	m.sel.a, m.sel.b = cellPos{p.abs, a}, cellPos{p.abs, b}
}

// SelectLine selects the whole line under the cell.
func (m *termModel) SelectLine(row int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.viewPos(0, row)
	m.sel.on, m.sel.dragging, m.sel.rectangle, m.sel.hasContent = true, false, false, true
	m.sel.a, m.sel.b = cellPos{p.abs, 0}, cellPos{p.abs, m.cols - 1}
}

func (m *termModel) viewPos(col, row int) cellPos {
	col = min(max(col, 0), m.cols-1)
	row = min(max(row, 0), m.rows-1)
	return cellPos{m.em.ScrollbackLen() - m.off + row, col}
}

// SelectedText is the selection as text: trailing blanks of each line removed,
// lines joined by newlines (a rectangle keeps its columns).
func (m *termModel) SelectedText() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.sel.on || !m.sel.hasContent {
		return ""
	}
	a, b := m.sel.a, m.sel.b
	if b.before(a) {
		a, b = b, a
	}
	var out []string
	for abs := a.abs; abs <= b.abs; abs++ {
		c0, c1 := 0, m.cols-1
		switch {
		case m.sel.rectangle:
			c0, c1 = min(a.col, b.col), max(a.col, b.col)
		default:
			if abs == a.abs {
				c0 = a.col
			}
			if abs == b.abs {
				c1 = b.col
			}
		}
		var sb strings.Builder
		for c := c0; c <= c1; c++ {
			cell := m.lineCell(abs, c)
			if cell == nil || cell.Content == "" {
				if cell == nil {
					sb.WriteByte(' ')
				}
				continue
			}
			sb.WriteString(cell.Content)
		}
		out = append(out, strings.TrimRight(sb.String(), " "))
	}
	return strings.Join(out, "\n")
}

// Text is the whole visible screen as plain text (for tests and saving).
func (m *termModel) Text() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	sb := m.em.ScrollbackLen()
	for r := 0; r < m.rows; r++ {
		var s strings.Builder
		for c := 0; c < m.cols; c++ {
			cell := m.lineCell(sb-m.off+r, c)
			if cell == nil {
				s.WriteByte(' ')
			} else if cell.Content != "" {
				s.WriteString(cell.Content)
			}
		}
		out = append(out, strings.TrimRight(s.String(), " "))
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

// ScrollbackText returns up to max of the last scrollback lines followed by the
// screen, as plain text: what is saved with a workspace.
func (m *termModel) ScrollbackText(max int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	sb := m.em.ScrollbackLen()
	first := 0
	if max > 0 && sb > max {
		first = sb - max
	}
	var out []string
	for abs := first; abs < sb+m.rows; abs++ {
		var s strings.Builder
		for c := 0; c < m.cols; c++ {
			if cell := m.lineCell(abs, c); cell != nil {
				s.WriteString(cell.Content)
			}
		}
		out = append(out, strings.TrimRight(s.String(), " "))
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

// Close stops the model.
func (m *termModel) Close() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	// Closing the pipe the emulator answers on ends the goroutine that reads it
	// (the emulator's own Close would race with that reader).
	if c, ok := m.em.InputPipe().(io.Closer); ok {
		_ = c.Close()
	}
}

var _ io.Writer = (*termModel)(nil)

// Write lets the model be used as an io.Writer (the output of a session).
func (m *termModel) Write(p []byte) (int, error) { m.Feed(p); return len(p), nil }
