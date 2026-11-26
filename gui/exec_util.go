// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"time"
)

func cmdEnv() []string { return os.Environ() }

// runTimeout runs cmd and returns its standard output; it is killed after d.
func runTimeout(cmd *exec.Cmd, d time.Duration) ([]byte, error) {
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return out.Bytes(), err
	case <-time.After(d):
		_ = cmd.Process.Kill()
		<-done
		return nil, errors.New("timeout")
	}
}
