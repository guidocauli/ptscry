// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// panes.go: a tab split into panes (as tmux and zellij do), each one an
// ordinary terminal with its own session and connection: the panes of a tab
// can even be on different servers. Splitting, closing (only a detach: the
// session goes on), moving between panes, resizing with the keys or the
// mouse, zoom, preset layouts, input sent to every pane, and the layout
// saved on the server ("ptscry layout"), so that another computer can open
// it again.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"math"
	"strings"
	"time"

	"gioui.org/f32"
	"gioui.org/io/event"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget/material"

	"ptscry/internal/rclient"
)

// maxPanes per tab: more would be unreadable, and a program in a session
// must not be able to open panes without end (see the OSC below).
const maxPanes = 16

// paneNode is a pane (t) or a split of two (a, b).
type paneNode struct {
	t      *tab
	side   bool    // split: a | b side by side (true), or a over b
	ratio  float32 // the share of a
	a, b   *paneNode
	parent *paneNode

	rect   image.Rectangle // last laid out (pixels in the terminal area)
	divTag struct{}        // the divider, for the mouse
	drag   bool
}

// paneFields are the pane state of a tab.
type paneFields struct {
	root   *paneNode // the leader (the tab in the tab bar): its panes; nil = one terminal
	focus  *tab      // the leader: the pane with the keyboard
	zoomed bool      // the leader: only the focused pane is shown
	sync   bool      // the leader: what is typed goes to every pane
	lead   *tab      // a pane that is not the leader: its leader
	preset int       // the leader: the last preset layout used

	watchAct, watchSil bool      // tell about activity / silence in this terminal
	seenOut            time.Time // the output last looked at
	alerted            time.Time // the last alert (activity / silence)
}

func leaf(t *tab) *paneNode { return &paneNode{t: t} }

// leaves are the panes in order (left to right, top to bottom).
func (n *paneNode) leaves() []*tab {
	if n == nil {
		return nil
	}
	if n.t != nil {
		return []*tab{n.t}
	}
	return append(n.a.leaves(), n.b.leaves()...)
}

func (n *paneNode) find(t *tab) *paneNode {
	if n == nil {
		return nil
	}
	if n.t == t {
		return n
	}
	if n.t != nil {
		return nil
	}
	if f := n.a.find(t); f != nil {
		return f
	}
	return n.b.find(t)
}

func (n *paneNode) fixParents(p *paneNode) {
	if n == nil {
		return
	}
	n.parent = p
	n.a.fixParents(n)
	n.b.fixParents(n)
}

// leaderOf is the tab of the tab bar that holds t.
func leaderOf(t *tab) *tab {
	if t.lead != nil {
		return t.lead
	}
	return t
}

// panes of the leader l (just l when it is not split).
func (l *tab) panes() []*tab {
	if l.root == nil {
		return []*tab{l}
	}
	return l.root.leaves()
}

// focused is the pane of l with the keyboard.
func (l *tab) focused() *tab {
	if l.root == nil || l.focus == nil || l.root.find(l.focus) == nil {
		return l
	}
	return l.focus
}

// curTab is the terminal with the keyboard (the focused pane of the active tab).
func (g *gui) curTab() *tab {
	if len(g.tabs) == 0 {
		return nil
	}
	return g.tabs[min(max(g.active, 0), len(g.tabs)-1)].focused()
}

// allTabs is every terminal: the tabs and their panes.
func (g *gui) allTabs() []*tab {
	g.tabMu.Lock()
	defer g.tabMu.Unlock()
	var out []*tab
	for _, l := range g.tabs {
		out = append(out, l.panes()...)
	}
	return out
}

// ---------------------------------------------------------------- split / close

// splitPane opens a new pane next to the focused one: the same connection,
// a new session; sp, if not nil, says what to open instead (another
// connection, from the "Nuova sessione" dialog).
func (g *gui) splitPane(side bool, sp *tabSpec) {
	if len(g.tabs) == 0 {
		return
	}
	l := g.tabs[g.active]
	if len(l.panes()) >= maxPanes {
		g.flash("al massimo %d pannelli per scheda", maxPanes)
		return
	}
	cur := l.focused()
	spec := tabSpec{profile: cur.profile, shell: cur.shell}
	if sp != nil {
		spec = *sp
	}
	if spec.profile == "" {
		spec.profile = g.set.Default
	}
	if spec.session == "" {
		spec.session = g.freeSessionName()
	}
	if _, _, _, err := rclient.Cmdline([]string{"new", "-A", "-n", spec.session}); err != nil {
		g.flash("nome della sessione non valido: solo lettere, cifre e _ . : = @ + , -")
		return
	}
	if spec.cwd == "" && sp == nil {
		spec.cwd = cur.getCwd() // the new pane starts where this one is
	}
	t := g.newTab(g.hubFor(spec.profile), spec, g.set.Scrollback)
	g.insertPane(l, cur, t, side, 0.5)
	g.saveWorkspace()
	g.invalidate()
}

