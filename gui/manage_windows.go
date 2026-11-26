//go:build windows

// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import "os/exec"

func detachProc(cmd *exec.Cmd) {}
