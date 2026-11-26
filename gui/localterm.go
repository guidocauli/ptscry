// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// localterm.go: the "standalone" profile, this computer without any server: the
// ptscry client is run on a local pseudoterminal with PTSCRY_STANDALONE=1, which
// starts (or finds) the per-user host. No root, no SSH, no open port. The
// sessions live in the host, so they outlive the window.

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/creack/pty"

	"ptscry/internal/rclient"
)

// gterm is a running terminal, whatever it is attached through.
type gterm struct {
	In     io.WriteCloser
	Out    io.Reader
	Resize func(rows, cols int) error
	Wait   func() rclient.Result
	Close  func()
}

func fromSSH(t *rclient.Term) *gterm {
	return &gterm{In: t.In, Out: t.Out, Resize: t.Resize, Wait: t.Wait, Close: t.Close}
}

// ptscryBinary finds the command-line client: the profile's own path, one next
// to this program, or the PATH.
func ptscryBinary(p profile) (string, error) {
	name := "ptscry"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if p.Target != "" {
		if _, err := os.Stat(p.Target); err == nil {
			return p.Target, nil
		}
		return "", fmt.Errorf("%s: non trovato", p.Target)
	}
	if exe, err := os.Executable(); err == nil {
		if b := filepath.Join(filepath.Dir(exe), name); fileOK(b) {
			return b, nil
		}
	}
	if b, err := exec.LookPath(name); err == nil {
		return b, nil
	}
	return "", errors.New("il comando ptscry non e' stato trovato: mettilo accanto a ptscry-gui o nel PATH, oppure indicane il percorso nel profilo")
}

func fileOK(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0
}

// localConf writes (once) the configuration of the standalone host: everything
// under the user's own state directory.
func localConf() (conf, state string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	state = filepath.Join(home, ".local", "state", "ptscry")
	if d := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(d) {
		state = filepath.Join(d, "ptscry")
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		return "", "", err
	}
	cd, err := os.UserConfigDir()
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(filepath.Join(cd, "ptscry"), 0o700); err != nil {
		return "", "", err
	}
	conf = filepath.Join(cd, "ptscry", "local.conf")
	if _, err := os.Stat(conf); err == nil {
		return conf, state, nil
	}
	body := fmt.Sprintf("[global]\nsocket_dir = %s\naudit_log = %s\nrecord_dir = %s\nsession_lifecycle = persistent_per_user\n",
		filepath.Join(state, "run"), filepath.Join(state, "audit.log"), filepath.Join(state, "rec"))
	if strings.ContainsAny(state, "\n\r") {
		return "", "", errors.New("percorso non valido")
	}
	return conf, state, os.WriteFile(conf, []byte(body), 0o600)
}

func openLocal(p profile, session string, ro bool, rows, cols int) (*gterm, error) {
	if p.Mode == "shell" {
		return openShell(p, rows, cols)
	}
	if runtime.GOOS == "windows" {
		return nil, errors.New("la modalita' senza server non e' disponibile su Windows: usa un profilo remoto")
	}
	bin, err := ptscryBinary(p)
	if err != nil {
		return nil, err
	}
	conf, _, err := localConf()
	if err != nil {
		return nil, err
	}
	args := []string{"new", "-A", "-n", session}
	if ro {
		args = []string{"attach", "-r", session}
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = withShell(append(os.Environ(), "PTSCRY_STANDALONE=1", "PTSCRY_CONFIG="+conf, "PTSCRY_NO_PREFIX=1", "TERM=xterm-256color"))
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
		// closing the window only detaches: the client is hung up, the host keeps the session
		Close: func() {
			_ = f.Close() // the slave side gets a hangup
			_ = cmd.Process.Kill()
		},
	}, nil
}
