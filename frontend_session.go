//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// frontend_session.go: one SSH session channel = one ptscry client.

package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

type sessState struct {
	pty        bool
	term       string
	rows, cols uint16
	env        map[string]string
	prefix     byte // chosen by the client (request ptscry-prefix); 0 = from the policy
	killOnDrop bool // `ptscry run -k`: kill the job if the connection goes away
	listJSON   bool
	statusName string // `ptscry status NAME`: answer from the list
	opOut      opOut  // rename, notify, send-keys, layout: what to print
	isOp       bool
	noPTYKey   bool                          // the key may not have a terminal (no-pty, restrict)
	input      func(max int) (string, error) // the channel's input (notify, send-keys, layout save)
}

// parseCommand turns the SSH command into a request. Like `ptscry login`, it
// never reaches a shell: only ptscry subcommands are understood.
func parseCommand(cmdline, defaultName string, st *sessState) (hello, bool, error) {
	hl := hello{Rows: st.rows, Cols: st.cols, Env: st.env}
	if strings.TrimSpace(cmdline) == "" {
		hl.Op, hl.Name, hl.Create = "new", defaultName, true
		return hl, true, nil
	}
	for _, r := range cmdline {
		ok := r == ' ' || r == '_' || r == '.' || r == '/' || r == ':' || r == '=' ||
			r == '@' || r == '+' || r == ',' || r == '-' ||
			(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !ok {
			return hl, false, errors.New("command not permitted")
		}
	}
	f := strings.Fields(cmdline)
	if f[0] != progName || len(f) < 2 {
		return hl, false, errors.New("command not permitted")
	}
	fs := flag.NewFlagSet(f[1], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	switch f[1] {
	case "new":
		name := fs.String("n", "", "")
		detached := fs.Bool("d", false, "")
		attachIfExists := fs.Bool("A", false, "")
		if err := fs.Parse(f[2:]); err != nil {
			return hl, false, errors.New("usage: ptscry new [-d] [-A] [-n name] [command...]")
		}
		hl.Op, hl.Name, hl.Cmd, hl.Create, hl.Detached = "new", *name, fs.Args(), *attachIfExists, *detached
		return hl, !*detached, nil
	case "attach":
		ro := fs.Bool("r", false, "")
		create := fs.Bool("c", false, "")
		if err := fs.Parse(f[2:]); err != nil || fs.NArg() > 1 {
			return hl, false, errors.New("usage: ptscry attach [-r] [-c] [name]")
		}
		hl.Op, hl.Name, hl.ReadOnly, hl.Create = "attach", fs.Arg(0), *ro, *create
		return hl, true, nil
	case "run":
		name := fs.String("n", "", "")
		kill := fs.Bool("k", false, "")
		_ = fs.Bool("q", false, "")
		detach := fs.Bool("d", false, "")
		if err := fs.Parse(f[2:]); err != nil || fs.NArg() == 0 || (*detach && *kill) {
			return hl, false, errors.New("usage: ptscry run [-n name] [-k] [-q] command [args...] | ptscry run -d [-n name] command [args...]")
		}
		hl.Op, hl.Name, hl.Cmd, st.killOnDrop = "new", *name, fs.Args(), *kill
		hl.Job, hl.Detached = *detach, *detach
		return hl, false, nil
	case "wait":
		_ = fs.Bool("q", false, "")
		rm := fs.Bool("x", false, "")
		if err := fs.Parse(f[2:]); err != nil || fs.NArg() != 1 {
			return hl, false, errors.New("usage: ptscry wait [-q] [-x] name")
		}
		hl.Op, hl.Name, hl.ReadOnly, hl.Remove = "attach", fs.Arg(0), true, *rm
		return hl, false, nil
	case "list":
		asJSON := fs.Bool("j", false, "")
		if err := fs.Parse(f[2:]); err != nil || fs.NArg() != 0 {
			return hl, false, errors.New("usage: ptscry list [-j]")
		}
		hl.Op = "list"
		st.listJSON = *asJSON
		return hl, false, nil
	case "status":
		asJSON := fs.Bool("j", false, "")
		if err := fs.Parse(f[2:]); err != nil || fs.NArg() != 1 {
			return hl, false, errors.New("usage: ptscry status [-j] name")
		}
		hl.Op = "list"
		st.statusName, st.listJSON = fs.Arg(0), *asJSON
		return hl, false, nil
	case "kill":
		if len(f) != 3 {
			return hl, false, errors.New("usage: ptscry kill <name>")
		}
		hl.Op, hl.Name = "kill", f[2]
		return hl, false, nil
	case "freeze", "thaw":
		if len(f) != 3 {
			return hl, false, fmt.Errorf("usage: ptscry %s <name>", f[1])
		}
		hl.Op, hl.Name = f[1], f[2]
		return hl, false, nil
	case "stop":
		hl.Op = "stop"
		return hl, false, nil
	case "rename", "notify", "send-keys", "layout":
		if f[1] == "send-keys" && st.noPTYKey {
			// typing into a shell is having a terminal, by another way
			return hl, false, errors.New("send-keys is not permitted for a key without a terminal (no-pty)")
		}
		if f[1] == "layout" && len(f) == 5 && f[4] != "-" {
			return hl, false, errors.New("layout save: the layout comes on the standard input over ssh")
		}
		input := st.input
		if input == nil {
			input = func(int) (string, error) { return "", errors.New("no input") }
		}
		op, out, err := parseOp(f[1], f[2:], input)
		if err != nil {
			return hl, false, err
		}
		op.Rows, op.Cols = 0, 0
		st.isOp, st.opOut = true, out
		return op, false, nil
	}
	return hl, false, fmt.Errorf("%q is not permitted over ssh", f[1])
}

func sendExit(ch ssh.Channel, code int) {
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(code)}))
}

