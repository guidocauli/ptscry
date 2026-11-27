// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// client.go: the portable client library. It talks to the SSH front-end of a
// ptscry server (docs/MANUALE.md 3.5) from Linux, macOS or Windows, statically
// linked and without cgo. The command-line client (remote.go in the main
// package) and the graphical one (gui/) are both built on it.

package rclient

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// ProgName is the name of the remote command on the server.
const ProgName = "ptscry"

// DefaultPort is the port of the ptscry front-end.
const DefaultPort = "2222"

// Prompter asks the person a question (a terminal line, or a dialog) and
// returns the answer. secret = do not echo.
type Prompter func(question string, secret bool) (string, error)

// Target is where and as whom to connect.
type Target struct {
	User, Host, Port string
}

// Addr is host:port.
func (t Target) Addr() string { return net.JoinHostPort(t.Host, t.Port) }

// String is user@host:port.
func (t Target) String() string { return t.User + "@" + t.Addr() }

// ParseTarget reads [user@]host[:port] ("[::1]:22" for IPv6). A missing user is
// the current one; a missing port is DefaultPort.
func ParseTarget(s string) (Target, error) {
	var t Target
	if i := strings.LastIndex(s, "@"); i >= 0 {
		t.User, s = s[:i], s[i+1:]
	}
	if strings.HasPrefix(s, "[") || strings.Count(s, ":") == 1 {
		h, p, err := net.SplitHostPort(s)
		if err != nil {
			return t, fmt.Errorf("server %q: %v", s, err)
		}
		t.Host, t.Port = h, p
	} else {
		t.Host, t.Port = strings.Trim(s, "[]"), DefaultPort
	}
	if n, err := strconv.Atoi(t.Port); err != nil || n < 1 || n > 65535 || t.Host == "" {
		return t, fmt.Errorf("server %q: invalid host or port", s)
	}
	if t.User == "" {
		u, err := CurrentUser()
		if err != nil {
			return t, err
		}
		t.User = u
	}
	return t, nil
}

