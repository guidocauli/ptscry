//go:build !windows

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// term_unix.go: terminal handling for the portable client on Unix (macOS,
// Linux, BSD) with the standard library only: stty does the termios work, so
// no cgo and no platform-specific ioctl numbers are needed.

package main

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
)

// stty runs the system stty with f as the terminal. Fixed paths: PATH is not
// trusted for a program that handles the terminal.
func stty(f *os.File, args ...string) (string, error) {
	var last error
	for _, p := range []string{"/bin/stty", "/usr/bin/stty"} {
		if _, err := os.Stat(p); err != nil {
			last = err
			continue
		}
		cmd := exec.Command(p, args...)
		cmd.Stdin = f
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
	return "", last
}

func ctIsTTY(f *os.File) bool {
	_, err := stty(f, "-g")
	return err == nil
}

// ctRaw puts the terminal in raw mode and returns the function that restores it.
func ctRaw(f *os.File) (func(), error) {
	saved, err := stty(f, "-g")
	if err != nil || saved == "" {
		return nil, errors.New("standard input is not a terminal")
	}
	if _, err := stty(f, "raw", "-echo"); err != nil {
		return nil, err
	}
	return func() { _, _ = stty(f, saved) }, nil
}

func ctSize(f *os.File) (rows, cols int) {
	out, err := stty(f, "size")
	if err != nil {
		return 24, 80
	}
	p := strings.Fields(out)
	if len(p) != 2 {
		return 24, 80
	}
	r, e1 := strconv.Atoi(p[0])
	c, e2 := strconv.Atoi(p[1])
	if e1 != nil || e2 != nil || r <= 0 || c <= 0 {
		return 24, 80
	}
	return r, c
}

// ctWatchResize calls cb when the window size may have changed, until stop is closed.
func ctWatchResize(stop <-chan struct{}, cb func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	go func() {
		defer signal.Stop(ch)
		for {
			select {
			case <-ch:
				cb()
			case <-stop:
				return
			}
		}
	}()
}

// ctPrompt asks a question on the controlling terminal (not on stdin, which
// may be a pipe). secret turns the echo off.
func ctPrompt(prompt string, secret bool) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", errors.New("no terminal to ask on")
	}
	defer tty.Close()
	_, _ = tty.WriteString(prompt)
	if secret {
		if saved, err := stty(tty, "-g"); err == nil && saved != "" {
			_, _ = stty(tty, "-echo")
			defer func() { _, _ = stty(tty, saved); _, _ = tty.WriteString("\n") }()
		}
	}
	line, err := bufio.NewReader(tty).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
