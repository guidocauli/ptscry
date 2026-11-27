//go:build !windows

// ptscry-tui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// app.go: the screen, the keys and the mouse.

package main

import (
	"crypto/rand"
	"fmt"
	"sync"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
)

type app struct {
	be     backend
	prefix byte
	t      *uv.Terminal
	scr    *uv.TerminalScreen
	w, h   int
	status bool // the status bar
	mouse  bool

	mu     sync.Mutex // root, focus, the panes
	root   *node
	focus  *pane
	last   *pane // the pane focused before (prefix ;)
	zoomed bool
	sync   bool
	preset int
	nextID int

	pending   bool // prefix typed
	drag      *node
	overlay   overlay
	notice    string
	noticeAt  time.Time
	noticeMu  sync.Mutex // notice, noticeAt, bellAt: written by other goroutines
	bellAt    time.Time
	dirty     chan struct{}
	ctl       chan ctlReq
	gone      chan *pane
	quit      chan string
	detaching bool

	lastOSCLayout time.Time

	cursorMode string // client.conf "cursor": "" (as the program asks), "blink", "steady"
	lastCursor int    // the DECSCUSR last sent to the terminal
}

type ctlReq struct {
	p    *pane
	args []string
	plan *plan         // a layout to open (from loadLayout)
	done chan struct{} // closed when it is done
}

// overlay takes the keys for a while (a question, the help).
type overlay interface {
	key(a *app, k uv.KeyPressEvent) (done bool)
	line(w int) string // drawn on the status bar
}

func (a *app) redraw() {
	select {
	case a.dirty <- struct{}{}:
	default:
	}
}

func (a *app) say(format string, args ...any) {
	a.noticeMu.Lock()
	a.notice, a.noticeAt = fmt.Sprintf(format, args...), time.Now()
	a.noticeMu.Unlock()
	a.redraw()
}

// bell comes from the panes' goroutines.
func (a *app) bell() {
	a.noticeMu.Lock()
	a.bellAt = time.Now()
	a.noticeMu.Unlock()
	a.redraw()
}

func (a *app) notices() (notice string, at, bell time.Time) {
	a.noticeMu.Lock()
	defer a.noticeMu.Unlock()
	return a.notice, a.noticeAt, a.bellAt
}

func (a *app) panes() []*pane { return a.root.leaves() }

// area is where the panes go (all but the status bar).
func (a *app) area() uv.Rectangle {
	h := a.h
	if a.status {
		h--
	}
	return uv.Rect(0, 0, a.w, max(h, 1))
}

// layout places the panes and gives each its size.
func (a *app) layout() {
	if a.root == nil {
		return
	}
	r := a.area()
	a.root.place(r)
	if a.zoomed && a.focus != nil {
		if n := a.root.find(a.focus); n != nil {
			n.rect = r
		}
		a.focus.resize(r.Dx(), r.Dy())
		return
	}
	for _, p := range a.panes() {
		if n := a.root.find(p); n != nil {
			p.resize(n.rect.Dx(), n.rect.Dy())
		}
	}
}

// ---------------------------------------------------------------- the loop

func (a *app) run() string {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	limit := time.NewTicker(16 * time.Millisecond)
	defer limit.Stop()
	need := true
	for {
		select {
		case ev := <-a.t.Events():
			a.event(ev)
			need = true
		case <-a.dirty:
			need = true
		case r := <-a.ctl:
			if r.plan != nil {
				if err := a.openPlan(r.plan); err != nil {
					a.say("layout: %v", err)
				}
			} else {
				a.onControl(r.p, r.args)
			}
			if r.done != nil {
				close(r.done)
			}
			need = true
		case p := <-a.gone:
			if msg := a.removePane(p); msg != "" {
				return msg
			}
			need = true
		case msg := <-a.quit:
			return msg
		case <-tick.C:
			need = true // the clock, the notices
		case <-limit.C:
			if need {
				a.render()
				need = false
			}
		}
	}
}

func (a *app) paneGone(p *pane) {
	select {
	case a.gone <- p:
	case <-time.After(5 * time.Second):
	}
}

