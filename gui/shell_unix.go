// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// shell_unix.go: the "terminale locale" profile on Linux and macOS: the user's
// own shell on a local pseudoterminal. Nothing persists: closing the tab ends it.

//go:build !windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"github.com/creack/pty"

	"ptscry/internal/rclient"
)

func openShell(p profile, rows, cols int) (*gterm, error) {
	line := p.Target
	if line == "" {
		line = loginShell()
	}
	if line == "" {
		line = "/bin/sh"
	}
	words, err := splitCommand(line)
	if err != nil {
		return nil, err
	}
	if len(words) == 0 {
		return nil, errors.New("comando della shell vuoto")
	}
	cmd := exec.Command(words[0], words[1:]...)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor")
	if h, err := os.UserHomeDir(); err == nil {
		cmd.Dir = h
	}
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(max(rows, 1)), Cols: uint16(max(cols, 1))})
	if err != nil {
		return nil, err
	}
	done := make(chan rclient.Result, 1)
	go func() {
		err := cmd.Wait()
		r := rclient.Result{}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			r.Code = ee.ExitCode()
		} else if err != nil {
			r.Code = 255
		}
		done <- r
	}()
	var res rclient.Result
	got := false
	return &gterm{
		In:  f,
		Out: f,
		Resize: func(r, c int) error {
			return pty.Setsize(f, &pty.Winsize{Rows: uint16(max(r, 1)), Cols: uint16(max(c, 1))})
		},
		Wait: func() rclient.Result {
			if !got {
				res, got = <-done, true
			}
			return res
		},
		Close: func() {
			_ = cmd.Process.Signal(syscall.SIGHUP)
			_ = f.Close()
			_ = cmd.Process.Kill()
		},
	}, nil
}
