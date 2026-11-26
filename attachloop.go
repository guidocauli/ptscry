//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// attachloop.go: an attached terminal, with the session commands of the
// prefix (choose, previous / next / last, new, rename, freeze, watch). The
// same code serves the local client (stdin/stdout) and the SSH front-end
// (the channel): every command is an ordinary request on behalf of the user
// (dial), so it is checked and audited like any other.

package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// termSide is the user's terminal and the way to reach the server.
type termSide struct {
	keys     <-chan []byte    // keyboard; closed when the user's side is gone
	out      io.Writer        // the user's terminal, in raw mode
	resize   <-chan [2]uint16 // may be nil
	size     func() (rows, cols uint16)
	gone     <-chan struct{} // the user's side went away (front-end); may be nil
	dial     func(hl hello) (*framed, okReply, error)
	prefix   byte
	noPrefix bool
	readOnly bool
	env      map[string]string
}

type loopResult struct {
	kind string // exit | bye | lost | gone
	code int
	msg  string
	name string // the session attached last
}

type frame struct {
	t byte
	p []byte
}

// hostConn reads the frames of one attachment.
type hostConn struct {
	fc     *framed
	frames chan frame
	done   chan struct{} // the loop no longer reads: the reader must not wait for it
	once   sync.Once
}

func newHostConn(fc *framed) *hostConn {
	hc := &hostConn{fc: fc, frames: make(chan frame, 16), done: make(chan struct{})}
	go func() {
		defer close(hc.frames)
		for {
			t, p, err := fc.read()
			if err != nil {
				return
			}
			select {
			case hc.frames <- frame{t, p}:
			case <-hc.done:
				return
			}
			if t == msgExit || t == msgBye {
				return
			}
		}
	}()
	return hc
}

// stop closes the connection and lets the reader go.
func (hc *hostConn) stop() {
	hc.once.Do(func() { close(hc.done) })
	hc.fc.c.Close()
}

// detach asks the host to let go and waits (a little) for it to agree.
func (hc *hostConn) detach() {
	defer hc.stop()
	_ = hc.fc.write(msgDetach, nil)
	timeout := time.After(2 * time.Second)
	for {
		select {
		case f, ok := <-hc.frames:
			if !ok || f.t == msgBye || f.t == msgExit {
				hc.fc.c.Close()
				return
			}
		case <-timeout:
			hc.fc.c.Close()
			return
		}
	}
}

// overlay is a screen of the client drawn over the session (the list of
// sessions, a question); feed returns true when it is finished.
type overlay interface {
	draw(rows, cols int) string
	feed(k []byte) (done bool, rest []byte)
}

// maxHeld is how much output of the session is held while an overlay is
// shown; beyond that it is dropped and the program is asked to repaint.
const maxHeld = 512 * 1024

type attachLoop struct {
	ts       *termSide
	st       inState
	hc       *hostConn
	cur      string
	prev     string
	alt      bool // the session is on the alternate screen (tracked from its output)
	watch    byte
	ov       overlay
	ovDone   func() *loopResult
	held     []byte
	overflow bool
	redrawAt <-chan time.Time
	byeAt    <-chan time.Time // detach asked: give the host this long to answer
	rows     uint16
	cols     uint16
}

func runAttachLoop(ts *termSide, fc *framed, r okReply) loopResult {
	l := &attachLoop{ts: ts, hc: newHostConn(fc)}
	if r.Session != nil {
		l.cur = r.Session.Name
	}
	l.rows, l.cols = ts.size()
	l.st = inState{prefix: ts.prefix, off: ts.noPrefix, help: func(s string) { l.write(s) }}
	return l.run()
}

func (l *attachLoop) write(s string) { _, _ = io.WriteString(l.ts.out, s) }

// say shows a short message of the client on the terminal, like a notice.
func (l *attachLoop) say(text string) {
	var flags byte
	if l.alt {
		flags = noticeAlt
	}
	out, redraw := renderNotice(append([]byte{flags}, text...))
	_, _ = l.ts.out.Write(out)
	if redraw {
		l.redrawAt = time.After(noticeShown)
	}
}

