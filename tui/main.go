//go:build !windows

// ptscry-tui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// Sessions of ptscry in panes on a text terminal, tmux style: each pane is a
// session of its own (with its audit and recording), the layout can be kept
// on the server ("ptscry layout") and opened again here or in ptscry-gui.

package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	uv "github.com/charmbracelet/ultraviolet"

	"ptscry/internal/rclient"
)

const tuiVersion = "0.7.0-tui"

const usage = `ptscry-tui - Guido Cauli <guido.cauli@gmail.com>
  sessions of ptscry in panes (version ` + tuiVersion + `)

Usage:
  ptscry-tui [-l LAYOUT] [session...]          this computer
  ptscry-tui -H [user@]host[:port] [-i key] [-c cert] [-F SHA256:...] [-l LAYOUT] [session...]
                                               a server, through its SSH front-end
  Without sessions: "main". -l opens a layout saved with "ptscry layout" (or
  from ptscry-gui). Options: -s no status bar, -m no mouse.

Inside (prefix Ctrl-B, or "prefix" in client.conf; prefix ? for this list):
  % "   split side by side / one over the other     x  close the pane (detach only)
  arrows o ;   move between panes     z zoom     Space  next preset layout
  Alt+arrows / Ctrl+arrows   resize    S  synchronized input (to every pane)
  [   copy mode: arrows, PgUp/PgDn, / ? n N search, Space select, Enter copy (OSC 52), q
  /   search      d  detach everything     W / O  save / open a layout on the server
  s ( ) L c $ f m M  are those of the session in the pane (see ptscry help)
`

func main() {
	fs := flag.NewFlagSet("ptscry-tui", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	server := fs.String("H", "", "")
	identity := fs.String("i", "", "")
	cert := fs.String("c", "", "")
	pinned := fs.String("F", "", "")
	lay := fs.String("l", "", "")
	noStatus := fs.Bool("s", false, "")
	noMouse := fs.Bool("m", false, "")
	version := fs.Bool("version", false, "")
	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		os.Exit(2)
	}
	if *version {
		fmt.Println("ptscry-tui " + tuiVersion + " - Guido Cauli <guido.cauli@gmail.com>")
		fmt.Println("Copyright (C) 2026 Guido Cauli. License AGPL-3.0-or-later: free software, WITHOUT ANY WARRANTY (see LICENSE and NOTICE.md).")
		return
	}
	sessions := fs.Args()
	for _, s := range sessions {
		if !nameRe.MatchString(s) {
			fail("invalid session name %q", s)
		}
	}
	if *lay != "" && !nameRe.MatchString(*lay) {
		fail("invalid layout name %q", *lay)
	}
	if len(sessions) > maxPanes {
		fail("at most %d panes", maxPanes)
	}
	if fi, err := os.Stdin.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		fail("a terminal is required")
	}
	if os.Getenv("PTSCRY_SESSION") != "" && os.Getenv("PTSCRY_TUI_NESTED") != "1" {
		fail("this is already a ptscry session: panes inside a pane would be confusing (PTSCRY_TUI_NESTED=1 to do it anyway)")
	}

	cc, warn := rclient.ReadConf(rclient.ConfigPaths(), true)
	for _, w := range warn {
		fmt.Fprintln(os.Stderr, "ptscry-tui: client.conf:", w)
	}
	prefix := byte(rclient.DefaultPrefix)
	if cc.Prefix != 0 {
		prefix = cc.Prefix
	}

	var be backend
	var err error
	srv := *server
	if srv == "" {
		srv = os.Getenv("PTSCRY_SERVER")
	}
	if srv != "" {
		if *pinned != "" && !strings.HasPrefix(*pinned, "SHA256:") {
			fail("-F wants a fingerprint like SHA256:...")
		}
		idf, cf := *identity, *cert
		if idf == "" {
			idf = cc.Identity
		}
		if cf == "" {
			cf = cc.Cert
		}
		be, err = newRemoteBackend(srv, idf, cf, *pinned, prefix)
	} else {
		be, err = newLocalBackend(prefix)
	}
	if err != nil {
		fail("%v", err)
	}

	var pl *plan
	if *lay != "" {
		out, err := be.run([]string{"layout", "show", *lay}, nil)
		if err != nil {
			fail("layout %s: %v", *lay, err)
		}
		if pl, err = parseLayout([]byte(out)); err != nil {
			fail("layout %s: %v", *lay, err)
		}
	}
	if pl == nil {
		if len(sessions) == 0 {
			sessions = []string{"main"}
		}
		pl = &plan{session: sessions[0], focus: true}
		for _, s := range sessions[1:] {
			pl = &plan{side: true, ratio: 0.5, a: pl, b: &plan{session: s}}
		}
	}

	a := &app{be: be, prefix: prefix, status: !*noStatus && cc.Status != "off", mouse: !*noMouse && cc.Mouse != "off",
		dirty: make(chan struct{}, 1), ctl: make(chan ctlReq, 16), gone: make(chan *pane, 16), quit: make(chan string, 1)}
	if cc.Cursor == "blink" || cc.Cursor == "steady" {
		a.cursorMode = cc.Cursor
	}
	a.t = uv.DefaultTerminal()
	a.scr = a.t.Screen()
	if err := a.t.Start(); err != nil {
		fail("%v", err)
	}
	_ = a.scr.EnterAltScreen()
	_ = a.scr.EnableBracketedPaste()
	if a.mouse {
		_ = a.scr.SetMouseMode(uv.MouseModeDrag)
	}
	a.w, a.h = termSize()
	_ = a.scr.Resize(a.w, a.h)
	msg := ""
	if err := a.openPlan(pl); err != nil {
		msg = "ptscry-tui: " + err.Error()
	} else {
		msg = a.run()
	}
	_ = a.scr.SetMouseMode(uv.MouseModeNone)
	_ = a.scr.ExitAltScreen()
	_ = a.scr.ShowCursor()
	if a.lastCursor != 0 {
		_, _ = a.scr.WriteString("\x1b[0 q") // the terminal's own cursor again
	}
	_ = a.scr.Flush()
	_ = a.t.Stop()
	for _, p := range a.panes() {
		p.close()
	}
	if msg != "" {
		fmt.Fprintln(os.Stderr, msg)
	}
	time.Sleep(200 * time.Millisecond) // let the panes' clients say goodbye
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ptscry-tui: "+format+"\n", args...)
	os.Exit(1)
}

// termSize asks stty for the size (before the event loop reports it).
func termSize() (w, h int) {
	cmd := exec.Command("stty", "size")
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	if err == nil {
		var r, c int
		if _, err := fmt.Sscanf(string(out), "%d %d", &r, &c); err == nil && r > 0 && c > 0 {
			return c, r
		}
	}
	return 80, 24
}

// ttyPrompt asks on the terminal (host key, passphrase, TOTP), before the
// panes take the screen.
func ttyPrompt(q string, secret bool) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", errors.New("no terminal to ask on")
	}
	defer tty.Close()
	_, _ = tty.WriteString(q)
	if secret {
		for _, p := range []string{"/bin/stty", "/usr/bin/stty"} {
			if _, err := os.Stat(p); err == nil {
				save := exec.Command(p, "-g")
				save.Stdin = tty
				if st, err := save.Output(); err == nil {
					off := exec.Command(p, "-echo")
					off.Stdin = tty
					_ = off.Run()
					defer func() {
						on := exec.Command(p, strings.TrimSpace(string(st)))
						on.Stdin = tty
						_ = on.Run()
						_, _ = tty.WriteString("\n")
					}()
				}
				break
			}
		}
	}
	line, err := bufio.NewReader(tty).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
