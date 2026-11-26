//go:build !windows

// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os/exec"
	"syscall"
)

// detachProc makes the process live on after the GUI: its own session, no terminal.
func detachProc(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