func (l *attachLoop) run() loopResult {
	for {
		select {
		case f, ok := <-l.hc.frames:
			if !ok {
				l.closeOverlay(false)
				l.hc.stop()
				return loopResult{kind: "lost", name: l.cur}
			}
			switch f.t {
			case msgData:
				l.alt = altScreenState(f.p, l.alt)
				if l.ov != nil {
					if len(l.held)+len(f.p) > maxHeld {
						l.held, l.overflow = nil, true
					} else if !l.overflow {
						l.held = append(l.held, f.p...)
					}
					continue
				}
				_, _ = l.ts.out.Write(f.p)
			case msgName:
				if n := string(f.p); nameRe.MatchString(n) {
					if l.prev == n {
						l.prev = ""
					}
					if l.cur != n {
						l.cur = n
						l.announce()
					}
				}
			case msgNotice:
				if l.ov != nil {
					continue
				}
				if out, redraw := renderNotice(f.p); len(out) > 0 {
					_, _ = l.ts.out.Write(out)
					if redraw {
						l.redrawAt = time.After(noticeShown)
					}
				}
			case msgExit:
				l.closeOverlay(false)
				code := 0
				if len(f.p) == 4 {
					code = int(int32(uint32(f.p[0])<<24 | uint32(f.p[1])<<16 | uint32(f.p[2])<<8 | uint32(f.p[3])))
				}
				l.hc.stop()
				return loopResult{kind: "exit", code: code, name: l.cur}
			case msgBye:
				l.closeOverlay(false)
				l.hc.stop()
				return loopResult{kind: "bye", msg: string(f.p), name: l.cur}
			}

		case k, ok := <-l.ts.keys:
			if !ok {
				l.hc.detach()
				return loopResult{kind: "gone", name: l.cur}
			}
			for len(k) > 0 {
				if l.ov != nil {
					done, rest := l.ov.feed(k)
					if !done {
						l.write(l.ov.draw(int(l.rows), int(l.cols)))
						break
					}
					if res := l.closeOverlay(true); res != nil {
						return *res
					}
					k = rest // what was typed after Enter goes to the session
					continue
				}
				out, act, rest := l.st.processAct(k)
				if len(out) > 0 && l.hc.fc.write(msgData, out) != nil {
					return loopResult{kind: "lost", name: l.cur}
				}
				if act == actNone {
					break
				}
				if act == actDetach {
					_ = l.hc.fc.write(msgDetach, nil)
					l.byeAt = time.After(2 * time.Second)
					break
				}
				if res := l.command(act); res != nil {
					return *res
				}
				k = rest
			}

		case sz := <-l.ts.resize:
			l.rows, l.cols = sz[0], sz[1]
			_ = l.hc.fc.write(msgResize, encodeResize(sz[0], sz[1]))
			if l.ov != nil {
				l.write(l.ov.draw(int(l.rows), int(l.cols)))
			}

		case <-l.ts.gone:
			l.hc.detach()
			return loopResult{kind: "gone", name: l.cur}

		case <-l.byeAt:
			l.hc.stop()
			return loopResult{kind: "bye", msg: "detached", name: l.cur}

		case <-l.redrawAt:
			l.redrawAt = nil
			_ = l.hc.fc.write(msgRedraw, nil)
		}
	}
}

// openOverlay hides the session behind a screen of the client.
func (l *attachLoop) openOverlay(ov overlay, done func() *loopResult) {
	l.ov, l.ovDone, l.held, l.overflow = ov, done, nil, false
	if !l.alt {
		l.write("\x1b[?1049h") // the normal screen comes back as it was
	}
	l.write(ov.draw(int(l.rows), int(l.cols)))
}

// closeOverlay gives the screen back to the session, then (act) does what
// the overlay decided.
func (l *attachLoop) closeOverlay(act bool) *loopResult {
	if l.ov == nil {
		return nil
	}
	done := l.ovDone
	l.ov, l.ovDone = nil, nil
	l.write("\x1b[0m\x1b[?25h")
	if !l.alt {
		l.write("\x1b[?1049l")
	} else {
		l.write("\x1b[H\x1b[2J")
	}
	held, overflow := l.held, l.overflow
	l.held, l.overflow = nil, false
	if !overflow {
		_, _ = l.ts.out.Write(held)
	}
	if l.alt || overflow {
		_ = l.hc.fc.write(msgRedraw, nil)
	}
	if act && done != nil {
		return done()
	}
	return nil
}

