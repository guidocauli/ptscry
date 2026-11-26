// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// restore.go: working directory and start-up commands of the tabs. The server
// tells the directory of every session (list -j); the GUI saves it with the
// workspace and, if the session no longer exists when the tab is reopened (the
// server was restarted, the session was killed), types "cd" into the new one.
// A running program cannot be brought back: only where it was.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
	"time"

	"ptscry/internal/rclient"
)

type sessInfo struct {
	Name     string    `json:"name"`
	Cwd      string    `json:"cwd"`
	Attached int       `json:"attached"`
	Rows     int       `json:"rows"`
	Cols     int       `json:"cols"`
	Last     time.Time `json:"last_activity"`
	Cmd      string    `json:"cmd"`
	Frozen   bool      `json:"frozen"`
}

func parseSessions(b []byte) []sessInfo {
	var v struct {
		Sessions []sessInfo `json:"sessions"`
	}
	if json.Unmarshal(b, &v) != nil {
		return nil
	}
	return v.Sessions
}

// sessions lists the sessions of the profile. With connect false it does not
// open a connection (and so never asks for a password).
func (h *hub) sessions(connect bool) ([]sessInfo, error) {
	if h.p.Mode == "shell" {
		return nil, errors.New("il terminale locale non ha sessioni persistenti")
	}
	if h.p.Mode == "ssh" {
		return nil, errors.New("una connessione SSH semplice non ha sessioni ptscry")
	}
	if h.p.Mode == "standalone" {
		bin, err := ptscryBinary(h.p)
		if err != nil {
			return nil, err
		}
		conf, _, err := localConf()
		if err != nil {
			return nil, err
		}
		cmd := exec.Command(bin, "list", "-j")
		cmd.Env = append(cmdEnv(), "PTSCRY_STANDALONE=1", "PTSCRY_CONFIG="+conf)
		out, err := runTimeout(cmd, 6*time.Second)
		if err != nil {
			return nil, err
		}
		return parseSessions(out), nil
	}
	h.mu.Lock()
	cl := h.cl
	h.mu.Unlock()
	if cl == nil {
		if !connect {
			return nil, io.EOF
		}
		var err error
		if cl, err = h.client(); err != nil {
			return nil, err
		}
	}
	line, _, _, err := rclient.Cmdline([]string{"list", "-j"})
	if err != nil {
		return nil, err
	}
	type res struct {
		b   []byte
		err error
	}
	ch := make(chan res, 1)
	go func() {
		var buf bytes.Buffer
		_, err := rclient.Run(cl, line, &buf, io.Discard)
		ch <- res{buf.Bytes(), err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		return parseSessions(r.b), nil
	case <-time.After(8 * time.Second):
		return nil, io.ErrNoProgress
	}
}

// checkFresh tells whether the session of the tab does not exist yet (before
// its first attach) and remembers the directory it had if it does.
func (t *tab) checkFresh() bool {
	if t.ro {
		return false
	}
	list, err := t.h.sessions(true)
	if err != nil {
		return false // unknown: do not type anything
	}
	for _, s := range list {
		if s.Name == t.sessionName() {
			t.setCwd(s.Cwd)
			t.mu.Lock()
			t.frozen = s.Frozen
			t.mu.Unlock()
			return false
		}
	}
	return true
}

func (t *tab) setCwd(c string) {
	if c == "" {
		return
	}
	t.mu.Lock()
	t.cwd = c
	t.mu.Unlock()
}

func (t *tab) getCwd() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cwd
}

// shQuote quotes s for a POSIX shell.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// seed types the start-up lines into a session that has just been created: the
// directory it had, then the person's own commands (exports, aliases...). A
// leading space keeps them out of the history of shells that ignore it.
func (t *tab) seed(term *gterm, fresh bool) {
	if !fresh {
		return
	}
	var sb strings.Builder
	if c := t.getCwd(); c != "" && !strings.ContainsAny(c, "\x00\r\n") {
		sb.WriteString(" cd -- " + shQuote(c) + "\n")
	}
	sb.WriteString(exportLines(t.getEnv()))
	t.app.setMu.Lock()
	startup := t.app.set.Startup
	t.app.setMu.Unlock()
	for _, l := range strings.Split(startup, "\n") {
		if l = strings.TrimRight(l, "\r"); strings.TrimSpace(l) != "" {
			sb.WriteString(" " + l + "\n")
		}
	}
	if sb.Len() > 0 {
		_, _ = term.In.Write([]byte(sb.String()))
	}
}

// pollCwds refreshes the directory of every tab whose connection is open.
func (g *gui) pollCwds() {
	byHub := map[*hub][]*tab{}
	for _, t := range g.allTabs() {
		byHub[t.h] = append(byHub[t.h], t)
	}
	changed := false
	for h, ts := range byHub {
		list, err := h.sessions(false)
		if err != nil {
			continue
		}
		for _, t := range ts {
			for _, s := range list {
				if s.Name == t.sessionName() && s.Cwd != "" && s.Cwd != t.getCwd() {
					t.setCwd(s.Cwd)
					changed = true
				}
			}
		}
	}
	if changed {
		g.runOnUI(g.saveWorkspace) // the pane tree is the UI goroutine's
	}
}

// seedLater waits for the shell to show its prompt before typing, so that the
// lines are not echoed twice (by the terminal and then by the shell).
func (t *tab) seedLater(term *gterm, fresh bool) {
	if fresh {
		time.Sleep(400 * time.Millisecond)
	}
	t.seed(term, fresh)
}

// kill ends a session of the profile (its processes get the hangup).
func (h *hub) kill(name string) error { return h.simple("kill", name) }

// freeze stops (on) or resumes (off) every process of a session.
func (h *hub) freeze(name string, on bool) error {
	op := "thaw"
	if on {
		op = "freeze"
	}
	return h.simple(op, name)
}

// simple runs "ptscry <op> <name>" on the server of the profile (or on this computer).
func (h *hub) simple(op, name string) error {
	line, _, _, err := rclient.Cmdline([]string{op, name})
	if err != nil {
		return err
	}
	if h.p.Mode == "standalone" {
		bin, err := ptscryBinary(h.p)
		if err != nil {
			return err
		}
		conf, _, err := localConf()
		if err != nil {
			return err
		}
		cmd := exec.Command(bin, op, name)
		cmd.Env = append(cmdEnv(), "PTSCRY_STANDALONE=1", "PTSCRY_CONFIG="+conf)
		_, err = runTimeout(cmd, 8*time.Second)
		return err
	}
	cl, err := h.client()
	if err != nil {
		return err
	}
	var out bytes.Buffer
	res, err := rclient.Run(cl, line, &out, &out)
	if err != nil {
		return err
	}
	if res.Code != 0 {
		return errors.New(strings.TrimSpace(out.String()))
	}
	return nil
}

func (t *tab) getEnv() []envVar {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]envVar(nil), t.env...)
}

// exportLines is what is typed to give a shell these variables.
func exportLines(vars []envVar) string {
	var sb strings.Builder
	for _, v := range vars {
		sb.WriteString(" export " + v.Name + "=" + shQuote(v.Value) + "\n")
	}
	return sb.String()
}
