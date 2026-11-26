//go:build windows

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later

package rclient

import (
	"io"
	"os"
	"strings"
)

// agentDial opens the Windows OpenSSH agent's named pipe (or the one named by
// SSH_AUTH_SOCK, e.g. from another agent).
func agentDial() (io.ReadWriteCloser, error) {
	p := `\\.\pipe\openssh-ssh-agent`
	if v := os.Getenv("SSH_AUTH_SOCK"); strings.HasPrefix(v, `\\.\pipe\`) {
		p = v
	}
	return os.OpenFile(p, os.O_RDWR, 0)
}
