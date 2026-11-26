//go:build !windows

// ptscry-tui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// control.go: the requests of the programs in the panes ("ptscry split",
// "ptscry layout apply", and the ptscry client saying which session it shows),
// and opening a layout.

package main

import (
	"fmt"
	"strings"
	"time"
)

// ctlOSC: see ptscry-gui. Whatever a session prints can carry it, so it only
// does what a key could: a new session on the same server, a layout from the
// user's own server, never a command; at most one a second, never in the
// replay that comes right after a pane attaches.
const ctlOSC = 7799

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

// control is called by the emulator, with p.mu held: it only queues.
func (a *app) control(p *pane, args []string) {
	select {
	case a.ctl <- ctlReq{p: p, args: args}:
	default:
	}
}

func (a *app) onControl(p *pane, args []string) {
	if p == nil || len(args) == 0 || a.root == nil || a.root.find(p) == nil {
		return
	}
	now := time.Now()
	p.mu.Lock()
	live, last, typed := p.liveAt, p.lastCtl, p.prefixAt
	if now.Sub(last) < time.Second || now.Sub(live) < 3*time.Second {
		p.mu.Unlock()
		return
	}
	p.lastCtl = now
	p.mu.Unlock()
	if args[0] == "session" {
		// the ptscry client of the pane changed session: believed only
		// shortly after the person sent it a prefix command
		if len(args) == 2 && now.Sub(typed) < 2*time.Minute {
			p.mu.Lock()
			p.name = args[1]
			p.mu.Unlock()
		}
		return
	}
	// the rest only from the pane with the keyboard
	if p != a.focus {
		return
	}
	switch args[0] {
	case "split":
		a.split(len(args) < 2 || args[1] != "tb", "")
	case "layout":
		if len(args) == 2 && now.Sub(a.lastOSCLayout) >= 10*time.Second {
			a.lastOSCLayout = now
			name := args[1]
			go a.loadLayout(name)
		}
	}
}

// openPlan replaces the panes with those of a layout.
func (a *app) openPlan(pl *plan) error {
	r := a.area()
	var focus *pane
	var opened []*pane
	var build func(x *plan) (*node, error)
	build = func(x *plan) (*node, error) {
		if x.a != nil {
			na, err := build(x.a)
			if err != nil {
				return nil, err
			}
			nb, err := build(x.b)
			if err != nil {
				return nil, err
			}
			return &node{side: x.side, ratio: x.ratio, a: na, b: nb}, nil
		}
		p, err := a.newPane(x.session, max(r.Dy()/2, 2), max(r.Dx()/2, 2))
		if err != nil {
			return nil, fmt.Errorf("session %s: %v", x.session, err)
		}
		opened = append(opened, p)
		if x.focus {
			focus = p
		}
		return leaf(p), nil
	}
	root, err := build(pl)
	if err != nil {
		for _, p := range opened {
			p.close()
		}
		return err
	}
	root.fix(nil)
	old := a.panes()
	a.mu.Lock()
	a.root, a.focus, a.last, a.zoomed, a.sync = root, focus, nil, false, false
	if a.focus == nil {
		a.focus = root.leaves()[0]
	}
	a.mu.Unlock()
	for _, p := range old {
		p.close() // only detached
	}
	a.layout()
	return nil
}
