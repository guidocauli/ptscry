//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// client_ops.go: ptscry rename / notify / send-keys / layout (local client).

package main

import (
	"errors"
	"fmt"
	"os"
)

// cmdSplit asks the client that shows this session (ptscry-gui, ptscry-tui)
// for a new pane: the request travels in the output (OSC 7799).
func cmdSplit(args []string) error {
	dir := "lr"
	for _, a := range args {
		switch a {
		case "-h":
			dir = "lr"
		case "-v":
			dir = "tb"
		default:
			return errors.New("usage: ptscry split [-h | -v]   (-h side by side, -v one over the other; inside ptscry-gui or ptscry-tui)")
		}
	}
	return clientRequest("split;" + dir)
}

func clientRequest(req string) error {
	if os.Getenv("PTSCRY_SESSION") == "" || !isTerminal(os.Stdout) {
		return errors.New("this works inside a session shown by ptscry-gui or ptscry-tui")
	}
	_, err := fmt.Fprintf(os.Stdout, "\x1b]7799;%s\x07", req)
	return err
}

func cmdOp(cmd string, args []string) error {
	if cmd == "layout" && len(args) == 2 && args[0] == "apply" {
		if !validLayoutName(args[1]) {
			return errors.New("invalid layout name")
		}
		return clientRequest("layout;" + args[1])
	}
	var file string
	if cmd == "layout" && len(args) == 3 && (args[0] == "save" || args[0] == "set") && args[2] != "-" {
		file, args = args[2], args[:2]
	}
	input := func(max int) (string, error) {
		if file != "" {
			f, err := os.Open(file)
			if err != nil {
				return "", err
			}
			defer f.Close()
			return readLimited(f, max)
		}
		if isTerminal(os.Stdin) {
			fmt.Fprintln(os.Stderr, "(reading from the standard input; end with Ctrl-D)")
		}
		return readLimited(os.Stdin, max)
	}
	hl, out, err := parseOp(cmd, args, input)
	if err != nil {
		return err
	}
	autostart := hl.Op == "layout-set" || hl.Op == "layout-get" || hl.Op == "layout-list" || hl.Op == "layout-del"
	fc, r, err := connect(hl, autostart)
	if err != nil {
		if errors.Is(err, errNoHost) {
			return fmt.Errorf("no such session: %q", hl.Name)
		}
		return err
	}
	fc.c.Close()
	fmt.Print(printOp(r, out))
	return nil
}
