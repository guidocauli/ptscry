//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// tui_exec.go: "ptscry tui" and "ptscry attach -l" run ptscry-tui, the
// terminal client with panes (a program of its own: the server binary does
// not carry a terminal emulator).

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func tuiBinary() (string, error) {
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "ptscry-tui")
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p, nil
		}
	}
	for _, p := range []string{"/usr/bin/ptscry-tui", "/usr/local/bin/ptscry-tui"} {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p, nil
		}
	}
	return "", errors.New("ptscry-tui is not installed (it comes with the client packages)")
}

func cmdTUI(args []string) error {
	bin, err := tuiBinary()
	if err != nil {
		return err
	}
	return syscall.Exec(bin, append([]string{bin}, args...), os.Environ())
}