// removePane takes away a pane whose client ended; with none left the
// program ends, and says why.
func (a *app) removePane(p *pane) string {
	if a.root == nil || a.root.find(p) == nil {
		return ""
	}
	a.mu.Lock()
	a.root = remove(a.root, p)
	if a.focus == p {
		a.focus = nil
		if a.last != nil && a.root.find(a.last) != nil {
			a.focus = a.last
		} else if ps := a.root.leaves(); len(ps) > 0 {
			a.focus = ps[0]
		}
	}
	if a.last == p {
		a.last = nil
	}
	a.mu.Unlock()
	p.mu.Lock()
	why := p.lastLine
	p.mu.Unlock()
	if a.root == nil {
		if a.detaching {
			return "[detached]"
		}
		if why == "" {
			why = "[the last pane ended]"
		}
		return why
	}
	a.zoomed = a.zoomed && len(a.panes()) > 1
	a.layout()
	if why != "" && !a.detaching {
		a.say("pane %d: %s", p.id, why)
	}
	return ""
}

func (a *app) event(ev uv.Event) {
	switch e := ev.(type) {
	case uv.WindowSizeEvent:
		a.w, a.h = e.Width, e.Height
		_ = a.scr.Resize(a.w, a.h)
		a.layout()
	case uv.KeyPressEvent:
		a.keyPress(e)
	case uv.PasteEvent:
		if a.overlay != nil || a.focus == nil {
			return
		}
		for _, p := range a.targets() {
			p.mu.Lock()
			p.em.Paste(e.Content)
			p.mu.Unlock()
		}
	case uv.MouseClickEvent:
		a.mouseEvent(uv.Mouse(e), "click")
	case uv.MouseReleaseEvent:
		a.mouseEvent(uv.Mouse(e), "release")
	case uv.MouseMotionEvent:
		a.mouseEvent(uv.Mouse(e), "motion")
	case uv.MouseWheelEvent:
		a.mouseEvent(uv.Mouse(e), "wheel")
	}
}

// targets are the panes that get what is typed: the focused one, or all of
// them with the synchronized input.
func (a *app) targets() []*pane {
	if a.sync {
		return a.panes()
	}
	if a.focus == nil {
		return nil
	}
	return []*pane{a.focus}
}

// ---------------------------------------------------------------- keys

func (a *app) isPrefix(k uv.KeyPressEvent) bool {
	pk := a.prefix
	var code rune
	switch {
	case pk >= 1 && pk <= 26:
		code = rune('a' + pk - 1)
	case pk >= 0x1b && pk <= 0x1f:
		code = rune("[\\]^_"[pk-0x1b])
	default:
		return false
	}
	return k.Mod == uv.ModCtrl && (k.Code == code || k.Code == rune(pk))
}

func (a *app) keyPress(k uv.KeyPressEvent) {
	if a.overlay != nil {
		if a.overlay.key(a, k) {
			a.overlay = nil
		}
		return
	}
	if a.focus != nil && a.focus.copy != nil && !a.pending && !a.isPrefix(k) {
		a.focus.copyKey(k)
		return
	}
	if a.pending {
		a.pending = false
		a.command(k)
		return
	}
	if a.isPrefix(k) {
		a.pending = true
		return
	}
	for _, p := range a.targets() {
		p.mu.Lock()
		p.em.SendKey(k)
		p.mu.Unlock()
	}
}

