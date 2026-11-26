//go:build !windows

// ptscry-tui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// pane.go: one pane: a terminal emulator fed by the ptscry client of its
// session.

package main

import (
	"image/color"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

type pane struct {
	app *app
	id  int

	mu       sync.Mutex // em, name, modes
	em       *vt.Emulator
	name     string
	title    string
	modes    map[ansi.Mode]bool
	io       *paneIO
	liveAt   time.Time
	lastCtl  time.Time
	prefixAt time.Time // the last prefix command passed to the client of the pane
	cols     int
	rows     int
	lastOut  time.Time
	copy     *copyMode
	done     chan struct{}
	out      chan []byte // to the client (keys, answers of the emulator)
	finished bool
	lastLine string // the last line it printed, shown when it goes away
}

func (a *app) newPane(session string, rows, cols int) (*pane, error) {
	pio, err := a.be.open(session, rows, cols)
	if err != nil {
		return nil, err
	}
	a.nextID++
	p := &pane{app: a, id: a.nextID, name: session, io: pio, modes: map[ansi.Mode]bool{}, cols: cols, rows: rows,
		liveAt: time.Now(), done: make(chan struct{}), out: make(chan []byte, 256)}
	p.em = vt.NewEmulator(max(cols, 1), max(rows, 1))
	p.em.SetScrollbackSize(5000)
	p.em.SetCallbacks(vt.Callbacks{
		Title:       func(t string) { p.title = t },
		EnableMode:  func(m ansi.Mode) { p.modes[m] = true },
		DisableMode: func(m ansi.Mode) { delete(p.modes, m) },
		Bell:        func() { a.bell() },
	})
	p.em.RegisterOscHandler(ctlOSC, func(data []byte) bool {
		if args := parseControl(data); args != nil {
			a.control(p, args) // queued: we hold p.mu here
		}
		return true
	})
	// what the emulator answers (and the keys it encodes) goes to the
	// client, through a queue: the emulator waits for its answers to be
	// read, and it runs with p.mu held
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := p.em.Read(buf)
			if n > 0 {
				p.send(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		for {
			select {
			case b := <-p.out:
				if _, err := p.io.in.Write(b); err != nil {
					return
				}
			case <-p.done:
				return
			}
		}
	}()
	// what the client prints goes to the emulator
	go func() {
		defer close(p.done)
		buf := make([]byte, 32*1024)
		for {
			n, err := p.io.out.Read(buf)
			if n > 0 {
				p.mu.Lock()
				_, _ = p.em.Write(buf[:n])
				p.lastOut = time.Now()
				if i := strings.LastIndexByte(string(buf[:n]), '['); i >= 0 {
					if j := strings.IndexByte(string(buf[i:n]), ']'); j > 0 && j < 200 {
						p.lastLine = string(buf[i : i+j+1]) // "[detached (session x)]" and the like
					}
				}
				p.mu.Unlock()
				a.redraw()
			}
			if err != nil {
				p.mu.Lock()
				p.finished = true
				p.mu.Unlock()
				a.paneGone(p)
				return
			}
		}
	}()
	return p, nil
}

func (p *pane) session() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.name
}

// resize gives the pane a new size (the emulator and the session).
func (p *pane) resize(cols, rows int) {
	cols, rows = max(cols, 1), max(rows, 1)
	p.mu.Lock()
	changed := cols != p.cols || rows != p.rows
	p.cols, p.rows = cols, rows
	if changed {
		p.em.Resize(cols, rows)
	}
	p.mu.Unlock()
	if changed {
		p.io.resize(rows, cols)
	}
}

// mouseReporting tells whether the program asked for the mouse.
func (p *pane) mouseReporting() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.modes[ansi.ModeMouseX10] || p.modes[ansi.ModeMouseNormal] || p.modes[ansi.ModeMouseHighlight] ||
		p.modes[ansi.ModeMouseButtonEvent] || p.modes[ansi.ModeMouseAnyEvent]
}

func (p *pane) close() { p.io.close() }

// write sends bytes to the client of the pane as they are (the prefix and
// the keys after it: the ptscry client in the pane understands them).
func (p *pane) write(b []byte) {
	if len(b) > 0 && b[0] == p.app.prefix {
		p.mu.Lock()
		p.prefixAt = time.Now()
		p.mu.Unlock()
	}
	p.send(b)
}

// send queues bytes for the client; when the client does not read (the
// session is stuck), they are dropped rather than freezing the screen.
func (p *pane) send(b []byte) {
	select {
	case p.out <- append([]byte(nil), b...):
	default:
	}
}

var (
	focusColor  = color.RGBA{R: 0x5f, G: 0xd7, B: 0x87, A: 0xff}
	borderColor = color.RGBA{R: 0x58, G: 0x58, B: 0x58, A: 0xff}
	syncColor   = color.RGBA{R: 0xe0, G: 0x40, B: 0x40, A: 0xff}
	barBg       = color.RGBA{R: 0x26, G: 0x2a, B: 0x33, A: 0xff}
	barFg       = color.RGBA{R: 0xc8, G: 0xcc, B: 0xd4, A: 0xff}
	warnColor   = color.RGBA{R: 0xff, G: 0xb0, B: 0x40, A: 0xff}
)