// ask runs a request on behalf of the user and closes its connection.
func (l *attachLoop) ask(hl hello) (okReply, error) {
	fc, r, err := l.ts.dial(hl)
	if err != nil {
		return r, err
	}
	fc.c.Close()
	return r, nil
}

func (l *attachLoop) sessions() ([]sessionInfo, error) {
	r, err := l.ask(hello{Op: "list"})
	if err != nil {
		return nil, err
	}
	sort.Slice(r.Sessions, func(i, j int) bool { return r.Sessions[i].Name < r.Sessions[j].Name })
	return r.Sessions, nil
}

// command runs a prefix command; a result ends the attachment.
func (l *attachLoop) command(act keyAct) *loopResult {
	if l.ts.readOnly {
		switch act {
		case actNew, actRename, actFreeze, actTake:
			l.say("read-only: only moving between sessions is possible here")
			return nil
		}
	}
	switch act {
	case actChoose:
		ss, err := l.sessions()
		if err != nil {
			l.say("cannot list the sessions: " + err.Error())
			return nil
		}
		ch := newChooser(ss, l.cur)
		l.openOverlay(ch, func() *loopResult {
			if ch.picked != "" && ch.picked != l.cur {
				return l.switchTo(hello{Op: "attach", Name: ch.picked})
			}
			return nil
		})
		return nil

	case actPrev, actNext:
		ss, err := l.sessions()
		if err != nil {
			l.say("cannot list the sessions: " + err.Error())
			return nil
		}
		if len(ss) < 2 {
			l.say("there is no other session")
			return nil
		}
		i := 0
		for k, s := range ss {
			if s.Name == l.cur {
				i = k
			}
		}
		if act == actNext {
			i = (i + 1) % len(ss)
		} else {
			i = (i - 1 + len(ss)) % len(ss)
		}
		return l.switchTo(hello{Op: "attach", Name: ss[i].Name})

	case actLast:
		if l.prev == "" {
			l.say("no session was attached before this one")
			return nil
		}
		return l.switchTo(hello{Op: "attach", Name: l.prev})

	case actNew:
		return l.switchTo(hello{Op: "new", Env: l.ts.env})

	case actRename:
		pr := newPrompt("New name for session "+l.cur+": ", l.cur)
		l.openOverlay(pr, func() *loopResult {
			switch {
			case !pr.ok || pr.text == l.cur:
			case !nameRe.MatchString(pr.text):
				l.say("invalid name: letters, digits, . _ - (max 64)")
			default:
				if _, err := l.ask(hello{Op: "rename", Name: l.cur, NewName: pr.text}); err != nil {
					l.say("rename: " + err.Error())
					break
				}
				if l.prev == pr.text {
					l.prev = ""
				}
				l.cur = pr.text
				l.announce()
				l.say("session renamed " + pr.text)
			}
			return nil
		})
		return nil

	case actFreeze:
		ss, err := l.sessions()
		if err != nil {
			l.say("cannot list the sessions: " + err.Error())
			return nil
		}
		op := "freeze"
		for _, s := range ss {
			if s.Name == l.cur && s.Frozen {
				op = "thaw"
			}
		}
		if _, err := l.ask(hello{Op: op, Name: l.cur}); err != nil {
			l.say(op + ": " + err.Error())
			return nil
		}
		if op == "freeze" {
			l.say("session frozen: its processes are stopped (prefix f again to thaw)")
		} else {
			l.say("session thawed")
		}
		return nil

	case actTake:
		if l.ts.readOnly {
			l.say("read-only: this client cannot type")
			return nil
		}
		_ = l.hc.fc.write(msgTake, []byte{2}) // the host knows who holds it: take or give back
		return nil

	case actWatchAct, actWatchSil:
		bit := watchActivity
		what := "activity"
		if act == actWatchSil {
			bit, what = watchSilence, "30 s of silence"
		}
		l.watch ^= bit
		l.sendWatch()
		if l.watch&bit != 0 {
			l.say("you will be told about " + what + " in the other sessions")
		} else {
			l.say("no more alerts about " + what)
		}
		return nil
	}
	return nil
}

func (l *attachLoop) sendWatch() {
	sec := uint16(defaultSilence / time.Second)
	_ = l.hc.fc.write(msgWatch, []byte{l.watch, byte(sec >> 8), byte(sec)})
}