// CurrentUser is the local account name (without DOMAIN\ on Windows).
func CurrentUser() (string, error) {
	u, err := user.Current()
	if err != nil || u.Username == "" {
		return "", errors.New("cannot tell the user name: write user@host")
	}
	name := u.Username
	if i := strings.LastIndexAny(name, `\`); i >= 0 {
		name = name[i+1:]
	}
	return name, nil
}

// Cmdline checks the arguments against what the front-end accepts and joins
// them; it also says whether a terminal is needed and the session name when it
// is known (for reconnection).
func Cmdline(args []string) (line string, tty bool, name string, err error) {
	if len(args) == 0 {
		return "", true, "main", nil
	}
	for _, a := range args {
		for _, r := range a {
			ok := r == '_' || r == '.' || r == '/' || r == ':' || r == '=' || r == '@' || r == '+' || r == ',' || r == '-' ||
				(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
			if !ok {
				return "", false, "", fmt.Errorf("argument %q: only letters, digits and _ . / : = @ + , - are allowed over ssh", a)
			}
		}
	}
	switch args[0] {
	case "new", "attach", "run", "wait", "list", "status", "kill", "freeze", "thaw", "stop",
		"rename", "notify", "send-keys", "layout":
	default:
		return "", false, "", fmt.Errorf("%q is not available remotely (new, attach, run, wait, list, status, kill, freeze, thaw, stop, rename, notify, send-keys, layout)", args[0])
	}
	line = ProgName + " " + strings.Join(args, " ")
	switch args[0] {
	case "new":
		detached := false
		for i := 1; i < len(args) && strings.HasPrefix(args[i], "-"); i++ {
			if strings.Contains(args[i], "d") {
				detached = true
			}
			if args[i] == "-n" && i+1 < len(args) {
				name = args[i+1]
			}
		}
		return line, !detached, name, nil
	case "attach":
		var pos []string
		for _, a := range args[1:] {
			if !strings.HasPrefix(a, "-") {
				pos = append(pos, a)
			}
		}
		if len(pos) == 1 {
			name = pos[0]
		} else if len(pos) == 0 {
			name = "main"
		}
		return line, true, name, nil
	}
	return line, false, "", nil
}

// Auth builds the authentication methods: the key in the file identity, or the
// keys of the ssh-agent (also FIDO2 sk keys); with certFile the matching key is
// presented with its certificate. prompt is used for a key passphrase and for
// the TOTP code.
func Auth(identity, certFile string, prompt Prompter) ([]ssh.AuthMethod, error) {
	var signers []ssh.Signer
	if identity != "" {
		raw, err := readFile(identity)
		if err != nil {
			return nil, err
		}
		s, err := ssh.ParsePrivateKey(raw)
		var pm *ssh.PassphraseMissingError
		if errors.As(err, &pm) {
			pw, perr := prompt("Passphrase for "+identity+": ", true)
			if perr != nil {
				return nil, perr
			}
			s, err = ssh.ParsePrivateKeyWithPassphrase(raw, []byte(pw))
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %v", identity, err)
		}
		signers = []ssh.Signer{s}
	} else {
		// the agent first, then the usual key files of ~/.ssh (no agent on
		// Windows unless the OpenSSH Authentication Agent service runs)
		var agentErr error
		if ag, err := DialAgent(); err != nil {
			agentErr = err
		} else if ss, err := ag.Signers(); err != nil || len(ss) == 0 {
			agentErr = errors.New("the ssh-agent has no keys (ssh-add)")
		} else {
			signers = ss
		}
		if len(signers) == 0 {
			signers = defaultKeySigners(prompt)
		}
		if len(signers) == 0 {
			return nil, fmt.Errorf("no key: give an identity file, or start an ssh-agent with your key, or put one in ~/.ssh (%v)", agentErr)
		}
	}
	if certFile != "" {
		raw, err := readFile(certFile)
		if err != nil {
			return nil, err
		}
		pk, _, _, _, err := ssh.ParseAuthorizedKey(raw)
		cert, ok := pk.(*ssh.Certificate)
		if err != nil || !ok {
			return nil, fmt.Errorf("%s: not an SSH certificate", certFile)
		}
		var cs []ssh.Signer
		for _, s := range signers {
			if string(s.PublicKey().Marshal()) == string(cert.Key.Marshal()) {
				c, err := ssh.NewCertSigner(cert, s)
				if err != nil {
					return nil, err
				}
				cs = append(cs, c)
			}
		}
		if len(cs) == 0 {
			return nil, fmt.Errorf("%s: no key matches this certificate", certFile)
		}
		signers = cs
	}
	return []ssh.AuthMethod{
		ssh.PublicKeys(signers...),
		ssh.KeyboardInteractive(func(_, instr string, qs []string, _ []bool) ([]string, error) {
			ans := make([]string, len(qs))
			for i, q := range qs {
				if instr != "" {
					q = instr + "\n" + q
				}
				a, err := prompt(q, true)
				if err != nil {
					return nil, err
				}
				ans[i] = strings.TrimSpace(a)
			}
			return ans, nil
		}),
	}, nil
}

// Connect dials, verifies the server key and authenticates. version is the
// program's version (announced in the SSH banner).
func Connect(t Target, auth []ssh.AuthMethod, pinned, version string, prompt Prompter) (*ssh.Client, error) {
	return ConnectVia(nil, t, auth, pinned, version, prompt)
}

// ConnectVia is Connect through an already connected jump host (as ssh -J), or
// directly when jump is nil.
func ConnectVia(jump *ssh.Client, t Target, auth []ssh.AuthMethod, pinned, version string, prompt Prompter) (*ssh.Client, error) {
	var nc net.Conn
	var err error
	if jump != nil {
		nc, err = jump.Dial("tcp", t.Addr())
	} else {
		nc, err = net.DialTimeout("tcp", t.Addr(), 10*time.Second)
	}
	if err != nil {
		return nil, err
	}
	hostname := t.Addr()
	conf := &ssh.ClientConfig{User: t.User, Auth: auth, HostKeyCallback: HostKeyChecker(pinned, prompt),
		ClientVersion: "SSH-2.0-ptscry_" + version, Timeout: 60 * time.Second}
	_ = nc.SetDeadline(time.Now().Add(120 * time.Second)) // includes typing the TOTP code
	c, chans, reqs, err := ssh.NewClientConn(nc, hostname, conf)
	if err != nil {
		nc.Close()
		return nil, err
	}
	_ = nc.SetDeadline(time.Time{})
	cl := ssh.NewClient(c, chans, reqs)
	go func() { // detect a dead path sooner than TCP does
		miss := 0
		for {
			time.Sleep(15 * time.Second)
			done := make(chan error, 1)
			go func() { _, _, err := c.SendRequest("keepalive@openssh.com", true, nil); done <- err }()
			select {
			case err := <-done:
				if err != nil {
					return
				}
				miss = 0
			case <-time.After(10 * time.Second):
				if miss++; miss >= 3 {
					c.Close()
					return
				}
			}
		}
	}()
	return cl, nil
}

// Result is how a remote command ended.
type Result struct {
	Code int
	Lost bool // the connection went away without an exit status
}

func result(werr error) Result {
	var ee *ssh.ExitError
	switch {
	case werr == nil:
		return Result{}
	case errors.As(werr, &ee):
		return Result{Code: ee.ExitStatus()}
	}
	return Result{Code: 255, Lost: true}
}

// Run runs a command that needs no terminal (list, status, kill...), with its
// output on stdout and stderr.
func Run(cl *ssh.Client, line string, stdout, stderr io.Writer) (Result, error) {
	s, err := cl.NewSession()
	if err != nil {
		return Result{}, err
	}
	defer s.Close()
	s.Stdout, s.Stderr = stdout, stderr
	if err := s.Start(line); err != nil {
		return Result{}, err
	}
	return result(s.Wait()), nil
}

// Term is a command running on a remote terminal (a session).
type Term struct {
	s      *ssh.Session
	In     io.WriteCloser // keyboard
	Out    io.Reader      // screen (stdout and stderr are one stream on a terminal)
	closed sync.Once
}

// OpenTerm starts line on a new remote terminal of the given size; an empty
// line starts the user's shell session ("no command = new -A -n main" for
// the front-end). prefix, if not 0, is the detach key the client announces.
func OpenTerm(cl *ssh.Client, line string, rows, cols int, termType string, prefix byte) (*Term, error) {
	s, err := cl.NewSession()
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Term, error) { s.Close(); return nil, err }
	if termType == "" || len(termType) > 64 {
		termType = "xterm-256color"
	}
	if err := s.RequestPty(termType, rows, cols, ssh.TerminalModes{}); err != nil {
		return fail(fmt.Errorf("the server refused the terminal: %v", err))
	}
	if prefix != 0 {
		_, _ = s.SendRequest("ptscry-prefix", true, []byte{prefix})
	}
	in, err := s.StdinPipe()
	if err != nil {
		return fail(err)
	}
	so, err := s.StdoutPipe()
	if err != nil {
		return fail(err)
	}
	se, err := s.StderrPipe()
	if err != nil {
		return fail(err)
	}
	// The server writes some messages (such as "[detached]") on stderr: one
	// screen, one stream.
	pr, pw := io.Pipe()
	var wg sync.WaitGroup
	for _, r := range []io.Reader{so, se} {
		wg.Add(1)
		go func(r io.Reader) { defer wg.Done(); _, _ = io.Copy(pw, r) }(r)
	}
	go func() { wg.Wait(); pw.Close() }()
	if line == "" {
		err = s.Shell()
	} else {
		err = s.Start(line)
	}
	if err != nil {
		return fail(err)
	}
	return &Term{s: s, In: in, Out: pr}, nil
}

// Resize tells the server the new size of the window.
func (t *Term) Resize(rows, cols int) error { return t.s.WindowChange(rows, cols) }

// Wait returns when the command ends or the connection is lost.
func (t *Term) Wait() Result { return result(t.s.Wait()) }

// Close ends the terminal; the session stays alive on the server (it detaches).
func (t *Term) Close() { t.closed.Do(func() { t.s.Close() }) }

// readFile reads a key or certificate file: a regular file of at most 1 MiB
// (a path from an imported configuration could name /dev/zero or a fifo).
func readFile(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", p)
	}
	b, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 1<<20 {
		return nil, fmt.Errorf("%s: too large for a key", p)
	}
	return b, nil
}

// defaultKeySigners loads the keys ssh itself would try: ~/.ssh/id_ed25519,
// id_ecdsa, id_rsa. An encrypted key asks for its passphrase; a wrong or empty
// one skips that key.
func defaultKeySigners(prompt Prompter) []ssh.Signer {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	var out []ssh.Signer
	for _, n := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
		path := filepath.Join(home, ".ssh", n)
		raw, err := readFile(path)
		if err != nil {
			continue
		}
		s, err := ssh.ParsePrivateKey(raw)
		var pm *ssh.PassphraseMissingError
		if errors.As(err, &pm) && prompt != nil {
			pw, perr := prompt("Passphrase for "+path+": ", true)
			if perr != nil || pw == "" {
				continue
			}
			s, err = ssh.ParsePrivateKeyWithPassphrase(raw, []byte(pw))
		}
		if err == nil {
			out = append(out, s)
		}
	}
	return out
}

// PasswordAuth asks for the password of user@host when the server wants one
// (plain SSH servers; ptscry's front-end never does).
func PasswordAuth(t Target, prompt Prompter) ssh.AuthMethod {
	return ssh.PasswordCallback(func() (string, error) {
		return prompt("Password di "+t.User+"@"+t.Host+": ", true)
	})
}

// InteractiveAuth answers keyboard-interactive questions (passwords, OTP codes)
// through prompt.
func InteractiveAuth(prompt Prompter) ssh.AuthMethod {
	return ssh.KeyboardInteractive(func(_, instr string, qs []string, _ []bool) ([]string, error) {
		ans := make([]string, len(qs))
		for i, q := range qs {
			if instr != "" {
				q = instr + "\n" + q
			}
			a, err := prompt(q, true)
			if err != nil {
				return nil, err
			}
			ans[i] = strings.TrimRight(a, "\r\n") // a password may begin or end with a space
		}
		return ans, nil
	})
}