// command runs what follows the prefix. The keys of the panes are taken
// here; the others (s ( ) L c $ f m M w h and the prefix itself) go to the
// ptscry client of the focused pane, which knows them.
func (a *app) command(k uv.KeyPressEvent) {
	p := a.focus
	switch {
	case k.Code == uv.KeyLeft && k.Mod == 0:
		a.move(-1, 0)
	case k.Code == uv.KeyRight && k.Mod == 0:
		a.move(1, 0)
	case k.Code == uv.KeyUp && k.Mod == 0:
		a.move(0, -1)
	case k.Code == uv.KeyDown && k.Mod == 0:
		a.move(0, 1)
	case k.Code == uv.KeyLeft && k.Mod != 0:
		a.grow(-1, 0, k.Mod)
	case k.Code == uv.KeyRight && k.Mod != 0:
		a.grow(1, 0, k.Mod)
	case k.Code == uv.KeyUp && k.Mod != 0:
		a.grow(0, -1, k.Mod)
	case k.Code == uv.KeyDown && k.Mod != 0:
		a.grow(0, 1, k.Mod)
	case k.Code == uv.KeySpace:
		a.nextPreset()
	default:
		switch k.Text {
		case "%":
			a.split(true, "")
		case "\"":
			a.split(false, "")
		case "o":
			a.cycle(1)
		case ";":
			if a.last != nil && a.root.find(a.last) != nil {
				a.focus, a.last = a.last, a.focus
			}
		case "x":
			if p != nil {
				p.close() // only a detach: the session goes on
			}
		case "z":
			if len(a.panes()) > 1 {
				a.zoomed = !a.zoomed
				a.layout()
			}
		case "S":
			a.toggleSync()
		case "[":
			if p != nil {
				p.enterCopy()
			}
		case "/":
			if p != nil {
				p.enterCopy()
				a.overlay = &question{label: "search: ", done: func(s string) { p.search(s, true) }}
			}
		case "d":
			a.detachAll()
		case "W":
			a.overlay = &question{label: "save the layout as: ", text: a.focusSession(), done: a.saveLayout}
		case "O":
			a.overlay = &question{label: "open the layout: ", done: func(s string) { go a.loadLayout(s) }}
		case "?", "h":
			a.overlay = &help{prefix: a.prefix}
		default:
			if p != nil && !a.isPrefixLiteral(k) {
				if b := keyBytes(k); b != nil {
					p.write(append([]byte{a.prefix}, b...))
				}
				return
			}
			if p != nil { // prefix twice: the prefix itself, for the program
				p.write([]byte{a.prefix, a.prefix})
			}
		}
	}
	a.redraw()
}

func (a *app) isPrefixLiteral(k uv.KeyPressEvent) bool { return a.isPrefix(k) }

// keyBytes is what a simple key after the prefix is on the wire.
func keyBytes(k uv.KeyPressEvent) []byte {
	if k.Text != "" && k.Mod&(uv.ModCtrl|uv.ModAlt) == 0 {
		return []byte(k.Text)
	}
	return nil
}

func (a *app) focusSession() string {
	if a.focus == nil {
		return ""
	}
	return a.focus.session()
}

// ---------------------------------------------------------------- panes

func (a *app) split(side bool, session string) {
	if a.focus == nil {
		return
	}
	if len(a.panes()) >= maxPanes {
		a.say("at most %d panes", maxPanes)
		return
	}
	n := a.root.find(a.focus)
	if n == nil {
		return
	}
	if session == "" {
		session = a.freeName()
	}
	cols, rows := n.rect.Dx()/2, n.rect.Dy()/2
	p, err := a.newPane(session, max(rows, 2), max(cols, 2))
	if err != nil {
		a.say("new pane: %v", err)
		return
	}
	a.mu.Lock()
	n.split(p, side)
	a.root.fix(nil)
	a.last, a.focus = a.focus, p
	a.zoomed = false
	a.mu.Unlock()
	a.layout()
}

// freeName is a session name not used by the panes. The server is not
// asked (a split must not wait on it): the name carries a random part, so
// that it does not fall on an old session ("new -A" would attach to it).
func (a *app) freeName() string {
	used := map[string]bool{}
	for _, p := range a.panes() {
		used[p.session()] = true
	}
	for {
		var r [2]byte
		_, _ = rand.Read(r[:])
		n := fmt.Sprintf("pane-%02x%02x", r[0], r[1])
		if !used[n] {
			return n
		}
	}
}

func (a *app) cycle(d int) {
	ps := a.panes()
	for i, p := range ps {
		if p == a.focus && len(ps) > 1 {
			a.last, a.focus = a.focus, ps[(i+d+len(ps))%len(ps)]
			if a.zoomed {
				a.layout()
			}
			return
		}
	}
}