// insertPane puts t next to cur in the panes of the leader l.
func (g *gui) insertPane(l, cur, t *tab, side bool, ratio float32) {
	g.tabMu.Lock()
	defer g.tabMu.Unlock()
	if l.root == nil {
		l.root = leaf(l)
	}
	n := l.root.find(cur)
	if n == nil {
		n = l.root
	}
	old := &paneNode{t: n.t, a: n.a, b: n.b, side: n.side, ratio: n.ratio}
	*n = paneNode{side: side, ratio: ratio, a: old, b: leaf(t), parent: n.parent}
	l.root.fixParents(nil)
	t.lead = l
	l.focus = t
	l.zoomed = false
}

// closePane detaches the session of one pane and removes it; the last pane
// closes the tab. Closing the leader's own pane hands the tab to another one.
func (g *gui) closePane(t *tab) {
	l := leaderOf(t)
	if l.root == nil || len(l.root.leaves()) < 2 {
		g.closeTab(l)
		return
	}
	t.Close()
	g.tabMu.Lock()
	n := l.root.find(t)
	if n == nil || n.parent == nil {
		g.tabMu.Unlock()
		return
	}
	p := n.parent
	other := p.a
	if other == n {
		other = p.b
	}
	*p = paneNode{t: other.t, a: other.a, b: other.b, side: other.side, ratio: other.ratio, parent: p.parent}
	l.root.fixParents(nil)
	if l.focus == t || (l.focus == nil && t == l) {
		l.focus = other.leaves()[0] // the pane that took its place
	}
	nl := l
	if t == l {
		// the leader goes away: the first of the others leads the tab
		nl = l.root.leaves()[0]
		nl.root, nl.focus, nl.zoomed, nl.sync, nl.preset = l.root, l.focus, l.zoomed, l.sync, l.preset
		nl.lead = nil
		for _, x := range nl.root.leaves() {
			if x != nl {
				x.lead = nl
			}
		}
		for i, x := range g.tabs {
			if x == l {
				g.tabs[i] = nl
			}
		}
		l.root, l.focus = nil, nil
	}
	if len(nl.root.leaves()) == 1 {
		nl.root, nl.focus, nl.zoomed, nl.sync = nil, nil, false, false
	}
	t.lead = nil
	g.tabMu.Unlock()
	g.saveWorkspace()
	g.invalidate()
}

// ---------------------------------------------------------------- moving, resizing

// focusPane moves the keyboard to the pane in a direction (dx, dy).
func (g *gui) focusPane(dx, dy int) {
	l := g.tabs[g.active]
	if l.root == nil {
		return
	}
	cur := l.root.find(l.focused())
	if cur == nil {
		return
	}
	c := cur.rect
	best, bestD := (*paneNode)(nil), math.MaxFloat64
	var walk func(n *paneNode)
	walk = func(n *paneNode) {
		if n == nil {
			return
		}
		if n.t == nil {
			walk(n.a)
			walk(n.b)
			return
		}
		if n == cur {
			return
		}
		r := n.rect
		ok := false
		switch {
		case dx > 0:
			ok = r.Min.X >= c.Max.X-2 && r.Min.Y < c.Max.Y && r.Max.Y > c.Min.Y
		case dx < 0:
			ok = r.Max.X <= c.Min.X+2 && r.Min.Y < c.Max.Y && r.Max.Y > c.Min.Y
		case dy > 0:
			ok = r.Min.Y >= c.Max.Y-2 && r.Min.X < c.Max.X && r.Max.X > c.Min.X
		case dy < 0:
			ok = r.Max.Y <= c.Min.Y+2 && r.Min.X < c.Max.X && r.Max.X > c.Min.X
		}
		if !ok {
			return
		}
		ddx := float64((r.Min.X + r.Max.X - c.Min.X - c.Max.X) / 2)
		ddy := float64((r.Min.Y + r.Max.Y - c.Min.Y - c.Max.Y) / 2)
		if d := ddx*ddx + ddy*ddy; d < bestD {
			best, bestD = n, d
		}
	}
	walk(l.root)
	if best != nil {
		l.focus = best.t
		g.invalidate()
	}
}

// cyclePane gives the keyboard to the next pane (in order).
func (g *gui) cyclePane(d int) {
	l := g.tabs[g.active]
	ps := l.panes()
	if len(ps) < 2 {
		return
	}
	for i, p := range ps {
		if p == l.focused() {
			l.focus = ps[(i+d+len(ps))%len(ps)]
			break
		}
	}
	g.invalidate()
}