// session serves one channel: collect pty/env requests, then run the command.
func (fe *frontend) session(nch ssh.NewChannel, base *proxyInfo, noPTY bool) {
	ch, reqs, err := nch.Accept()
	if err != nil {
		return
	}
	defer ch.Close()
	st := &sessState{env: map[string]string{}, noPTYKey: noPTY}
	resize := make(chan [2]uint16, 8)
	closed := make(chan struct{})
	var wg sync.WaitGroup
	started := false

	for r := range reqs {
		switch r.Type {
		case "pty-req":
			var p struct {
				Term       string
				Cols, Rows uint32
				W, H       uint32
				Modes      string
			}
			if noPTY || started || ssh.Unmarshal(r.Payload, &p) != nil || p.Rows == 0 || p.Cols == 0 ||
				p.Rows > 1000 || p.Cols > 1000 || !termRe.MatchString(p.Term) {
				_ = r.Reply(false, nil)
				continue
			}
			st.pty, st.term, st.rows, st.cols = true, p.Term, uint16(p.Rows), uint16(p.Cols)
			_ = r.Reply(true, nil)
		case "env":
			var e struct{ Name, Value string }
			ok := !started && ssh.Unmarshal(r.Payload, &e) == nil && envAllowed(e.Name) && e.Name != "TERM" &&
				len(e.Value) <= 256 && !strings.ContainsRune(e.Value, 0) && len(st.env) < 16
			if ok {
				st.env[e.Name] = e.Value
			}
			_ = r.Reply(ok, nil)
		case "ptscry-prefix": // the client announces its own detach key (client.conf)
			ok := !started && len(r.Payload) == 1 && validPrefixByte(r.Payload[0])
			if ok {
				st.prefix = r.Payload[0]
			}
			_ = r.Reply(ok, nil)
		case "window-change":
			var w struct{ Cols, Rows, W, H uint32 }
			if ssh.Unmarshal(r.Payload, &w) == nil && w.Rows > 0 && w.Cols > 0 && w.Rows <= 1000 && w.Cols <= 1000 {
				select {
				case resize <- [2]uint16{uint16(w.Rows), uint16(w.Cols)}:
				default:
				}
			}
		case "shell", "exec":
			if started {
				_ = r.Reply(false, nil)
				continue
			}
			cmdline := ""
			if r.Type == "exec" {
				var e struct{ Command string }
				if ssh.Unmarshal(r.Payload, &e) != nil || len(e.Command) > 4096 {
					_ = r.Reply(false, nil)
					continue
				}
				cmdline = e.Command
			}
			started = true
			_ = r.Reply(true, nil)
			if st.pty {
				st.env["TERM"] = st.term
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				fe.run(ch, cmdline, st, base, resize, closed)
				ch.Close()
			}()
		default: // subsystem, x11-req, auth-agent, signal, break ...
			_ = r.Reply(false, nil)
		}
	}
	close(closed)
	wg.Wait()
}

