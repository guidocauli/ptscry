//go:build !windows

// ptscry-tui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// backend.go: where the panes come from. Locally each pane is the ptscry
// client of this computer on a pseudoterminal ("ptscry new -A -n NAME"); on
// another server it is a terminal channel of one SSH connection to its
// front-end. Either way the ptscry client in the pane is the one that talks
// to the server: the panes are only windows on it.

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"golang.org/x/crypto/ssh"

	"ptscry/internal/rclient"
)

// paneIO is a running pane.
type paneIO struct {
	in     io.Writer
	out    io.Reader
	resize func(rows, cols int)
	close  func()
}

type backend interface {
	open(session string, rows, cols int) (*paneIO, error)
	// run runs a ptscry command (layout show, freeze...) and returns its output.
	run(args []string, stdin []byte) (string, error)
	where() string
}

// ---------------------------------------------------------------- this computer

type localBackend struct {
	bin    string
	prefix byte
}

func newLocalBackend(prefix byte) (*localBackend, error) {
	bin := ""
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "ptscry")
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			bin = p
		}
	}
	if bin == "" {
		p, err := exec.LookPath("ptscry")
		if err != nil {
			return nil, errors.New("the ptscry client is not installed here (or use -H to reach a server)")
		}
		bin = p
	}
	return &localBackend{bin: bin, prefix: prefix}, nil
}

func (b *localBackend) where() string {
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return "localhost"
}

func (b *localBackend) env() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "PTSCRY_PREFIX=") || strings.HasPrefix(kv, "PTSCRY_NO_PREFIX=") || strings.HasPrefix(kv, "TERM=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "TERM=xterm-256color", "PTSCRY_PREFIX="+rclient.PrefixName(b.prefix))
}

func (b *localBackend) open(session string, rows, cols int) (*paneIO, error) {
	cmd := exec.Command(b.bin, "new", "-A", "-n", session)
	cmd.Env = b.env()
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(max(rows, 1)), Cols: uint16(max(cols, 1))})
	if err != nil {
		return nil, err
	}
	var once sync.Once
	go func() { _ = cmd.Wait() }()
	return &paneIO{
		in:  f,
		out: f,
		resize: func(r, c int) {
			_ = pty.Setsize(f, &pty.Winsize{Rows: uint16(max(r, 1)), Cols: uint16(max(c, 1))})
		},
		close: func() {
			once.Do(func() {
				// the client detaches (the session goes on), then goes away
				_, _ = f.Write([]byte{b.prefix, 'd'})
				go func() {
					time.Sleep(2 * time.Second)
					if cmd.Process != nil {
						_ = cmd.Process.Kill()
					}
					f.Close()
				}()
			})
		},
	}, nil
}

func (b *localBackend) run(args []string, stdin []byte) (string, error) {
	cmd := exec.Command(b.bin, args...)
	cmd.Env = b.env()
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return "", err
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		s := strings.TrimSpace(out.String())
		if err != nil {
			if s != "" {
				return s, errors.New(strings.TrimPrefix(s, "ptscry: "))
			}
			return s, err
		}
		return s, nil
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		return "", errors.New("timed out")
	}
}

// ---------------------------------------------------------------- another server

type remoteBackend struct {
	cl     *ssh.Client
	target rclient.Target
	prefix byte
}

func newRemoteBackend(target, identity, cert, pinned string, prefix byte) (*remoteBackend, error) {
	t, err := rclient.ParseTarget(target)
	if err != nil {
		return nil, err
	}
	auth, err := rclient.Auth(identity, cert, ttyPrompt)
	if err != nil {
		return nil, err
	}
	cl, err := rclient.Connect(t, auth, pinned, tuiVersion, ttyPrompt)
	if err != nil {
		return nil, err
	}
	return &remoteBackend{cl: cl, target: t, prefix: prefix}, nil
}

func (b *remoteBackend) where() string { return b.target.Host }

func (b *remoteBackend) open(session string, rows, cols int) (*paneIO, error) {
	line, _, _, err := rclient.Cmdline([]string{"new", "-A", "-n", session})
	if err != nil {
		return nil, err
	}
	t, err := rclient.OpenTerm(b.cl, line, rows, cols, "xterm-256color", b.prefix)
	if err != nil {
		return nil, err
	}
	var once sync.Once
	return &paneIO{
		in:     t.In,
		out:    t.Out,
		resize: func(r, c int) { _ = t.Resize(r, c) },
		close: func() {
			once.Do(func() {
				_, _ = t.In.Write([]byte{b.prefix, 'd'})
				go func() {
					time.Sleep(2 * time.Second)
					t.Close()
				}()
			})
		},
	}, nil
}

func (b *remoteBackend) run(args []string, stdin []byte) (string, error) {
	line, _, _, err := rclient.Cmdline(args)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	var in io.Reader
	if stdin != nil {
		in = bytes.NewReader(stdin)
	}
	type res struct {
		r   rclient.Result
		err error
	}
	ch := make(chan res, 1)
	go func() {
		r, err := rclient.RunInput(b.cl, line, in, &out, &out)
		ch <- res{r, err}
	}()
	select {
	case x := <-ch:
		s := strings.TrimSpace(out.String())
		if x.err != nil {
			return s, x.err
		}
		if x.r.Code != 0 {
			if s != "" {
				return s, errors.New(strings.TrimPrefix(s, "ptscry: "))
			}
			return s, fmt.Errorf("exit %d", x.r.Code)
		}
		return s, nil
	case <-time.After(15 * time.Second):
		return "", errors.New("timed out")
	}
}
