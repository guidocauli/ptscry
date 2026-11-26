//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// client_run.go: run (a job in a session, no terminal needed) and wait.

package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// cmdRun: ptscry run [-n name] [-k] [-q] command [args...]
//
// Creates a session that runs the command (not a shell) and follows its
// output. The session ends by itself when the command ends and ptscry exits
// with the command's exit status. No terminal is needed, so it works from
// scripts and over `ssh host ptscry run ...` without -t. If the caller goes
// away (Ctrl-C, closed connection) the job keeps running, unless -k asks to
// kill it; it can be followed again with `ptscry wait NAME`.
func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	name := fs.String("n", "", "session name (default: automatic sN)")
	kill := fs.Bool("k", false, "kill the job if this command is interrupted")
	quiet := fs.Bool("q", false, "do not print the session name")
	detach := fs.Bool("d", false, "submit the job and return, keeping its result")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 || (*detach && *kill) {
		return errors.New("usage: ptscry run [-n name] [-k] [-q] command [args...] | ptscry run -d [-n name] command [args...]")
	}
	hl := hello{Op: "new", Name: *name, Cmd: fs.Args(), Rows: 24, Cols: 80, Env: clientEnv(), Job: *detach, Detached: *detach}
	fc, r, err := connect(hl, true)
	if err != nil {
		return err
	}
	if *detach {
		fc.c.Close()
		if r.Session != nil {
			fmt.Println(r.Session.Name)
		}
		return nil
	}
	return follow(fc, r, *kill, *quiet)
}

// cmdWait: ptscry wait [-q] name - follow a running session's output (read-only,
// no terminal needed) and exit with its exit status when it ends.
func cmdWait(args []string) error {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	quiet := fs.Bool("q", false, "do not print the session name")
	remove := fs.Bool("x", false, "discard the result of a finished job after collecting it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: ptscry wait [-q] [-x] name")
	}
	hl := hello{Op: "attach", Name: fs.Arg(0), Rows: 24, Cols: 80, ReadOnly: true, Remove: *remove, Env: clientEnv()}
	fc, r, err := connect(hl, false)
	if err != nil {
		if errors.Is(err, errNoHost) {
			return fmt.Errorf("no such session: %q", fs.Arg(0))
		}
		return err
	}
	return follow(fc, r, false, *quiet)
}

// follow copies the session's output to stdout until it ends.
func follow(fc *framed, r okReply, killOnAbort, quiet bool) error {
	name := ""
	if r.Session != nil {
		name = r.Session.Name
	}
	if !quiet {
		fmt.Fprintf(os.Stderr, "[session %s]\n", name)
	}
	type result struct {
		kind string
		code int
		msg  string
	}
	res := make(chan result, 1)
	go func() {
		for {
			t, p, err := fc.read()
			if err != nil {
				res <- result{kind: "lost", msg: err.Error()}
				return
			}
			switch t {
			case msgData:
				if _, err := os.Stdout.Write(p); err != nil {
					res <- result{kind: "stdout"}
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
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)

	select {
	case x := <-res:
		fc.c.Close()
		switch x.kind {
		case "exit":
			if x.code != 0 {
				return &exitStatus{x.code}
			}
			return nil
		case "bye":
			fmt.Fprintf(os.Stderr, "[%s (session %s)]\n", x.msg, name)
			return &exitStatus{1}
		case "stdout": // the reader of our output went away: leave the job running
			return &exitStatus{141}
		}
		fmt.Fprintf(os.Stderr, "[connection to host lost (session %s may still be running)]\n", name)
		return &exitStatus{1}
	case <-sig:
		_ = fc.write(msgDetach, nil)
		fc.c.Close()
		if killOnAbort {
			if kc, _, err := connect(hello{Op: "kill", Name: name}, false); err == nil {
				kc.c.Close()
				fmt.Fprintf(os.Stderr, "[session %s killed]\n", name)
			}
		} else {
			fmt.Fprintf(os.Stderr, "[detached: session %s keeps running; follow it with: ptscry wait %s]\n", name, name)
		}
		time.Sleep(50 * time.Millisecond)
		return &exitStatus{130}
	}
}
