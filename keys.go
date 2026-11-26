// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// keys.go: the detach prefix (tmux style) as the server-side code sees it. The
// portable parts (parsing, client.conf) live in internal/rclient.

package main

import (
	"fmt"
	"os"
	"strings"

	"ptscry/internal/rclient"
)

// defaultPrefix is Ctrl-B, as in tmux.
const defaultPrefix = rclient.DefaultPrefix

type clientConf = rclient.Conf

func parsePrefix(s string) (byte, error) { return rclient.ParsePrefix(s) }
func validPrefixByte(b byte) bool        { return rclient.ValidPrefixByte(b) }
func prefixName(b byte) string           { return rclient.PrefixName(b) }
func clientConfigPaths() []string        { return rclient.ConfigPaths() }
func readClientConf(paths []string, checkOwner bool) (clientConf, []string) {
	return rclient.ReadConf(paths, checkOwner)
}

// clientPrefix is what the interactive client uses: client.conf, else what the
// server's policy says (serverPrefix, 0 = unknown), else Ctrl-B.
func clientPrefix(serverPrefix byte) byte {
	// ptscry-tui runs this client in each of its panes and tells it its own
	// prefix, so that the keys it passes on mean the same thing here
	if v := os.Getenv("PTSCRY_PREFIX"); v != "" {
		if b, err := parsePrefix(v); err == nil {
			return b
		}
	}
	cc, warn := readClientConf(clientConfigPaths(), true)
	for _, w := range warn {
		fmt.Fprintln(os.Stderr, "ptscry: client.conf:", w)
	}
	if cc.Prefix != 0 {
		return cc.Prefix
	}
	if serverPrefix != 0 {
		return serverPrefix
	}
	return defaultPrefix
}

// Detach: press the prefix (default Ctrl-B, see keys.go) then "d". The prefix
// twice sends a literal prefix to the session.
type inState struct {
	prefix  byte // 0 = defaultPrefix
	pending bool
	off     bool              // no prefix at all: everything goes to the session (the GUI has its own detach)
	help    func(text string) // shows the key help (prefix then h or ?) on the user's screen
}

// keyAct is a command typed after the prefix.
type keyAct int

const (
	actNone     keyAct = iota
	actDetach          // d
	actChoose          // s: the list of sessions
	actPrev            // (
	actNext            // )
	actLast            // L: the session attached before
	actNew             // c: a new session
	actRename          // $
	actFreeze          // f: freeze / thaw this session
	actWatchAct        // m: tell me about activity in the other sessions
	actWatchSil        // M: tell me about silence in the other sessions
	actTake            // w: take the keyboard of the session (the other clients only watch) / give it back
)

var actKeys = map[byte]keyAct{'d': actDetach, 'D': actDetach, 's': actChoose, '(': actPrev, ')': actNext,
	'L': actLast, 'c': actNew, '$': actRename, 'f': actFreeze, 'm': actWatchAct, 'M': actWatchSil, 'w': actTake}

// keyHelp is what "prefix h" shows: the keys of a session, and the commands to
// use outside of it. Lines end in CRLF (the terminal is in raw mode).
func keyHelp(pk byte) string {
	p := prefixName(pk)
	lines := []string{
		"",
		"ptscry - keys inside a session (prefix " + p + "):",
		"  " + p + " d          detach: the session keeps running (ptscry attach to come back)",
		"  " + p + " s          choose a session from the list (type to filter)",
		"  " + p + " ( / )      previous / next session;   " + p + " L   the session before this one",
		"  " + p + " c          new session;   " + p + " $   rename this session",
		"  " + p + " f          freeze / thaw this session (its processes stop and resume)",
		"  " + p + " m / M      tell me about activity / 30 s of silence in the other sessions (on/off)",
		"  " + p + " w          take the keyboard: other clients of this session only watch (again: give it back)",
		"  " + p + " " + p + "     send " + p + " itself to the program",
		"  " + p + " h  or ?    this help",
		"Outside the session: ptscry list | attach [-r] NAME | new -A -n NAME | kill NAME | rename OLD NEW",
		"  | notify NAME TEXT | send-keys NAME KEYS | freeze NAME | thaw NAME | run ... | status NAME | stop",
		"  (ptscry help: all of them). The prefix can be changed in client.conf (prefix = ctrl-a).",
		"",
	}
	return strings.Join(lines, "\r\n")
}

// process filters keyboard input, returning bytes to forward and whether the
// user asked to detach (the other commands are ignored: see processAct).
func (st *inState) process(in []byte) (out []byte, detach bool) {
	for {
		o, act, rest := st.processAct(in)
		out = append(out, o...)
		if act == actDetach {
			return out, true
		}
		if act == actNone {
			return out, false
		}
		in = rest
	}
}

// processAct filters keyboard input up to the first command: the bytes to
// forward before it, the command, and the input that follows it.
func (st *inState) processAct(in []byte) (out []byte, act keyAct, rest []byte) {
	if st.off {
		return append([]byte(nil), in...), actNone, nil
	}
	pk := st.prefix
	if pk == 0 {
		pk = defaultPrefix
	}
	for i, b := range in {
		if st.pending {
			st.pending = false
			if a, ok := actKeys[b]; ok {
				return out, a, append([]byte(nil), in[i+1:]...)
			}
			switch b {
			case 'h', 'H', '?':
				if st.help != nil {
					st.help(keyHelp(pk))
				}
			case pk:
				out = append(out, pk)
			default:
				out = append(out, pk, b)
			}
			continue
		}
		if b == pk {
			st.pending = true
			continue
		}
		out = append(out, b)
	}
	return out, actNone, nil
}
