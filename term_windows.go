//go:build windows

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// term_windows.go: console handling for the portable client on Windows
// (standard library only; needs Windows 10 1809 or later for the virtual
// terminal sequences).

package main

import (
	"bufio"
	"errors"
	"os"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode             = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode             = kernel32.NewProc("SetConsoleMode")
	procGetConsoleScreenBufferInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
	procSetConsoleOutputCP         = kernel32.NewProc("SetConsoleOutputCP")
	procGetConsoleOutputCP         = kernel32.NewProc("GetConsoleOutputCP")
)

const (
	enableProcessedInput       = 0x0001
	enableLineInput            = 0x0002
	enableEchoInput            = 0x0004
	enableVirtualTerminalInput = 0x0200
	enableProcessedOutput      = 0x0001
	enableVTProcessing         = 0x0004
)

func getMode(h syscall.Handle) (uint32, bool) {
	var m uint32
	r, _, _ := procGetConsoleMode.Call(uintptr(h), uintptr(unsafe.Pointer(&m)))
	return m, r != 0
}

func setMode(h syscall.Handle, m uint32) bool {
	r, _, _ := procSetConsoleMode.Call(uintptr(h), uintptr(m))
	return r != 0
}

func ctIsTTY(f *os.File) bool {
	_, ok := getMode(syscall.Handle(f.Fd()))
	return ok
}

// ctRaw switches the console to raw input with VT sequences on both sides.
// It acts on stdin and stdout; the returned function restores both.
func ctRaw(f *os.File) (func(), error) {
	in := syscall.Handle(f.Fd())
	out := syscall.Handle(os.Stdout.Fd())
	inMode, ok := getMode(in)
	if !ok {
		return nil, errors.New("standard input is not a console")
	}
	outMode, outOK := getMode(out)
	if !setMode(in, (inMode&^(enableLineInput|enableEchoInput|enableProcessedInput))|enableVirtualTerminalInput) {
		return nil, errors.New("cannot set the console to raw mode (Windows 10 1809 or later is required)")
	}
	oldCP, _, _ := procGetConsoleOutputCP.Call()
	if outOK {
		setMode(out, outMode|enableProcessedOutput|enableVTProcessing)
		procSetConsoleOutputCP.Call(65001)
	}
	return func() {
		setMode(in, inMode)
		if outOK {
			setMode(out, outMode)
			if oldCP != 0 {
				procSetConsoleOutputCP.Call(oldCP)
			}
		}
	}, nil
}

func ctSize(f *os.File) (rows, cols int) {
	var info struct {
		SizeX, SizeY             int16
		CurX, CurY               int16
		Attr                     uint16
		Left, Top, Right, Bottom int16
		MaxX, MaxY               int16
	}
	r, _, _ := procGetConsoleScreenBufferInfo.Call(os.Stdout.Fd(), uintptr(unsafe.Pointer(&info)))
	if r == 0 {
		return 24, 80
	}
	rows, cols = int(info.Bottom-info.Top)+1, int(info.Right-info.Left)+1
	if rows <= 0 || cols <= 0 {
		return 24, 80
	}
	return rows, cols
}

// ctWatchResize polls: Windows has no SIGWINCH for console programs.
func ctWatchResize(stop <-chan struct{}, cb func()) {
	go func() {
		r0, c0 := ctSize(os.Stdout)
		t := time.NewTicker(400 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if r, c := ctSize(os.Stdout); r != r0 || c != c0 {
					r0, c0 = r, c
					cb()
				}
			case <-stop:
				return
			}
		}
	}()
}

func ctPrompt(prompt string, secret bool) (string, error) {
	in, err := os.OpenFile("CONIN$", os.O_RDWR, 0)
	if err != nil {
		return "", errors.New("no console to ask on")
	}
	defer in.Close()
	out, err := os.OpenFile("CONOUT$", os.O_RDWR, 0)
	if err != nil {
		return "", errors.New("no console to ask on")
	}
	defer out.Close()
	_, _ = out.WriteString(prompt)
	if secret {
		h := syscall.Handle(in.Fd())
		if m, ok := getMode(h); ok {
			setMode(h, m&^enableEchoInput)
			defer func() { setMode(h, m); _, _ = out.WriteString("\r\n") }()
		}
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