// switchTo leaves the current session for another one (attach or new). If
// that fails, it goes back to the current one.
func (l *attachLoop) switchTo(hl hello) *loopResult {
	if hl.Op == "attach" && hl.Name == l.cur {
		return nil
	}
	l.closeOverlay(false)
	l.hc.detach()
	wasAlt := l.alt
	l.alt = false
	if wasAlt {
		l.write("\x1b[?1049l")
	}
	l.write("\x1b[0m\x1b[H\x1b[2J")
	l.rows, l.cols = l.ts.size()
	hl.Rows, hl.Cols, hl.ReadOnly = l.rows, l.cols, l.ts.readOnly && hl.Op == "attach"
	if hl.Op == "attach" {
		hl.Env = l.ts.env
	}
	fc, r, err := l.ts.dial(hl)
	if err != nil {
		msg := err.Error()
		back := hello{Op: "attach", Name: l.cur, Rows: l.rows, Cols: l.cols, Env: l.ts.env, ReadOnly: l.ts.readOnly}
		if fc, r, err = l.ts.dial(back); err != nil {
			return &loopResult{kind: "lost", msg: msg, name: l.cur}
		}
		l.hc = newHostConn(fc)
		l.restoreWatch()
		l.say("cannot switch: " + msg)
		return nil
	}
	l.hc = newHostConn(fc)
	if r.Session != nil && r.Session.Name != l.cur {
		l.prev, l.cur = l.cur, r.Session.Name
		l.announce()
	}
	l.restoreWatch()
	return nil
}

// announce tells the terminal (ptscry-gui, ptscry-tui) which session it now
// shows, so that it reattaches that one after a lost connection and saves it
// in a layout. Other terminals ignore the sequence.
func (l *attachLoop) announce() {
	if nameRe.MatchString(l.cur) {
		l.write("\x1b]7799;session;" + l.cur + "\x07")
	}
}

func (l *attachLoop) restoreWatch() {
	if l.watch != 0 {
		l.sendWatch()
	}
}

// ---------------------------------------------------------------- chooser

type chooser struct {
	all    []sessionInfo
	cur    string
	filter string
	sel    int
	picked string
	done   bool
}

func newChooser(ss []sessionInfo, cur string) *chooser {
	c := &chooser{all: ss, cur: cur}
	for i, s := range c.shown() {
		if s.Name == cur {
			c.sel = i
		}
	}
	return c
}

func (c *chooser) shown() []sessionInfo {
	f := strings.ToLower(c.filter)
	var out []sessionInfo
	for _, s := range c.all {
		if f == "" || strings.Contains(strings.ToLower(s.Name+" "+s.Cmd+" "+s.Cwd), f) {
			out = append(out, s)
		}
	}
	return out
}

func (c *chooser) feed(k []byte) (bool, []byte) {
	for len(k) > 0 {
		switch {
		case len(k) == 1 && k[0] == 0x1b, k[0] == 0x03, k[0] == 0x07: // Esc alone, Ctrl-C, Ctrl-G
			return true, k[1:]
		case k[0] == '\r' || k[0] == '\n':
			if sh := c.shown(); c.sel < len(sh) {
				c.picked = sh[c.sel].Name
			}
			return true, k[1:]
		case hasPrefix(k, "\x1b[A", "\x1bOA") || k[0] == 0x10: // up, Ctrl-P
			if c.sel > 0 {
				c.sel--
			}
		case hasPrefix(k, "\x1b[B", "\x1bOB") || k[0] == 0x0e: // down, Ctrl-N
			if c.sel < len(c.shown())-1 {
				c.sel++
			}
		case k[0] == 0x7f || k[0] == 0x08:
			if c.filter != "" {
				_, n := utf8.DecodeLastRuneInString(c.filter)
				c.filter = c.filter[:len(c.filter)-n]
				c.sel = 0
			}
		case k[0] == 0x1b: // another escape sequence: skip it
			k = skipEscape(k)
			continue
		case k[0] >= 0x20:
			r, n := utf8.DecodeRune(k)
			if r != utf8.RuneError && len(c.filter) < 64 {
				c.filter += string(r)
				c.sel = 0
			}
			k = k[n:]
			continue
		}
		if hasPrefix(k, "\x1b[A", "\x1bOA", "\x1b[B", "\x1bOB") {
			k = k[3:]
		} else {
			k = k[1:]
		}
	}
	return false, nil
}