// resizePane moves the nearest divider in a direction (dx, dy) by 5%.
func (g *gui) resizePane(dx, dy int) {
	l := g.tabs[g.active]
	if l.root == nil {
		return
	}
	n := l.root.find(l.focused())
	for n != nil && n.parent != nil {
		p := n.parent
		if p.side == (dx != 0) {
			step := float32(0.05)
			if dx < 0 || dy < 0 {
				step = -step
			}
			p.ratio = min(max(p.ratio+step, 0.1), 0.9)
			g.saveWorkspace()
			g.invalidate()
			return
		}
		n = p
	}
}

func (g *gui) toggleZoomPane() {
	l := g.tabs[g.active]
	if l.root == nil {
		return
	}
	l.zoomed = !l.zoomed
	g.invalidate()
}

// presetNames are the preset layouts, in the order Alt+Shift+Space goes through them.
var presetNames = []string{"affiancati", "impilati", "principale a sinistra", "principale in alto", "griglia"}

// nextPreset arranges the panes in the next preset layout.
func (g *gui) nextPreset() {
	l := g.tabs[g.active]
	ps := l.panes()
	if len(ps) < 2 {
		return
	}
	k := l.preset % len(presetNames) // the next one to use
	l.preset = k + 1
	g.tabMu.Lock()
	l.root = presetTree(ps, k)
	l.root.fixParents(nil)
	l.zoomed = false
	g.tabMu.Unlock()
	g.flashInfo("disposizione: %s", presetNames[k])
	g.saveWorkspace()
	g.invalidate()
}

// even splits ps in equal parts along one axis.
func even(ps []*tab, side bool) *paneNode {
	if len(ps) == 1 {
		return leaf(ps[0])
	}
	return &paneNode{side: side, ratio: 1 / float32(len(ps)), a: leaf(ps[0]), b: even(ps[1:], side)}
}

func presetTree(ps []*tab, k int) *paneNode {
	switch k {
	case 0:
		return even(ps, true)
	case 1:
		return even(ps, false)
	case 2:
		return &paneNode{side: true, ratio: 0.6, a: leaf(ps[0]), b: even(ps[1:], false)}
	case 3:
		return &paneNode{side: false, ratio: 0.6, a: leaf(ps[0]), b: even(ps[1:], true)}
	}
	// grid: rows of about sqrt(n) panes
	per := int(math.Ceil(math.Sqrt(float64(len(ps)))))
	var rows []*paneNode
	for i := 0; i < len(ps); i += per {
		rows = append(rows, even(ps[i:min(i+per, len(ps))], true))
	}
	var stack func(r []*paneNode) *paneNode
	stack = func(r []*paneNode) *paneNode {
		if len(r) == 1 {
			return r[0]
		}
		return &paneNode{side: false, ratio: 1 / float32(len(r)), a: r[0], b: stack(r[1:])}
	}
	return stack(rows)
}

// toggleSync sends what is typed to every pane of the tab (or stops).
func (g *gui) toggleSync() {
	l := g.tabs[g.active]
	if len(l.panes()) < 2 {
		g.flash("l'input sincronizzato serve con piu' pannelli")
		return
	}
	l.sync = !l.sync
	if l.sync {
		g.flash("INPUT SINCRONIZZATO: cio' che scrivi va a tutti i %d pannelli (Alt+Shift+S per smettere)", len(l.panes()))
	} else {
		g.flashInfo("input sincronizzato disattivato")
	}
	g.invalidate()
}

// mirrors are the other views that get what is typed in t (input sincronizzato).
func (g *gui) mirrors(t *tab) []*termView {
	l := leaderOf(t)
	if !l.sync || l.root == nil {
		return nil
	}
	var out []*termView
	for _, p := range l.root.leaves() {
		if p != t && !p.ro {
			out = append(out, p.view)
		}
	}
	return out
}

// ---------------------------------------------------------------- drawing

var (
	syncColor  = color.NRGBA{R: 0xe0, G: 0x40, B: 0x40, A: 0xff}
	paneBorder = color.NRGBA{R: 0x50, G: 0x55, B: 0x60, A: 0xff}
)

// layoutPanes draws the panes of the leader l in the terminal area.
func (g *gui) layoutPanes(gtx layout.Context, l *tab, interactive bool) {
	size := gtx.Constraints.Max
	if l.root == nil {
		g.layoutPane(gtx, l, l, image.Rectangle{Max: size}, interactive, false)
		return
	}
	if l.zoomed {
		f := l.focused()
		if n := l.root.find(f); n != nil {
			n.rect = image.Rectangle{Max: size}
		}
		g.layoutPane(gtx, l, f, image.Rectangle{Max: size}, interactive, true)
		return
	}
	g.layoutNode(gtx, l, l.root, image.Rectangle{Max: size}, interactive)
}

