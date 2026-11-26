// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// remote.go: the portable client. It talks to the SSH front-end of a ptscry
// server (see docs/MANUALE.md 3.5) from Linux, macOS or Windows, statically
// linked and without cgo.

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"ptscry/internal/rclient"
)

const remoteUsage = `Remote use (the server runs the ptscry front-end, port 2222 by default):

    ptscry [-H [user@]host[:port]] [-i key] [-c cert] [-F SHA256:...] [command...]

    -H   the server (or "server" in client.conf, or PTSCRY_SERVER)
    -i   private key file (default: the keys of the ssh-agent, also FIDO2 sk keys)
    -c   SSH certificate (needs its key: -i or the agent)
    -F   fingerprint of the server key, instead of the known_hosts file

  commands: new [-d] [-A] [-n name] [cmd...] | attach [-r] [-c] [name] | list [-j] | kill name
            run [-d] [-n name] [-k] cmd... | wait [-x] name | status [-j] name | stop
  no command = new -A -n main.   Detach: Ctrl-B d (prefix = in client.conf).
`

// stdinPump reads the keyboard for the whole life of the process and hands it
// to the current session, so that after a reconnection no keystroke is eaten
// by the reader of the previous one.
var stdinPump struct {
	once sync.Once
	mu   sync.Mutex
	w    io.WriteCloser
}

type stdinSink struct{}

func (stdinSink) set(w io.WriteCloser) {
	stdinPump.mu.Lock()
	stdinPump.w = w
	stdinPump.mu.Unlock()
	stdinPump.once.Do(func() {
		go func() {
			buf := make([]byte, 4096)
			for {
				n, err := os.Stdin.Read(buf)
				stdinPump.mu.Lock()
				cur := stdinPump.w
				stdinPump.mu.Unlock()
				if n > 0 && cur != nil {
					_, _ = cur.Write(buf[:n])
				}
				if err != nil {
					if cur != nil {
						_ = cur.Close()
					}
					return
				}
			}
		}()
	})
}

var stdinPumpSink stdinSink

// remoteRun runs one command on an open connection, on this terminal.
func remoteRun(cl *ssh.Client, line string, tty bool, prefix byte) (rclient.Result, error) {
	if !tty {
		return rclient.Run(cl, line, os.Stdout, os.Stderr)
	}
	if !ctIsTTY(os.Stdin) {
		return rclient.Result{}, errors.New("a terminal is required for this command")
	}
	rows, cols := ctSize(os.Stdin)
	t, err := rclient.OpenTerm(cl, line, rows, cols, os.Getenv("TERM"), prefix)
	if err != nil {
		return rclient.Result{}, err
	}
	defer t.Close()
	restore, err := ctRaw(os.Stdin)
	if err != nil {
		return rclient.Result{}, err
	}
	defer restore()
	stop := make(chan struct{})
	defer close(stop)
	ctWatchResize(stop, func() { r, c := ctSize(os.Stdin); _ = t.Resize(r, c) })
	stdinPumpSink.set(t.In)
	defer stdinPumpSink.set(nil)
	sig := make(chan os.Signal, 1) // leave the terminal usable if we are killed
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)
	go func() {
		if _, ok := <-sig; ok {
			restore()
			os.Exit(130)
		}
	}()
	copied := make(chan struct{})
	go func() { _, _ = io.Copy(os.Stdout, t.Out); close(copied) }()
	res := t.Wait()
	select { // let the last output reach the screen
	case <-copied:
	case <-time.After(2 * time.Second):
	}
	return res, nil
}

// remoteMain is the entry point of the remote client; it returns the exit code.
func remoteMain(args []string) int {
	fs := flag.NewFlagSet("ptscry", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	server := fs.String("H", "", "")
	identity := fs.String("i", "", "")
	certFile := fs.String("c", "", "")
	pinned := fs.String("F", "", "")
	if err := fs.Parse(args); err != nil {
		fmt.Fprint(os.Stderr, remoteUsage)
		return 2
	}
	cc, warn := readClientConf(clientConfigPaths(), true)
	for _, w := range warn {
		fmt.Fprintln(os.Stderr, "ptscry: client.conf:", w)
	}
	pick := func(vals ...string) string {
		for _, v := range vals {
			if v != "" {
				return v
			}
		}
		return ""
	}
	srv := pick(*server, os.Getenv("PTSCRY_SERVER"), cc.Server)
	if srv == "" {
		fmt.Fprint(os.Stderr, "ptscry: which server? Use -H [user@]host[:port], PTSCRY_SERVER, or \"server =\" in client.conf\n\n", remoteUsage)
		return 2
	}
	t, err := rclient.ParseTarget(srv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ptscry:", err)
		return 2
	}
	if *pinned != "" && !strings.HasPrefix(*pinned, "SHA256:") {
		fmt.Fprintln(os.Stderr, "ptscry: -F wants a fingerprint like SHA256:...")
		return 2
	}
	line, tty, name, err := rclient.Cmdline(fs.Args())
	var opIn io.Reader
	if a := fs.Args(); len(a) > 0 && (a[0] == "notify" || a[0] == "send-keys" || a[0] == "layout") {
		line, opIn, err = rclient.OpInput(a, os.Stdin)
		tty, name = false, ""
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ptscry:", err)
		return 2
	}
	idf, cf := pick(*identity, cc.Identity), pick(*certFile, cc.Cert)

	connect := func() (*ssh.Client, error) {
		auth, err := rclient.Auth(idf, cf, ctPrompt)
		if err != nil {
			return nil, err
		}
		return rclient.Connect(t, auth, *pinned, progVersion, ctPrompt)
	}
	cl, err := connect()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ptscry:", err)
		return 255
	}
	var once sync.Once
	closeAll := func() { once.Do(func() { cl.Close() }) }
	defer closeAll()

	var res rclient.Result
	if opIn != nil {
		res, err = rclient.RunInput(cl, line, opIn, os.Stdout, os.Stderr)
	} else {
		res, err = remoteRun(cl, line, tty, cc.Prefix)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ptscry:", err)
		return 255
	}
	// A terminal session whose connection drops is still alive on the server:
	// reattach to it.
	for attempt := 1; res.Lost && tty && name != "" && attempt <= 20; attempt++ {
		fmt.Fprintf(os.Stderr, "\r\nptscry: connection lost; reconnecting (%d/20, Ctrl-C to give up)...\r\n", attempt)
		closeAll()
		time.Sleep(time.Duration(min(3*attempt, 15)) * time.Second)
		ncl, err := connect()
		if err != nil {
			fmt.Fprintln(os.Stderr, "ptscry:", err)
			continue
		}
		cl, once = ncl, sync.Once{}
		res, err = remoteRun(cl, rclient.ProgName+" attach "+name, true, cc.Prefix)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ptscry:", err)
			res = rclient.Result{Code: 255, Lost: true}
		}
	}
	if res.Lost {
		fmt.Fprintln(os.Stderr, "\nptscry: connection lost")
	}
	return res.Code
}