func (c *chooser) draw(rows, cols int) string {
	var b strings.Builder
	b.WriteString("\x1b[0m\x1b[H\x1b[2J\x1b[?25l")
	line := func(s string, rev bool) {
		s = fitWidth(s, cols)
		if rev {
			b.WriteString("\x1b[7m" + s + strings.Repeat(" ", max(0, cols-utf8.RuneCountInString(s))) + "\x1b[0m")
		} else {
			b.WriteString(s)
		}
		b.WriteString("\r\n")
	}
	line("ptscry - sessions   (type to filter, Up/Down, Enter: attach, Esc: back)", false)
	line("filter: "+c.filter+"_", false)
	line("", false)
	sh := c.shown()
	if c.sel >= len(sh) {
		c.sel = max(0, len(sh)-1)
	}
	room := max(1, rows-4)
	first := 0
	if c.sel >= room {
		first = c.sel - room + 1
	}
	if len(sh) == 0 {
		line("  (no session matches)", false)
	}
	for i := first; i < len(sh) && i < first+room; i++ {
		s := sh[i]
		mark := "  "
		if s.Name == c.cur {
			mark = "* "
		}
		var tags []string
		if s.Attached > 0 {
			tags = append(tags, fmt.Sprintf("%d attached", s.Attached))
		}
		if s.Frozen {
			tags = append(tags, "frozen")
		}
		if !s.Last.IsZero() {
			tags = append(tags, "idle "+shortDur(time.Since(s.Last)))
		}
		what := cleanNotice(s.Cmd)
		if s.Cwd != "" {
			what = cleanNotice(s.Cwd)
		}
		line(fmt.Sprintf("%s%-20s %-28s %s", mark, s.Name, "["+strings.Join(tags, ", ")+"]", what), i == c.sel)
	}
	return b.String()
}

// ---------------------------------------------------------------- prompt

type prompt struct {
	label string
	text  string
	ok    bool
}

func newPrompt(label, text string) *prompt { return &prompt{label: label, text: text} }

func (p *prompt) feed(k []byte) (bool, []byte) {
	for len(k) > 0 {
		switch {
		case len(k) == 1 && k[0] == 0x1b, k[0] == 0x03, k[0] == 0x07:
			return true, k[1:]
		case k[0] == '\r' || k[0] == '\n':
			p.ok = true
			return true, k[1:]
		case k[0] == 0x7f || k[0] == 0x08:
			if p.text != "" {
				p.text = p.text[:len(p.text)-1]
			}
		case k[0] == 0x15: // Ctrl-U
			p.text = ""
		case k[0] == 0x1b:
			k = skipEscape(k)
			continue
		case k[0] >= 0x20 && k[0] < 0x7f && len(p.text) < 64:
			p.text += string(k[0])
		}
		k = k[1:]
	}
	return false, nil
}

func (p *prompt) draw(rows, cols int) string {
	return "\x1b[0m\x1b[H\x1b[2J\x1b[?25h" + fitWidth(p.label+p.text, cols) +
		"\r\n\r\n" + fitWidth("(Enter: confirm, Esc: cancel)", cols) +
		fmt.Sprintf("\x1b[1;%dH", min(cols, utf8.RuneCountInString(p.label+p.text)+1))
}

// ---------------------------------------------------------------- helpers

func hasPrefix(k []byte, ps ...string) bool {
	for _, p := range ps {
		if strings.HasPrefix(string(k), p) {
			return true
		}
	}
	return false
}

// skipEscape drops one escape sequence (CSI, SS3 or a lone ESC + byte).
func skipEscape(k []byte) []byte {
	if len(k) < 2 {
		return nil
	}
	switch k[1] {
	case '[':
		for i := 2; i < len(k); i++ {
			if k[i] >= 0x40 && k[i] <= 0x7e {
				return k[i+1:]
			}
		}
		return nil
	case 'O':
		if len(k) >= 3 {
			return k[3:]
		}
		return nil
	}
	return k[2:]
}

// fitWidth cuts s to the width of the terminal (one column per rune: the
// names are ASCII, the rest is only a hint).
func fitWidth(s string, cols int) string {
	if cols <= 0 || utf8.RuneCountInString(s) <= cols {
		return s
	}
	r := []rune(s)
	return string(r[:cols])
}

func shortDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