func (g *gui) layoutNode(gtx layout.Context, l *tab, n *paneNode, r image.Rectangle, interactive bool) {
	n.rect = r
	if n.t != nil {
		g.layoutPane(gtx, l, n.t, r, interactive, true)
		return
	}
	div := max(gtx.Dp(unit.Dp(3)), 2)
	ra, rb, rd := r, r, r
	if n.side {
		w := r.Dx() - div
		x := r.Min.X + int(float32(w)*n.ratio)
		ra.Max.X, rd.Min.X, rd.Max.X, rb.Min.X = x, x, x+div, x+div
	} else {
		h := r.Dy() - div
		y := r.Min.Y + int(float32(h)*n.ratio)
		ra.Max.Y, rd.Min.Y, rd.Max.Y, rb.Min.Y = y, y, y+div, y+div
	}
	g.layoutNode(gtx, l, n.a, ra, interactive)
	g.layoutNode(gtx, l, n.b, rb, interactive)
	g.divider(gtx, l, n, r, rd)
}

// divider draws the line between two panes and lets the mouse move it.
func (g *gui) divider(gtx layout.Context, l *tab, n *paneNode, r, rd image.Rectangle) {
	for {
		ev, ok := gtx.Event(pointer.Filter{Target: &n.divTag, Kinds: pointer.Press | pointer.Drag | pointer.Release | pointer.Cancel})
		if !ok {
			break
		}
		e, ok := ev.(pointer.Event)
		if !ok {
			continue
		}
		switch e.Kind {
		case pointer.Press:
			n.drag = true
		case pointer.Release, pointer.Cancel:
			if n.drag {
				g.saveWorkspace()
			}
			n.drag = false
		case pointer.Drag:
			if !n.drag {
				continue
			}
			// e.Position is relative to the divider
			p := f32.Pt(float32(rd.Min.X)+e.Position.X, float32(rd.Min.Y)+e.Position.Y)
			if n.side && r.Dx() > 0 {
				n.ratio = min(max((p.X-float32(r.Min.X))/float32(r.Dx()), 0.08), 0.92)
			} else if r.Dy() > 0 {
				n.ratio = min(max((p.Y-float32(r.Min.Y))/float32(r.Dy()), 0.08), 0.92)
			}
			g.invalidate()
		}
	}
	c := paneBorder
	if l.sync {
		c = syncColor
	}
	paint.FillShape(gtx.Ops, c, clip.Rect(rd).Op())
	// a wider grip than the line, for the mouse
	grip := rd
	if n.side {
		grip.Min.X -= gtx.Dp(3)
		grip.Max.X += gtx.Dp(3)
	} else {
		grip.Min.Y -= gtx.Dp(3)
		grip.Max.Y += gtx.Dp(3)
	}
	st := op.Offset(grip.Min).Push(gtx.Ops)
	area := clip.Rect{Max: grip.Size()}.Push(gtx.Ops)
	if n.side {
		pointer.CursorColResize.Add(gtx.Ops)
	} else {
		pointer.CursorRowResize.Add(gtx.Ops)
	}
	event.Op(gtx.Ops, &n.divTag)
	area.Pop()
	st.Pop()
}

// layoutPane draws one terminal in r; with frame, the focused pane is marked.
func (g *gui) layoutPane(gtx layout.Context, l, t *tab, r image.Rectangle, interactive, frame bool) {
	if r.Dx() <= 0 || r.Dy() <= 0 {
		return
	}
	st := op.Offset(r.Min).Push(gtx.Ops)
	tg := gtx
	tg.Constraints = layout.Exact(r.Size())
	focus := t == l.focused()
	v := t.view
	v.focused = tg.Focused(&v.focusTag)
	if interactive && focus {
		v.cursorOn = time.Now().UnixMilli()/530%2 == 0 || time.Since(v.cursorAt) < 600*time.Millisecond
	} else if !focus {
		v.cursorOn = true // a hollow cursor: the focus is elsewhere
	}
	v.onPress = func() {
		if l.focus != t && l.root != nil {
			l.focus = t
			g.invalidate()
		}
	}
	v.Layout(tg, interactive && focus)
	t.seenOut = time.Now()
	if frame && l.root != nil {
		c := theme.accent
		if l.sync {
			c = syncColor
		}
		if !focus && !l.sync {
			c = color.NRGBA{}
		}
		if c.A > 0 {
			w := max(gtx.Dp(unit.Dp(1.5)), 1)
			sz := r.Size()
			for _, rr := range []image.Rectangle{
				{Max: image.Pt(sz.X, w)}, {Min: image.Pt(0, sz.Y-w), Max: sz},
				{Max: image.Pt(w, sz.Y)}, {Min: image.Pt(sz.X-w, 0), Max: sz},
			} {
				paint.FillShape(gtx.Ops, c, clip.Rect(rr).Op())
			}
		}
	}
	st.Pop()
}

