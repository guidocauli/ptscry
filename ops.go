//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// ops.go: the small session commands (tmux style): rename, notify,
// send-keys and the layouts saved on the server. What is shared by the host,
// the local client and the SSH front-end: limits, the cleaning of the notice
// text, the key names of send-keys and the drawing of a notice on a terminal.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"ptscry/internal/rclient"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxNotice   = 1024      // bytes of a notice, after cleaning
	maxSendKeys = 4096      // bytes typed by one send-keys
	maxLayout   = 16 * 1024 // bytes of a saved layout (JSON)
	maxLayouts  = 256       // layouts per user
	// maxOpText bounds hello.Text for every operation: JSON escaping can grow
	// it six times and the request must still fit in one control message.
	maxOpText = 16 * 1024
)

// opsWithText are the operations that carry hello.Text.
var opsWithText = map[string]bool{"notify": true, "send-keys": true, "layout-set": true, "admin-setup": true}

// cleanNotice keeps a notice to one line of printable text: no control
// characters (escape sequences could drive the terminal of whoever reads
// it), no format characters (bidirectional overrides could disguise it).
func cleanNotice(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "?")
	}
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t' || r == ' ':
			if !space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = true
			continue
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r), r == utf8.RuneError,
			unicode.Is(unicode.Co, r), unicode.Is(unicode.Cs, r):
			continue
		}
		if b.Len()+utf8.RuneLen(r) > maxNotice {
			break
		}
		b.WriteRune(r)
		space = false
	}
	return strings.TrimSpace(b.String())
}

// noticeAlt is the flag (first payload byte of msgNotice) telling that the
// session is on the alternate screen (a full-screen program).
const noticeAlt = 1

// noticeShown is how long a notice stays over a full-screen program before
// the program is asked to repaint.
const noticeShown = 4 * time.Second

// renderNotice draws a notice for a terminal: on the normal screen it is a
// line of its own, like a message from write(1); over a full-screen program
// it covers the last line, and redraw says the program should be asked to
// repaint a little later.
func renderNotice(p []byte) (out []byte, redraw bool) {
	if len(p) == 0 {
		return nil, false
	}
	alt := p[0]&noticeAlt != 0
	text := cleanNotice(string(p[1:])) // never trust the other side to have cleaned it
	if text == "" {
		return nil, false
	}
	var b strings.Builder
	if alt {
		b.WriteString("\x1b7\x1b[999;1H\x1b[2K\x1b[7m[ptscry] ")
		b.WriteString(text)
		b.WriteString("\x1b[0m\x1b8\a")
		return []byte(b.String()), true
	}
	b.WriteString("\r\n\x1b[7m[ptscry] ")
	b.WriteString(text)
	b.WriteString("\x1b[0m\a\r\n")
	return []byte(b.String()), false
}

func sendKeysText(args []string, literal bool) (string, error) {
	return rclient.SendKeysText(args, literal, maxSendKeys)
}

// validLayoutName is a session name that is not a hidden file.
func validLayoutName(n string) bool { return nameRe.MatchString(n) && n[0] != '.' }

// checkLayout accepts a layout only if it is a JSON object of reasonable
// size; the server keeps it as it is (the clients give it a meaning).
func checkLayout(s string) error {
	if len(s) == 0 || len(s) > maxLayout {
		return errors.New("a layout must be 1 to 16384 bytes of JSON")
	}
	if !utf8.ValidString(s) || strings.ContainsRune(s, 0) {
		return errors.New("a layout must be UTF-8 text")
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return errors.New("a layout must be a JSON object")
	}
	return nil
}

// opOut is what a small command prints from the reply.
type opOut int

const (
	outMessage   opOut = iota // r.Message
	outText                   // r.Text (layout show)
	outNames                  // r.Names, one per line (layout list)
	outNamesJSON              // r.Names as JSON (layout list -j)
)

