// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// Test helper: a small SSH client and certificate tool used by tests/e2e_frontend.py.
// Not installed; built with `go build ./tests/sshclient`.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

func die(f string, a ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", a...)
	os.Exit(99)
}

func main() {
	if len(os.Args) < 2 {
		die("usage: sshclient gen-key|sign|hostkey|run ...")
	}
	switch os.Args[1] {
	case "gen-key":
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		blk, err := ssh.MarshalPrivateKey(priv, "test")
		if err != nil {
			die("%v", err)
		}
		if err := os.WriteFile(os.Args[2], pem.EncodeToMemory(blk), 0o600); err != nil {
			die("%v", err)
		}
		sp, _ := ssh.NewPublicKey(pub)
		_ = os.WriteFile(os.Args[2]+".pub", ssh.MarshalAuthorizedKey(sp), 0o644)
	case "sign": // sign CAKEY USERPUB OUT ID PRINCIPAL VALID_SECONDS [source-address] [serial]
		a := os.Args[2:]
		caRaw, _ := os.ReadFile(a[0])
		ca, err := ssh.ParsePrivateKey(caRaw)
		if err != nil {
			die("%v", err)
		}
		pubRaw, _ := os.ReadFile(a[1])
		pub, _, _, _, err := ssh.ParseAuthorizedKey(pubRaw)
		if err != nil {
			die("%v", err)
		}
		secs, _ := strconv.ParseUint(a[5], 10, 64)
		now := uint64(time.Now().Unix())
		c := &ssh.Certificate{Key: pub, KeyId: a[3], CertType: ssh.UserCert, ValidPrincipals: []string{a[4]},
			ValidAfter: now - 30, ValidBefore: now + secs,
			Permissions: ssh.Permissions{Extensions: map[string]string{"permit-pty": ""}}}
		if secs == 0 {
			c.ValidBefore = ssh.CertTimeInfinity
		}
		if len(a) > 6 && a[6] != "" {
			c.CriticalOptions = map[string]string{"source-address": a[6]}
		}
		if len(a) > 7 {
			c.Serial, _ = strconv.ParseUint(a[7], 10, 64)
		}
		if err := c.SignCert(rand.Reader, ca); err != nil {
			die("%v", err)
		}
		_ = os.WriteFile(a[2], ssh.MarshalAuthorizedKey(c), 0o644)
	case "run":
		run(os.Args[2:])
	default:
		die("unknown command")
	}
}

