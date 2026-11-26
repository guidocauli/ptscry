// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// shells.go: the shells the "terminale locale" can start, found on this
// computer, and the parsing of a command line typed by the user.

package main

import (
	"errors"
	"strings"
)

// shellChoice is a shell the user can pick: a name and its command line.
type shellChoice struct {
	Name, Cmd string
}

// splitCommand splits a command line into words: spaces separate them, double
// or single quotes keep spaces inside a word ("C:\Program Files\Git\bin\bash.exe" -l).
func splitCommand(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	in, quote := false, rune(0)
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, in = r, true
		case r == ' ' || r == '\t':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if quote != 0 {
		return nil, errors.New("virgolette non chiuse nel comando della shell")
	}
	if in {
		out = append(out, cur.String())
	}
	return out, nil
}

// validShellCmd tells whether a command line may be stored and run.
func validShellCmd(s string) bool {
	if len(s) > 512 || strings.ContainsAny(s, "\x00\r\n") {
		return false
	}
	w, err := splitCommand(s)
	return err == nil && (s == "" || len(w) > 0)
}
