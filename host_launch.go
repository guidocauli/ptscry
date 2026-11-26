//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// host_launch.go: the host's side of session launching. The host asks the
// monitor to start a session's process and gets back the terminal master and
// the pid; the exit status comes later as a message (the process is not the
// host's child any more: it belongs to a root launcher).

package main

import (
	"encoding/json"
	"errors"
	"os"
	"time"
)

type spawnResult struct {
	reply  spawnReply
	master *os.File
}

// useLauncher reports whether sessions are started through the monitor.
func (h *host) useLauncher() bool {
	return h.ctl != nil && h.cfg.PAMService != "off"
}

// launch asks the monitor to start the session process. It returns the
// terminal master, the pid, and the channel that will carry the exit status.
func (h *host) launch(name string, argv []string, env map[string]string, rows, cols uint16) (*os.File, int, chan int, error) {
	res := make(chan spawnResult, 1)
	exit := make(chan int, 1)
	h.launchMu.Lock()
	h.spawns[name] = res
	h.exits[name] = exit
	h.launchMu.Unlock()
	cleanup := func() {
		h.launchMu.Lock()
		delete(h.spawns, name)
		delete(h.exits, name)
		h.launchMu.Unlock()
	}
	b, _ := json.Marshal(launchRequest{Session: name, Argv: argv, Env: env, Rows: rows, Cols: cols})
	if err := h.ctl.send(ctlSpawn, b); err != nil {
		cleanup()
		return nil, 0, nil, err
	}
	select {
	case r := <-res:
		h.launchMu.Lock()
		delete(h.spawns, name)
		h.launchMu.Unlock()
		if !r.reply.OK || r.master == nil {
			cleanup()
			if r.master != nil {
				r.master.Close()
			}
			msg := r.reply.Error
			if msg == "" {
				msg = "the monitor could not start the session"
			}
			return nil, 0, nil, errors.New(msg)
		}
		return r.master, r.reply.PID, exit, nil
	case <-time.After(launchTimeout + 10*time.Second):
		cleanup()
		return nil, 0, nil, errors.New("the monitor did not answer in time")
	}
}

func (h *host) onSpawned(p []byte, f *os.File) {
	var r spawnReply
	if json.Unmarshal(p, &r) != nil {
		if f != nil {
			f.Close()
		}
		return
	}
	h.launchMu.Lock()
	ch := h.spawns[r.Session]
	h.launchMu.Unlock()
	if ch == nil { // nobody is waiting (timed out): do not keep the terminal
		if f != nil {
			f.Close()
		}
		return
	}
	ch <- spawnResult{r, f}
}

func (h *host) onExited(p []byte) {
	var e exitedMsg
	if json.Unmarshal(p, &e) != nil {
		return
	}
	h.launchMu.Lock()
	ch := h.exits[e.Session]
	h.launchMu.Unlock()
	if ch != nil {
		select {
		case ch <- e.Code:
		default:
		}
	}
}

func (h *host) forgetLaunch(name string) {
	h.launchMu.Lock()
	delete(h.spawns, name)
	delete(h.exits, name)
	h.launchMu.Unlock()
}
