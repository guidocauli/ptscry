//go:build !windows

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later

package rclient

import (
	"errors"
	"io"
	"net"
	"os"
	"time"
)

func agentDial() (io.ReadWriteCloser, error) {
	p := os.Getenv("SSH_AUTH_SOCK")
	if p == "" {
		return nil, errors.New("SSH_AUTH_SOCK is not set: no ssh-agent")
	}
	return net.DialTimeout("unix", p, 3*time.Second)
}