// ---------------------------------------------------------------- saving

// paneState is the saved tree of the panes of a tab.
type paneState struct {
	Side  bool       `json:"side,omitempty"`
	Ratio float32    `json:"ratio,omitempty"`
	A     *paneState `json:"a,omitempty"`
	B     *paneState `json:"b,omitempty"`
	Lead  bool       `json:"lead,omitempty"` // the place of the tab itself
	Tab   *tabState  `json:"tab,omitempty"`  // another pane
	Focus bool       `json:"focus,omitempty"`
}

func (l *tab) paneState() *paneState {
	if l.root == nil {
		return nil
	}
	var conv func(n *paneNode) *paneState
	conv = func(n *paneNode) *paneState {
		if n.t != nil {
			ps := &paneState{Focus: n.t == l.focused()}
			if n.t == l {
				ps.Lead = true
			} else {
				st := n.t.state()
				ps.Tab = &st
			}
			return ps
		}
		return &paneState{Side: n.side, Ratio: n.ratio, A: conv(n.a), B: conv(n.b)}
	}
	return conv(l.root)
}

// openPanes rebuilds the panes of the leader l from a saved tree.
func (g *gui) openPanes(l *tab, ps *paneState) {
	if ps == nil {
		return
	}
	count := 0
	var focus *tab
	var build func(s *paneState) *paneNode
	build = func(s *paneState) *paneNode {
		if s == nil {
			return nil
		}
		if s.A != nil && s.B != nil {
			a, b := build(s.A), build(s.B)
			switch {
			case a == nil:
				return b
			case b == nil:
				return a
			}
			r := s.Ratio
			if r <= 0.05 || r >= 0.95 {
				r = 0.5
			}
			return &paneNode{side: s.Side, ratio: r, a: a, b: b}
		}
		var t *tab
		switch {
		case s.Lead:
			t = l
		case s.Tab != nil && count < maxPanes-1:
			count++
			sp := specOf(*s.Tab)
			if sp.profile == "" {
				sp.profile = l.profile
			}
			if g.set.profile(sp.profile) == nil || len(g.set.cleanTabs([]tabState{{Profile: sp.profile, Session: sp.session}})) == 0 {
				return nil // a connection that no longer exists, a session name not valid
			}
			t = g.newTab(g.hubFor(sp.profile), sp, g.set.Scrollback)
			t.lead = l
		default:
			return nil
		}
		if s.Focus {
			focus = t
		}
		return leaf(t)
	}
	root := build(ps)
	if root == nil || root.t != nil {
		return
	}
	if root.find(l) == nil { // the leader must be in it
		root = &paneNode{side: true, ratio: 0.5, a: leaf(l), b: root}
	}
	root.fixParents(nil)
	g.tabMu.Lock()
	l.root, l.focus = root, focus
	g.tabMu.Unlock()
}

// ---------------------------------------------------------------- layouts on the server

// serverLayout is the format of "ptscry layout" shared with the terminal
// client: a tree of sessions. Host is set only for a pane of another
// connection (the terminal client skips it).
type serverLayout struct {
	Version int        `json:"version"`
	Root    *layoutDoc `json:"root"`
}

type layoutDoc struct {
	Split   string     `json:"split,omitempty"` // "lr" side by side, "tb" one over the other
	Ratio   float32    `json:"ratio,omitempty"`
	A       *layoutDoc `json:"a,omitempty"`
	B       *layoutDoc `json:"b,omitempty"`
	Session string     `json:"session,omitempty"`
	Host    string     `json:"host,omitempty"` // the name of the connection in the GUI
	Focus   bool       `json:"focus,omitempty"`
}

// layoutOf describes the panes of the tab for the server of its leader.
func (g *gui) layoutOf(l *tab) ([]byte, error) {
	var conv func(n *paneNode) *layoutDoc
	conv = func(n *paneNode) *layoutDoc {
		if n.t != nil {
			d := &layoutDoc{Session: n.t.session, Focus: n.t == l.focused()}
			if n.t.profile != l.profile {
				d.Host = n.t.profile
			}
			return d
		}
		s := "tb"
		if n.side {
			s = "lr"
		}
		return &layoutDoc{Split: s, Ratio: n.ratio, A: conv(n.a), B: conv(n.b)}
	}
	root := leaf(l)
	if l.root != nil {
		root = l.root
	}
	b, err := json.Marshal(serverLayout{Version: 1, Root: conv(root)})
	if err != nil {
		return nil, err
	}
	if len(b) > 16*1024 {
		return nil, errors.New("disposizione troppo grande")
	}
	return b, nil
}