func (fe *frontend) connect(hl hello) (*framed, okReply, error) {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		fc, err := fe.dial()
		if err != nil {
			return nil, okReply{}, errors.New("the ptscry service is unavailable")
		}
		_ = fc.c.SetDeadline(time.Now().Add(20 * time.Second))
		r, err := request(fc, hl)
		_ = fc.c.SetDeadline(time.Time{})
		if err == nil {
			return fc, r, nil
		}
		fc.c.Close()
		lastErr = err
		msg := err.Error()
		if strings.Contains(msg, errHostClosingMsg) || strings.Contains(msg, "host unavailable") {
			time.Sleep(300 * time.Millisecond)
			continue
		}
		break
	}
	return nil, okReply{}, lastErr
}

func (fe *frontend) run(ch ssh.Channel, cmdline string, st *sessState, base *proxyInfo, resize chan [2]uint16, closed chan struct{}) {
	crlf := func(s string) string {
		if st.pty {
			return strings.ReplaceAll(s, "\n", "\r\n")
		}
		return s
	}
	fail := func(msg string) {
		_, _ = io.WriteString(ch.Stderr(), crlf("ptscry: "+msg+"\n"))
		sendExit(ch, 1)
	}
	st.input = func(max int) (string, error) {
		if st.pty {
			return "", errors.New("the text must come on the standard input, without a terminal (ssh without -t)")
		}
		type res struct {
			s   string
			err error
		}
		c := make(chan res, 1)
		go func() {
			s, err := readLimited(ch, max)
			c <- res{s, err}
		}()
		select {
		case r := <-c:
			return r.s, r.err
		case <-time.After(30 * time.Second):
			return "", errors.New("timed out reading the standard input")
		}
	}
	hl, needPTY, err := parseCommand(cmdline, fe.in.DefaultSession, st)
	if err != nil {
		fail(err.Error())
		return
	}
	if needPTY && !st.pty {
		fail("a terminal is required (use ssh -t)")
		return
	}
	if (hl.Rows == 0 || hl.Cols == 0) && !st.isOp {
		hl.Rows, hl.Cols = 24, 80
	}
	p := *base
	hl.Proxy = &p
	fc, r, err := fe.connect(hl)
	if err != nil {
		fail(err.Error())
		return
	}
	if hl.Op != "new" && hl.Op != "attach" || hl.Detached {
		fc.c.Close()
		switch {
		case st.isOp:
			_, _ = io.WriteString(ch, crlf(printOp(r, st.opOut)))
		case hl.Op == "list" && st.statusName != "":
			text, code := statusOf(r, st.statusName, st.listJSON)
			_, _ = io.WriteString(ch, crlf(text))
			sendExit(ch, code)
			return
		case hl.Op == "list" && st.listJSON:
			_, _ = io.WriteString(ch, crlf(listJSON(r)))
		case hl.Op == "list":
			_, _ = io.WriteString(ch, crlf(formatList(r)))
		case hl.Detached && r.Session != nil:
			_, _ = io.WriteString(ch, crlf(r.Session.Name+"\n"))
		case r.Message != "":
			_, _ = io.WriteString(ch, crlf(r.Message+"\n"))
		}
		sendExit(ch, 0)
		return
	}
	name := ""
	if r.Session != nil {
		name = r.Session.Name
	}
	prefix := r.Prefix
	if st.prefix != 0 {
		prefix = st.prefix
	}
	if st.pty {
		fe.pumpTerm(ch, fc, r, st, hl.ReadOnly, resize, closed, crlf, base, prefix)
		return
	}
	fe.pump(ch, fc, name, st.pty, resize, closed, crlf, st.killOnDrop, base, prefix)
}