// move gives the keyboard to the pane next to the focused one.
func (a *app) move(dx, dy int) {
	if a.focus == nil || a.zoomed {
		return
	}
	cur := a.root.find(a.focus)
	if cur == nil {
		return
	}
	c := cur.rect
	var best *pane
	bestD := 1 << 30
	for _, p := range a.panes() {
		n := a.root.find(p)
		if n == nil || p == a.focus {
			continue
		}
		r := n.rect
		ok := false
		switch {
		case dx > 0:
			ok = r.Min.X >= c.Max.X && r.Min.Y < c.Max.Y && r.Max.Y > c.Min.Y
		case dx < 0:
			ok = r.Max.X <= c.Min.X && r.Min.Y < c.Max.Y && r.Max.Y > c.Min.Y
		case dy > 0:
			ok = r.Min.Y >= c.Max.Y && r.Min.X < c.Max.X && r.Max.X > c.Min.X
		case dy < 0:
			ok = r.Max.Y <= c.Min.Y && r.Min.X < c.Max.X && r.Max.X > c.Min.X
		}
		if !ok {
			continue
		}
		ddx := (r.Min.X + r.Max.X - c.Min.X - c.Max.X) / 2
		ddy := (r.Min.Y + r.Max.Y - c.Min.Y - c.Max.Y) / 2
		if d := ddx*ddx + ddy*ddy; d < bestD {
			best, bestD = p, d
		}
	}
	if best != nil {
		a.last, a.focus = a.focus, best
	}
}

// grow moves the nearest divider: 5 cells with Alt, 1 with Ctrl.
func (a *app) grow(dx, dy int, mod uv.KeyMod) {
	if a.focus == nil {
		return
	}
	cells := 5
	if mod&uv.ModCtrl != 0 {
		cells = 1
	}
	n := a.root.find(a.focus)
	for n != nil && n.parent != nil {
		par := n.parent
		if par.side == (dx != 0) {
			size := par.rect.Dx()
			if !par.side {
				size = par.rect.Dy()
			}
			if size > 2 {
				step := float32(cells) / float32(size)
				if dx < 0 || dy < 0 {
					step = -step
				}
				par.ratio = min(max(par.ratio+step, 0.05), 0.95)
				a.layout()
			}
			return
		}
		n = par
	}
}

func (a *app) nextPreset() {
	ps := a.panes()
	if len(ps) < 2 {
		return
	}
	k := a.preset % len(presetNames)
	a.preset = k + 1
	a.mu.Lock()
	a.root = preset(ps, k)
	a.root.fix(nil)
	a.zoomed = false
	a.mu.Unlock()
	a.layout()
	a.say("layout: %s", presetNames[k])
}

func (a *app) toggleSync() {
	if len(a.panes()) < 2 {
		a.say("the synchronized input needs more than one pane")
		return
	}
	a.sync = !a.sync
	if a.sync {
		a.say("SYNCHRONIZED INPUT: what you type goes to all %d panes (prefix S to stop)", len(a.panes()))
	} else {
		a.say("synchronized input off")
	}
}

func (a *app) detachAll() {
	a.detaching = true
	for _, p := range a.panes() {
		p.close()
	}
	go func() {
		time.Sleep(3 * time.Second)
		select {
		case a.quit <- "[detached]":
		default:
		}
	}()
}

// ---------------------------------------------------------------- layouts

func (a *app) saveLayout(name string) {
	if !nameRe.MatchString(name) || name[0] == '.' {
		a.say("invalid name: letters, digits, . _ -")
		return
	}
	b, err := encodeLayout(a.root, a.focus)
	if err != nil {
		a.say("%v", err)
		return
	}
	go func() {
		if _, err := a.be.run([]string{"layout", "save", name}, b); err != nil {
			a.say("layout %s: %v", name, err)
			return
		}
		a.say("layout %s saved on %s", name, a.be.where())
	}()
}

// loadLayout replaces the panes with a layout from the server (the old
// panes are only detached).
func (a *app) loadLayout(name string) {
	if !nameRe.MatchString(name) {
		a.say("invalid name")
		return
	}
	out, err := a.be.run([]string{"layout", "show", name}, nil)
	if err != nil {
		a.say("layout %s: %v", name, err)
		return
	}
	pl, err := parseLayout([]byte(out))
	if err != nil {
		a.say("layout %s: %v", name, err)
		return
	}
	done := make(chan struct{})
	a.ctl <- ctlReq{plan: pl, done: done}
	<-done
}