// applyLayout opens a layout read from the server of profile as a new tab.
func (g *gui) applyLayout(profile string, data []byte, onlyHere bool) error {
	var sl serverLayout
	if err := json.Unmarshal(data, &sl); err != nil || sl.Root == nil {
		return errors.New("disposizione non valida")
	}
	count := 0
	var conv func(d *layoutDoc, depth int) *paneState
	conv = func(d *layoutDoc, depth int) *paneState {
		if d == nil || depth > 20 {
			return nil
		}
		if d.A != nil && d.B != nil {
			return &paneState{Side: d.Split == "lr", Ratio: d.Ratio, A: conv(d.A, depth+1), B: conv(d.B, depth+1)}
		}
		if !nameRe.MatchString(d.Session) || count >= maxPanes {
			return nil
		}
		p := profile
		if d.Host != "" {
			if onlyHere {
				return nil // asked by a program: nothing on other connections
			}
			if g.set.profile(d.Host) != nil {
				p = d.Host
			}
		}
		count++
		return &paneState{Tab: &tabState{Profile: p, Session: d.Session}, Focus: d.Focus}
	}
	ps := conv(sl.Root, 0)
	if ps == nil {
		return errors.New("la disposizione non contiene sessioni valide")
	}
	// the first pane leads the tab
	var first *paneState
	var find func(s *paneState)
	find = func(s *paneState) {
		if s == nil || first != nil {
			return
		}
		if s.Tab != nil {
			first = s
			return
		}
		find(s.A)
		find(s.B)
	}
	find(ps)
	if first == nil {
		return errors.New("la disposizione non contiene sessioni valide")
	}
	lead := *first.Tab
	first.Tab, first.Lead = nil, true
	g.openSpec(specOf(lead))
	l := g.tabs[len(g.tabs)-1]
	g.openPanes(l, ps)
	g.saveWorkspace()
	g.invalidate()
	return nil
}

// ---------------------------------------------------------------- activity / silence

const silenceAlert = 30 * time.Second

// checkActivity marks the terminals not on screen that printed something
// (or went silent), for those that asked; it runs with the cursor blink.
func (g *gui) checkActivity() {
	now := time.Now()
	visible := map[*tab]bool{}
	if len(g.tabs) > 0 {
		for _, p := range g.tabs[min(max(g.active, 0), len(g.tabs)-1)].panes() {
			visible[p] = true
		}
	}
	for _, t := range g.allTabs() {
		if visible[t] || (!t.watchAct && !t.watchSil) {
			continue
		}
		last := t.model.LastOutput()
		if t.watchAct && last.After(t.seenOut) && last.After(t.alerted) && now.Sub(t.alerted) > silenceAlert {
			t.alerted = now
			g.flash("attivita' in %s", t.title())
			g.bell()
		}
		quiet := last
		if t.seenOut.After(quiet) {
			quiet = t.seenOut
		}
		if t.watchSil && now.Sub(quiet) >= silenceAlert && t.alerted.Before(quiet) {
			t.alerted = now
			g.flash("%s: nessun output da %s", t.title(), silenceAlert)
		}
	}
}

// activityMark is shown on a tab with news in a pane not on screen.
func (g *gui) activityMark(l *tab) string {
	for _, p := range l.panes() {
		if p.watchAct && p.model.LastOutput().After(p.seenOut) && p.alerted.After(p.seenOut) {
			return "● "
		}
	}
	return ""
}

// ---------------------------------------------------------------- control from the session

// The program in a pane can ask the GUI (and the terminal client) for a
// pane: "ptscry split" and "ptscry layout apply" print OSC 7799. Whatever
// the session prints can carry it, so it can only do what the person could
// do with a key, within the limits: no command is ever run, a split opens a
// new session of the same connection, a layout comes from the user's own
// server; at most one request a second.
const ctlOSC = 7799

func (g *gui) onControl(t *tab, args []string) {
	if len(args) == 0 {
		return
	}
	now := time.Now()
	t.mu.Lock()
	live := t.liveAt
	t.mu.Unlock()
	// right after an attach comes the replay of what the session printed
	// before: an old request in it must not be done again; and one a second
	if now.Sub(t.lastCtl) < time.Second || now.Sub(live) < 3*time.Second {
		return
	}
	t.lastCtl = now
	if args[0] == "session" {
		// the ptscry client in the terminal changed session (prefix s, ( ),
		// L, c, $): reattach that one after a lost connection. Believed only
		// shortly after the person typed the prefix in this terminal: any
		// output could carry the sequence.
		typed := time.Unix(0, t.prefixAt.Load())
		if len(args) == 2 && nameRe.MatchString(args[1]) && t.h.p.persistent() && t.h.p.Mode != "ssh" &&
			now.Sub(typed) < 2*time.Minute {
			t.mu.Lock()
			t.session = args[1]
			t.mu.Unlock()
			g.saveWorkspace()
		}
		return
	}
	// The other requests only from the terminal with the keyboard: a pane
	// (perhaps of another server) must not act behind the person's back,
	// and never bring its tab to the front or take the keyboard.
	if g.curTab() != t {
		return
	}
	switch args[0] {
	case "split":
		g.splitPane(len(args) < 2 || args[1] != "tb", nil)
	case "layout":
		if len(args) != 2 || !nameRe.MatchString(args[1]) || now.Sub(g.lastOSCLayout) < 10*time.Second || len(g.tabs) >= 32 {
			return
		}
		g.lastOSCLayout = now
		name := args[1]
		go func() {
			if err := g.loadServerLayout(t.h, name, true); err != nil {
				g.flash("disposizione %s: %v", name, err)
			}
		}()
	}
	g.invalidate()
}

