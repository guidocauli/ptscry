//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// pty_linux.go: pseudoterminal allocation and terminal ioctls (stdlib only).

package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

func ioctlPtr(fd, req uintptr, arg unsafe.Pointer) error {
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg))
	if e != 0 {
		return e
	}
	return nil
}

// fdControl runs fn on the raw descriptor without switching the file to
// blocking mode (which calling f.Fd() would do and which breaks Close
// unblocking a pending Read).
func fdControl(f *os.File, fn func(fd uintptr) error) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ierr error
	if err := rc.Control(func(fd uintptr) { ierr = fn(fd) }); err != nil {
		return err
	}
	return ierr
}

// openPTY allocates a master/slave pair (posix_openpt, unlockpt, ptsname).
func openPTY() (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if err != nil {
			master.Close()
		}
	}()
	var n uint32
	var unlock int32
	err = fdControl(master, func(fd uintptr) error {
		if e := ioctlPtr(fd, syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); e != nil {
			return e
		}
		return ioctlPtr(fd, syscall.TIOCGPTN, unsafe.Pointer(&n))
	})
	if err != nil {
		return nil, nil, fmt.Errorf("ptmx ioctl: %w", err)
	}
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	return master, slave, nil
}

type winsize struct{ Row, Col, X, Y uint16 }

func setWinsize(f *os.File, rows, cols uint16) error {
	ws := winsize{Row: rows, Col: cols}
	return fdControl(f, func(fd uintptr) error {
		return ioctlPtr(fd, syscall.TIOCSWINSZ, unsafe.Pointer(&ws))
	})
}

func getWinsize(f *os.File) (rows, cols uint16, err error) {
	var ws winsize
	err = fdControl(f, func(fd uintptr) error {
		return ioctlPtr(fd, syscall.TIOCGWINSZ, unsafe.Pointer(&ws))
	})
	return ws.Row, ws.Col, err
}

// foregroundPgrp returns the foreground process group of the terminal whose
// master is f.
func foregroundPgrp(f *os.File) (int, error) {
	var pg int32
	err := fdControl(f, func(fd uintptr) error {
		return ioctlPtr(fd, syscall.TIOCGPGRP, unsafe.Pointer(&pg))
	})
	return int(pg), err
}

func isTerminal(f *os.File) bool {
	var t syscall.Termios
	return fdControl(f, func(fd uintptr) error {
		return ioctlPtr(fd, syscall.TCGETS, unsafe.Pointer(&t))
	}) == nil
}

// makeRaw puts the terminal in raw mode and returns a function restoring it.
func makeRaw(f *os.File) (restore func(), err error) {
	var old syscall.Termios
	err = fdControl(f, func(fd uintptr) error {
		return ioctlPtr(fd, syscall.TCGETS, unsafe.Pointer(&old))
	})
	if err != nil {
		return nil, err
	}
	raw := old
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	err = fdControl(f, func(fd uintptr) error {
		return ioctlPtr(fd, syscall.TCSETS, unsafe.Pointer(&raw))
	})
	if err != nil {
		return nil, err
	}
	return func() {
		_ = fdControl(f, func(fd uintptr) error {
			return ioctlPtr(fd, syscall.TCSETS, unsafe.Pointer(&old))
		})
	}, nil
}
