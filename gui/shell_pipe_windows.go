// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// shell_pipe_windows.go: the "terminale locale" without a pseudoconsole, for
// Windows before 10 1809 and for Wine: the shell runs on plain pipes and the
// program does the line editing a console would do (echo, backspace, Enter).
// There are no colours or full-screen programs, and a shell that asks for a
// console (PowerShell) is replaced by cmd.exe.

//go:build windows

package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"

	"golang.org/x/sys/windows"

	"ptscry/internal/rclient"
)

// isWine tells whether this is Wine (ntdll exports wine_get_version).
func isWine() bool {
	return windows.NewLazySystemDLL("ntdll.dll").NewProc("wine_get_version").Find() == nil
}

// splitExe takes the program out of a command line: "C:\a b\x.exe" -q, or x.exe -q.
func splitExe(line string) string {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, `"`) {
		if i := strings.Index(line[1:], `"`); i >= 0 {
			return line[1 : i+1]
		}
		return strings.Trim(line, `"`)
	}
	if i := strings.IndexAny(line, " \t"); i >= 0 {
		return line[:i]
	}
	return line
}

// lineWriter is what the terminal types into: it edits a line, echoes it, and
// sends it to the shell when Enter arrives.
type lineWriter struct {
	mu    sync.Mutex
	stdin io.WriteCloser
	echo  func([]byte)
	buf   []rune
	esc   int // inside an escape sequence: 1 after ESC, 2 inside CSI
}

func (l *lineWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	total := len(p)
	for len(p) > 0 {
		r, n := utf8.DecodeRune(p)
		p = p[n:]
		switch {
		case l.esc == 1:
			if r == '[' || r == 'O' {
				l.esc = 2
			} else {
				l.esc = 0
			}
		case l.esc == 2:
			if r >= 0x40 && r <= 0x7e {
				l.esc = 0
			}
		case r == 0x1b:
			l.esc = 1
		case r == '\r' || r == '\n':
			l.echo([]byte("\r\n"))
			line := string(l.buf) + "\r\n"
			l.buf = l.buf[:0]
			if _, err := io.WriteString(l.stdin, line); err != nil {
				return 0, err
			}
		case r == 0x7f || r == 0x08:
			if len(l.buf) > 0 {
				l.buf = l.buf[:len(l.buf)-1]
				l.echo([]byte("\b \b"))
			}
		case r == 0x03:
			l.buf = l.buf[:0]
			l.echo([]byte("^C\r\n"))
		case r < 0x20:
			// other control keys: nothing to do on a pipe
		default:
			l.buf = append(l.buf, r)
			l.echo([]byte(string(r)))
		}
	}
	return total, nil
}

func (l *lineWriter) Close() error { return l.stdin.Close() }

func openPipeShell(p profile) (*gterm, error) {
	line := strings.TrimSpace(p.Target)
	if line == "" {
		c := os.Getenv("COMSPEC")
		if c == "" {
			c = "cmd.exe"
		}
		line = `"` + c + `" /Q /K prompt $P$G`
	}
	exe := splitExe(line)
	if strings.ContainsAny(line, "\r\n\x00") || exe == "" {
		return nil, errors.New("comando non valido")
	}
	cmd := exec.Command(exe)
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: line, HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	if isWine() {
		cmd.SysProcAttr.CreationFlags = 0 // Wine's cmd then ignores the pipes it is given
	}
	if h, err := os.UserHomeDir(); err == nil {
		cmd.Dir = h
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	var wmu sync.Mutex
	out := func(b []byte) {
		wmu.Lock()
		defer wmu.Unlock()
		_, _ = pw.Write(b)
	}
	cmd.Stdout = writerFunc(out)
	cmd.Stderr = writerFunc(out)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go out([]byte("\x1b[2m[ptscry-gui: shell senza pseudoconsole (Wine o Windows prima della 10 1809): niente colori, prompt ne' programmi a schermo intero]\x1b[0m\r\n"))
	done := make(chan rclient.Result, 1)
	go func() {
		err := cmd.Wait() // the copies of stdout and stderr end with it
		r := rclient.Result{}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			r.Code = ee.ExitCode()
		}
		_ = pw.Close()
		done <- r
	}()
	var res rclient.Result
	got := false
	return &gterm{
		In:     &lineWriter{stdin: stdin, echo: out},
		Out:    pr,
		Resize: func(int, int) error { return nil },
		Wait: func() rclient.Result {
			if !got {
				res, got = <-done, true
			}
			return res
		},
		Close: func() {
			_ = stdin.Close()
			_ = cmd.Process.Kill()
			_ = pr.Close()
		},
	}, nil
}

type writerFunc func([]byte)

func (f writerFunc) Write(b []byte) (int, error) { f(b); return len(b), nil }