// parseControl reads the data of OSC 7799 ("7799;split;lr").
func parseControl(data []byte) []string {
	s := string(data)
	if len(s) > 200 {
		return nil
	}
	parts := strings.Split(s, ";")
	if len(parts) < 2 || parts[0] != fmt.Sprint(ctlOSC) {
		return nil
	}
	for _, p := range parts[1:] {
		if !nameRe.MatchString(p) {
			return nil
		}
	}
	return parts[1:]
}

type ctlReq struct {
	t    *tab
	args []string
}

// takeControls serves the requests of the programs (on the UI goroutine).
func (g *gui) takeControls() {
	for {
		select {
		case r := <-g.ctl:
			if !r.t.isClosing() {
				g.onControl(r.t, r.args)
			}
		default:
			return
		}
	}
}

// runOnUI runs f on the UI goroutine, at the next frame.
func (g *gui) runOnUI(f func()) {
	g.uiMu.Lock()
	g.uiQueue = append(g.uiQueue, f)
	g.uiMu.Unlock()
	g.invalidate()
}

func (g *gui) runQueued() {
	g.uiMu.Lock()
	q := g.uiQueue
	g.uiQueue = nil
	g.uiMu.Unlock()
	for _, f := range q {
		f()
	}
}

// ---------------------------------------------------------------- status bar

func (g *gui) statusBarOn() bool {
	g.setMu.Lock()
	defer g.setMu.Unlock()
	return g.set.StatusBar != "off"
}

func (g *gui) toggleStatusBar() {
	g.setMu.Lock()
	if g.set.StatusBar == "off" {
		g.set.StatusBar = "on"
	} else {
		g.set.StatusBar = "off"
	}
	g.setMu.Unlock()
	g.saveWorkspace()
	g.invalidate()
}

// statusBar: connection and session of the pane with the keyboard, the
// panes, the warnings (input sincronizzato, zoom, frozen) and the time.
func (g *gui) statusBar(gtx layout.Context, l, cur *tab) {
	size := gtx.Constraints.Max
	paint.FillShape(gtx.Ops, theme.chrome, clip.Rect{Max: size}.Op())
	kc := kindColor(cur.kind())
	paint.FillShape(gtx.Ops, kc, clip.Rect{Max: image.Pt(gtx.Dp(4), size.Y)}.Op())
	where := cur.profile
	if p := g.set.profile(cur.profile); p != nil && p.Target != "" && (p.Mode == "remote" || p.Mode == "ssh") {
		where = p.Name + " (" + p.Target + ")"
	}
	left := where
	if cur.kind() != "ssh" && cur.h.p.Mode != "shell" {
		left += "  ·  sessione " + cur.session
	}
	ps := l.panes()
	if len(ps) > 1 {
		for i, p := range ps {
			if p == cur {
				left += fmt.Sprintf("  ·  pannello %d/%d", i+1, len(ps))
			}
		}
	}
	var warn []string
	if l.sync {
		warn = append(warn, "INPUT SINCRONIZZATO")
	}
	if l.zoomed {
		warn = append(warn, "ZOOM")
	}
	if cur.isFrozen() {
		warn = append(warn, "[II] congelata")
	}
	if cur.ro {
		warn = append(warn, "sola lettura")
	}
	if cur.watchAct {
		warn = append(warn, "avviso attivita'")
	}
	if cur.watchSil {
		warn = append(warn, "avviso silenzio")
	}
	right := time.Now().Format("15:04")
	layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			l := materialLabel(g, 12, left, theme.chromeText)
			return layout.Inset{Left: 10, Top: 3}.Layout(gtx, l.Layout)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if len(warn) == 0 {
				return layout.Dimensions{}
			}
			lb := materialLabel(g, 12, strings.Join(warn, "  ·  "), theme.warn)
			if l.sync {
				lb.Color = syncColor
			}
			return layout.Inset{Right: 14, Top: 3}.Layout(gtx, lb.Layout)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			lb := materialLabel(g, 12, right, theme.chromeText)
			return layout.Inset{Right: 10, Top: 3}.Layout(gtx, lb.Layout)
		}),
	)
}