// parseOp reads the arguments of rename, notify, send-keys and layout (the
// same for the local client and the SSH front-end). input gives what was
// sent on the standard input, when the text is not on the command line.
func parseOp(cmd string, args []string, input func(max int) (string, error)) (hello, opOut, error) {
	var hl hello
	switch cmd {
	case "rename":
		if len(args) != 2 {
			return hl, 0, errors.New("usage: ptscry rename OLD NEW")
		}
		if !nameRe.MatchString(args[1]) {
			return hl, 0, errors.New("invalid session name (allowed: letters, digits, . _ -; max 64)")
		}
		hl.Op, hl.Name, hl.NewName = "rename", args[0], args[1]
		return hl, outMessage, nil

	case "notify":
		if len(args) < 1 {
			return hl, 0, errors.New("usage: ptscry notify NAME [text...]   (no text: read from the standard input)")
		}
		text := strings.Join(args[1:], " ")
		if len(args) == 1 {
			var err error
			if text, err = input(maxNotice * 4); err != nil {
				return hl, 0, err
			}
		}
		text = cleanNotice(text)
		if text == "" {
			return hl, 0, errors.New("empty notice")
		}
		hl.Op, hl.Name, hl.Text = "notify", args[0], text
		return hl, outMessage, nil

	case "send-keys":
		literal := false
		if len(args) > 0 && args[0] == "-l" {
			literal, args = true, args[1:]
		}
		if len(args) < 1 {
			return hl, 0, errors.New("usage: ptscry send-keys [-l] NAME [keys...]   (Enter, Tab, Escape, C-c, M-x, Up...; -l: literal; no keys: the standard input)")
		}
		var text string
		var err error
		if len(args) == 1 {
			if text, err = input(maxSendKeys); err != nil {
				return hl, 0, err
			}
			if text == "" {
				return hl, 0, errors.New("nothing to send")
			}
		} else if text, err = sendKeysText(args[1:], literal); err != nil {
			return hl, 0, err
		}
		hl.Op, hl.Name, hl.Text = "send-keys", args[0], text
		return hl, outMessage, nil

	case "layout":
		use := errors.New("usage: ptscry layout list [-j] | show NAME | save NAME [FILE|-] | rm NAME")
		if len(args) < 1 {
			return hl, 0, use
		}
		switch args[0] {
		case "list", "ls":
			switch {
			case len(args) == 1:
				hl.Op = "layout-list"
				return hl, outNames, nil
			case len(args) == 2 && args[1] == "-j":
				hl.Op = "layout-list"
				return hl, outNamesJSON, nil
			}
		case "show", "get":
			if len(args) == 2 {
				hl.Op, hl.Name = "layout-get", args[1]
				return hl, outText, nil
			}
		case "rm", "del", "remove":
			if len(args) == 2 {
				hl.Op, hl.Name = "layout-del", args[1]
				return hl, outMessage, nil
			}
		case "save", "set":
			if len(args) == 2 || (len(args) == 3 && args[2] == "-") {
				text, err := input(maxLayout)
				if err != nil {
					return hl, 0, err
				}
				if err := checkLayout(text); err != nil {
					return hl, 0, err
				}
				hl.Op, hl.Name, hl.Text = "layout-set", args[1], text
				return hl, outMessage, nil
			}
		}
		if hl.Op == "" && len(args) >= 2 && !validLayoutName(args[1]) {
			return hl, 0, errors.New("invalid layout name (letters, digits, . _ -; not starting with a dot)")
		}
		return hl, 0, use
	}
	return hl, 0, fmt.Errorf("unknown command %q", cmd)
}

// printOp is what the command prints.
func printOp(r okReply, out opOut) string {
	switch out {
	case outText:
		if strings.HasSuffix(r.Text, "\n") {
			return r.Text
		}
		return r.Text + "\n"
	case outNames:
		if len(r.Names) == 0 {
			return ""
		}
		return strings.Join(r.Names, "\n") + "\n"
	case outNamesJSON:
		names := r.Names
		if names == nil {
			names = []string{}
		}
		b, _ := json.Marshal(names)
		return string(b) + "\n"
	}
	if r.Message == "" {
		return ""
	}
	return r.Message + "\n"
}

// readLimited reads r up to max bytes; more is an error, not a cut.
func readLimited(r io.Reader, max int) (string, error) {
	b, err := io.ReadAll(io.LimitReader(r, int64(max)+1))
	if err != nil {
		return "", err
	}
	if len(b) > max {
		return "", fmt.Errorf("input too long (max %d bytes)", max)
	}
	return string(b), nil
}