// pumpTerm serves an attached terminal, with the session commands of the
// prefix: each one is a request on behalf of the same user and login.
func (fe *frontend) pumpTerm(ch ssh.Channel, fc *framed, r okReply, st *sessState, readOnly bool,
	resize chan [2]uint16, closed chan struct{}, crlf func(string) string, base *proxyInfo, prefix byte) {
	keys := make(chan []byte, 8)
	go func() {
		defer close(keys)
		buf := make([]byte, 4096)
		for {
			n, err := ch.Read(buf)
			if n > 0 {
				select {
				case keys <- append([]byte(nil), buf[:n]...):
				case <-closed:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	var szMu sync.Mutex
	rows, cols := st.rows, st.cols
	sized := make(chan [2]uint16, 4)
	go func() {
		for {
			select {
			case sz := <-resize:
				szMu.Lock()
				rows, cols = sz[0], sz[1]
				szMu.Unlock()
				select {
				case sized <- sz:
				default:
				}
			case <-closed:
				return
			}
		}
	}()
	env := map[string]string{}
	for k, v := range st.env {
		env[k] = v
	}
	ts := &termSide{keys: keys, out: ch, resize: sized, gone: closed,
		size: func() (uint16, uint16) {
			szMu.Lock()
			defer szMu.Unlock()
			return rows, cols
		},
		dial: func(hl hello) (*framed, okReply, error) {
			p := *base
			hl.Proxy = &p
			return fe.connect(hl)
		},
		prefix: prefix, readOnly: readOnly, env: env}
	lr := runAttachLoop(ts, fc, r)
	switch lr.kind {
	case "exit":
		_, _ = io.WriteString(ch.Stderr(), crlf(fmt.Sprintf("[session %s exited with status %d]\n", lr.name, lr.code)))
		sendExit(ch, lr.code)
	case "bye":
		_, _ = io.WriteString(ch.Stderr(), crlf(fmt.Sprintf("[%s (session %s)]\n", lr.msg, lr.name)))
		sendExit(ch, 0)
	case "lost":
		_, _ = io.WriteString(ch.Stderr(), crlf(fmt.Sprintf("[connection to host lost (session %s still running if the host is alive)]\n", lr.name)))
		sendExit(ch, 1)
	}
}

func (fe *frontend) pump(ch ssh.Channel, fc *framed, name string, pty bool, resize chan [2]uint16, closed chan struct{}, crlf func(string) string, killOnDrop bool, base *proxyInfo, prefix byte) {
	type result struct {
		kind string
		code int
		msg  string
	}
	res := make(chan result, 1)
	go func() { // host -> client
		for {
			t, p, err := fc.read()
			if err != nil {
				res <- result{kind: "lost"}
				return
			}
			switch t {
			case msgData:
				if _, err := ch.Write(p); err != nil {
					res <- result{kind: "gone"}
					return
				}
			case msgExit:
				code := 0
				if len(p) == 4 {
					code = int(int32(binary.BigEndian.Uint32(p)))
				}
				res <- result{kind: "exit", code: code}
				return
			case msgBye:
				res <- result{kind: "bye", msg: string(p)}
				return
			}
		}
	}()
	go func() { // client -> host
		if !pty { // run / wait: nothing to type; ignore input, and its EOF is not a detach
			_, _ = io.Copy(io.Discard, ch)
			return
		}
		in := inState{prefix: prefix, help: func(s string) { _, _ = ch.Write([]byte(s)) }}
		buf := make([]byte, 4096)
		for {
			n, err := ch.Read(buf)
			if n > 0 {
				out, detach := in.process(buf[:n])
				if len(out) > 0 {
					if fc.write(msgData, out) != nil {
						return
					}
				}
				if detach {
					_ = fc.write(msgDetach, nil)
					return
				}
			}
			if err != nil { // client gone: leave the session running
				_ = fc.write(msgDetach, nil)
				return
			}
		}
	}()
	go func() {
		for {
			select {
			case sz := <-resize:
				_ = fc.write(msgResize, encodeResize(sz[0], sz[1]))
			case <-closed:
				return
			}
		}
	}()

	var r result
	select {
	case r = <-res:
	case <-closed:
		_ = fc.write(msgDetach, nil)
		if killOnDrop && name != "" {
			p := *base
			if kc, _, err := fe.connect(hello{Op: "kill", Name: name, Proxy: &p}); err == nil {
				kc.c.Close()
			}
		}
		select {
		case r = <-res:
		case <-time.After(2 * time.Second):
			r = result{kind: "gone"}
		}
	}
	fc.c.Close()
	switch r.kind {
	case "exit":
		_, _ = io.WriteString(ch.Stderr(), crlf(fmt.Sprintf("[session %s exited with status %d]\n", name, r.code)))
		sendExit(ch, r.code)
	case "bye":
		_, _ = io.WriteString(ch.Stderr(), crlf(fmt.Sprintf("[%s (session %s)]\n", r.msg, name)))
		sendExit(ch, 0)
	case "lost":
		_, _ = io.WriteString(ch.Stderr(), crlf(fmt.Sprintf("[connection to host lost (session %s still running if the host is alive)]\n", name)))
		sendExit(ch, 1)
	}
}