func materialLabel(g *gui, size unit.Sp, s string, c color.NRGBA) material.LabelStyle {
	l := material.Label(g.th, size, s)
	l.Color = c
	l.MaxLines = 1
	return l
}

// paneMenu: the functions of the panes (menu bar and the terminal's menu).
func (g *gui) paneMenu(cur *tab) []menuItem {
	have := cur != nil
	var l *tab
	n := 0
	if have {
		l = leaderOf(cur)
		n = len(l.panes())
	}
	multi := n > 1
	room := have && n < maxPanes
	sep := menuItem{sep: true}
	zoomL, syncL := "Ingrandisci il pannello", "Input sincronizzato su tutti i pannelli"
	if multi && l.zoomed {
		zoomL = "Mostra tutti i pannelli"
	}
	if multi && l.sync {
		syncL = "Disattiva l'input sincronizzato"
	}
	actL, silL := "Avvisa di attivita' in questo terminale", "Avvisa di 30 s di silenzio in questo terminale"
	if have && cur.watchAct {
		actL = "Non avvisare piu' di attivita'"
	}
	if have && cur.watchSil {
		silL = "Non avvisare piu' del silenzio"
	}
	return []menuItem{
		{label: "Dividi a destra", enabled: room, key: "Alt+Shift++", do: func(layout.Context) { g.splitPane(true, nil) }},
		{label: "Dividi sotto", enabled: room, key: "Alt+Shift+-", do: func(layout.Context) { g.splitPane(false, nil) }},
		{label: "Dividi con un'altra connessione…", enabled: room, do: func(layout.Context) { g.splitWithDialog() }},
		{label: "Chiudi il pannello", enabled: have, key: sc("W"), do: func(layout.Context) { g.closePane(cur) }},
		sep,
		{label: "Pannello successivo", enabled: multi, key: "Alt+Shift+O", do: func(layout.Context) { g.cyclePane(1) }},
		{label: zoomL, enabled: multi, key: "Alt+Shift+Z", do: func(layout.Context) { g.toggleZoomPane() }},
		{label: "Disposizione successiva", enabled: multi, key: "Alt+Shift+Spazio", do: func(layout.Context) { g.nextPreset() }},
		{label: syncL, enabled: multi, key: "Alt+Shift+S", do: func(layout.Context) { g.toggleSync() }},
		sep,
		{label: "Disposizioni sul server…", enabled: have && cur.h.p.persistent() && cur.h.p.Mode != "ssh", key: "Alt+Shift+L", do: func(layout.Context) { g.openLayoutDialog() }},
		sep,
		{label: actL, enabled: have, do: func(layout.Context) { cur.watchAct = !cur.watchAct; cur.seenOut = time.Now(); g.invalidate() }},
		{label: silL, enabled: have, do: func(layout.Context) { cur.watchSil = !cur.watchSil; cur.seenOut = time.Now(); g.invalidate() }},
	}
}

// splitWithDialog: the "Nuova sessione" dialog, whose result goes into a new pane.
func (g *gui) splitWithDialog() {
	g.openNewSessionDialog()
	g.dlgMu.Lock()
	if d, ok := g.dlg.(*newSessionDialog); ok {
		d.split = true
	}
	g.dlgMu.Unlock()
}

// paneKey runs a pane shortcut typed in t; false: not for the panes (the
// arrows with Alt go to the program when the tab has one pane).
func (g *gui) paneKey(t *tab, act string) bool {
	l := leaderOf(t)
	multi := len(l.panes()) > 1
	switch act {
	case "split-lr":
		g.splitPane(true, nil)
	case "split-tb":
		g.splitPane(false, nil)
	case "layouts":
		if !t.h.p.persistent() || t.h.p.Mode == "ssh" {
			return false
		}
		g.openLayoutDialog()
	default:
		if !multi {
			return false
		}
		switch act {
		case "focus-left":
			g.focusPane(-1, 0)
		case "focus-right":
			g.focusPane(1, 0)
		case "focus-up":
			g.focusPane(0, -1)
		case "focus-down":
			g.focusPane(0, 1)
		case "grow-left":
			g.resizePane(-1, 0)
		case "grow-right":
			g.resizePane(1, 0)
		case "grow-up":
			g.resizePane(0, -1)
		case "grow-down":
			g.resizePane(0, 1)
		case "next":
			g.cyclePane(1)
		case "zoom":
			g.toggleZoomPane()
		case "sync":
			g.toggleSync()
		case "preset":
			g.nextPreset()
		default:
			return false
		}
	}
	return true
}

// sessionName is the session of the terminal (it may change: OSC 7799 session).
func (t *tab) sessionName() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.session
}