func run(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:2222", "")
	user := fs.String("user", "", "")
	keyf := fs.String("key", "", "")
	certf := fs.String("cert", "", "")
	totp := fs.String("totp", "", "")
	pty := fs.Bool("pty", false, "")
	script := fs.String("script", "", "ms:\"quoted\";ms:\"quoted\"")
	tail := fs.Int("tail", 600, "ms to wait for output after the script")
	rows := fs.Int("rows", 24, "")
	cols := fs.Int("cols", 80, "")
	expectAuthFail := fs.Bool("expect-auth-fail", false, "")
	dial := fs.String("dial", "", "try a direct-tcpip channel to host:port")
	x11 := fs.Bool("x11", false, "request X11 forwarding")
	cmds := fs.String("cmds", "", "run several commands, one session each, on this connection (;; separated; SHELL = shell request)")
	fs.Parse(args)

	raw, err := os.ReadFile(*keyf)
	if err != nil {
		die("%v", err)
	}
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		die("%v", err)
	}
	if *certf != "" {
		cr, _ := os.ReadFile(*certf)
		pk, _, _, _, err := ssh.ParseAuthorizedKey(cr)
		if err != nil {
			die("%v", err)
		}
		cs, err := ssh.NewCertSigner(pk.(*ssh.Certificate), signer)
		if err != nil {
			die("%v", err)
		}
		signer = cs
	}
	auth := []ssh.AuthMethod{ssh.PublicKeys(signer)}
	if *totp != "" {
		auth = append(auth, ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
			out := make([]string, len(qs))
			for i := range qs {
				out[i] = *totp
			}
			return out, nil
		}))
	}
	cc := &ssh.ClientConfig{User: *user, Auth: auth, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second}
	nc, err := net.DialTimeout("tcp", *addr, 5*time.Second)
	if err != nil {
		die("dial: %v", err)
	}
	c, chans, reqs, err := ssh.NewClientConn(nc, *addr, cc)
	if err != nil {
		if *expectAuthFail {
			fmt.Println("AUTH-FAILED:", err)
			os.Exit(0)
		}
		die("AUTH-FAILED: %v", err)
	}
	if *expectAuthFail {
		die("authentication unexpectedly succeeded")
	}
	cl := ssh.NewClient(c, chans, reqs)
	defer cl.Close()
	if *dial != "" {
		if dc, err := cl.Dial("tcp", *dial); err != nil {
			fmt.Println("DIAL-REFUSED:", err)
		} else {
			dc.Close()
			fmt.Println("DIAL-ALLOWED")
		}
	}
	if *cmds != "" {
		for _, one := range strings.Split(*cmds, ";;") {
			cs, err := cl.NewSession()
			if err != nil {
				fmt.Println("SESSION-REFUSED:", err)
				continue
			}
			if *x11 {
				ok, _ := cs.SendRequest("x11-req", true, ssh.Marshal(struct {
					Single bool
					Proto  string
					Cookie string
					Screen uint32
				}{false, "MIT-MAGIC-COOKIE-1", "00", 0}))
				fmt.Println("X11-ACCEPTED:", ok)
			}
			var ob strings.Builder
			cs.Stdout, cs.Stderr = &lockedW{&ob}, &lockedW{&ob}
			if c, in, ok := strings.Cut(one, "<<<"); ok { // command<<<standard input
				one = c
				cs.Stdin = strings.NewReader(in)
			}
			if one == "SHELL" {
				err = cs.Shell()
				if err == nil {
					err = cs.Wait()
				}
			} else {
				err = cs.Run(one)
			}
			code := 0
			if ee, ok := err.(*ssh.ExitError); ok {
				code = ee.ExitStatus()
			} else if err != nil {
				code = -1
			}
			mu.Lock()
			fmt.Printf("CMD %q -> %s[exit=%d]\n", one, ob.String(), code)
			mu.Unlock()
		}
		return
	}
	if *dial != "" {
		return
	}
	s, err := cl.NewSession()
	if err != nil {
		die("session: %v", err)
	}
	if *x11 {
		ok, err := s.SendRequest("x11-req", true, ssh.Marshal(struct {
			Single bool
			Proto  string
			Cookie string
			Screen uint32
		}{false, "MIT-MAGIC-COOKIE-1", "00", 0}))
		fmt.Println("X11-ACCEPTED:", ok, err)
	}
	if *pty {
		if err := s.RequestPty("xterm", *rows, *cols, ssh.TerminalModes{ssh.ECHO: 1}); err != nil {
			fmt.Println("PTY-REFUSED:", err)
		}
	}
	stdin, _ := s.StdinPipe()
	stdout, _ := s.StdoutPipe()
	stderr, _ := s.StderrPipe()
	out := make(chan string, 1)
	go func() {
		var sb strings.Builder
		done := make(chan struct{}, 2)
		go func() { io.Copy(&lockedW{&sb}, stdout); done <- struct{}{} }()
		go func() { io.Copy(&lockedW{&sb}, stderr); done <- struct{}{} }()
		<-done
		<-done
		out <- sb.String()
	}()
	cmd := strings.Join(fs.Args(), " ")
	if cmd == "" {
		err = s.Shell()
	} else {
		err = s.Start(cmd)
	}
	if err != nil {
		die("start: %v", err)
	}
	waitc := make(chan error, 1)
	go func() { waitc <- s.Wait() }()
	if *script != "" {
		for _, step := range strings.Split(*script, ";;") {
			ms, q, _ := strings.Cut(step, ":")
			n, _ := strconv.Atoi(ms)
			time.Sleep(time.Duration(n) * time.Millisecond)
			txt, err := strconv.Unquote(q)
			if err != nil {
				die("script: %v", err)
			}
			if strings.HasPrefix(txt, "\x00resize") {
				var r, c2 int
				fmt.Sscanf(txt[len("\x00resize"):], "%d,%d", &r, &c2)
				_ = s.WindowChange(r, c2)
				continue
			}
			_, _ = io.WriteString(stdin, txt)
		}
	}
	var werr error
	select {
	case werr = <-waitc:
	case <-time.After(time.Duration(*tail) * time.Millisecond):
		cl.Close() // like a dropped connection
		werr = <-waitc
	}
	status := 0
	if ee, ok := werr.(*ssh.ExitError); ok {
		status = ee.ExitStatus()
	} else if werr != nil {
		status = -1
	}
	select {
	case o := <-out:
		fmt.Print(o)
	case <-time.After(2 * time.Second):
	}
	fmt.Printf("\n[exit=%d]\n", status)
}

var mu sync.Mutex

type lockedW struct{ sb *strings.Builder }

func (l *lockedW) Write(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return l.sb.Write(p) }
