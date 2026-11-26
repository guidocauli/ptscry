// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// ops.go: the client side of send-keys and of the commands that take a text
// (notify, send-keys, layout save): over ssh the text goes on the standard
// input, so that it is not limited to the characters allowed in a command.

package rclient

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"
)

// keyNames are the names send-keys understands (tmux spelling).
var keyNames = map[string]string{
	"Enter": "\r", "Tab": "\t", "BTab": "\x1b[Z", "Escape": "\x1b", "Esc": "\x1b", "Space": " ",
	"BSpace": "\x7f", "Up": "\x1b[A", "Down": "\x1b[B", "Right": "\x1b[C", "Left": "\x1b[D",
	"Home": "\x1b[H", "End": "\x1b[F", "PageUp": "\x1b[5~", "PPage": "\x1b[5~", "PageDown": "\x1b[6~",
	"NPage": "\x1b[6~", "Insert": "\x1b[2~", "IC": "\x1b[2~", "Delete": "\x1b[3~", "DC": "\x1b[3~",
	"F1": "\x1bOP", "F2": "\x1bOQ", "F3": "\x1bOR", "F4": "\x1bOS", "F5": "\x1b[15~", "F6": "\x1b[17~",
	"F7": "\x1b[18~", "F8": "\x1b[19~", "F9": "\x1b[20~", "F10": "\x1b[21~", "F11": "\x1b[23~", "F12": "\x1b[24~",
}

// KeyOf turns one key name into bytes: a name from keyNames, C-x (control),
// M-x (escape first); ok is false for anything else (a literal string).
func KeyOf(a string) (string, bool) {
	if k, ok := keyNames[a]; ok {
		return k, true
	}
	if strings.HasPrefix(a, "M-") && len(a) > 2 {
		if k, ok := KeyOf(a[2:]); ok {
			return "\x1b" + k, true
		}
		if utf8.RuneCountInString(a[2:]) == 1 {
			return "\x1b" + a[2:], true
		}
		return "", false
	}
	if (strings.HasPrefix(a, "C-") || strings.HasPrefix(a, "^")) && len(a) >= 2 {
		c := a[2:]
		if a[0] == '^' {
			c = a[1:]
		}
		if len(c) != 1 {
			return "", false
		}
		ch := c[0]
		switch {
		case ch >= 'a' && ch <= 'z':
			return string(rune(ch - 'a' + 1)), true
		case ch >= '@' && ch <= '_':
			return string(rune(ch - '@')), true
		case ch == '?':
			return "\x7f", true
		case ch == ' ':
			return "\x00", true
		}
	}
	return "", false
}

// SendKeysText is what send-keys types: each argument is a key name or, if
// it is not one (or literal is set), the text itself, as in tmux.
func SendKeysText(args []string, literal bool, max int) (string, error) {
	var b strings.Builder
	for _, a := range args {
		if !literal {
			if k, ok := KeyOf(a); ok {
				b.WriteString(k)
				continue
			}
		}
		b.WriteString(a)
	}
	if b.Len() == 0 {
		return "", errors.New("nothing to send")
	}
	if b.Len() > max {
		return "", fmt.Errorf("too much to send at once (max %d bytes)", max)
	}
	return b.String(), nil
}

// OpInput prepares rename / notify / send-keys / layout for the front-end:
// the command line (checked by Cmdline) and what goes on its standard input
// (nil: nothing).
func OpInput(args []string, stdin io.Reader) (line string, in io.Reader, err error) {
	if len(args) == 0 {
		return "", nil, errors.New("no command")
	}
	switch args[0] {
	case "notify":
		if len(args) < 2 {
			return "", nil, errors.New("usage: ptscry notify NAME [text...]")
		}
		line, _, _, err = Cmdline(args[:2])
		if len(args) == 2 {
			return line, stdin, err
		}
		return line, strings.NewReader(strings.Join(args[2:], " ")), err
	case "send-keys":
		a := args[1:]
		literal := len(a) > 0 && a[0] == "-l"
		if literal {
			a = a[1:]
		}
		if len(a) < 1 {
			return "", nil, errors.New("usage: ptscry send-keys [-l] NAME [keys...]")
		}
		line, _, _, err = Cmdline([]string{"send-keys", a[0]})
		if len(a) == 1 {
			return line, stdin, err
		}
		text, kerr := SendKeysText(a[1:], literal, 4096)
		if kerr != nil {
			return "", nil, kerr
		}
		return line, strings.NewReader(text), err
	case "layout":
		if len(args) >= 3 && (args[1] == "save" || args[1] == "set") {
			if len(args) > 4 {
				return "", nil, errors.New("usage: ptscry layout save NAME [FILE|-]")
			}
			line, _, _, err = Cmdline(args[:3])
			if len(args) == 4 && args[3] != "-" {
				b, rerr := os.ReadFile(args[3])
				if rerr != nil {
					return "", nil, rerr
				}
				return line, strings.NewReader(string(b)), err
			}
			return line, stdin, err
		}
	}
	line, _, _, err = Cmdline(args)
	return line, nil, err
}

// RunInput is Run with a standard input (sent, then closed).
func RunInput(cl *ssh.Client, line string, stdin io.Reader, stdout, stderr io.Writer) (Result, error) {
	if stdin == nil {
		return Run(cl, line, stdout, stderr)
	}
	s, err := cl.NewSession()
	if err != nil {
		return Result{}, err
	}
	defer s.Close()
	s.Stdin, s.Stdout, s.Stderr = stdin, stdout, stderr
	if err := s.Start(line); err != nil {
		return Result{}, err
	}
	return result(s.Wait()), nil
}
