//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// client.go: client side (new, attach, list, kill, stop).

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var errNoHost = errors.New("no ptscry host is running for this user")

func spawnHost(dir string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(dir, "host.log"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	devnull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer devnull.Close()
	cmd := exec.Command(exe, "host")
	cmd.Env = os.Environ()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// standalone reports whether the unprivileged, user-run host is requested
// (development only). The default is the root monitor.
func standalone() bool { return os.Getenv("PTSCRY_STANDALONE") == "1" }

func dialMonitor(cfg *config) (*framed, error) {
	c, err := net.DialTimeout("unix", cfg.MonitorSocket, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the ptscry monitor at %s: %w", cfg.MonitorSocket, err)
	}
	// The monitor is root. Anything else answering on this socket (a path in a
	// directory someone else controls, say) could read what the user types.
	if uc, ok := c.(*net.UnixConn); ok {
		cred, err := peerCred(uc)
		if err != nil || cred.Uid != 0 {
			c.Close()
			return nil, fmt.Errorf("refusing to talk to %s: it is not served by root", cfg.MonitorSocket)
		}
	}
	return newFramed(c), nil
}

func dialHost(cfg *config, uid int, autostart bool) (*framed, error) {
	if !standalone() {
		return dialMonitor(cfg)
	}
	dir := resolveSocketDir(cfg, uid)
	sock := filepath.Join(dir, "host.sock")
	try := func() (*framed, error) {
		c, err := net.DialTimeout("unix", sock, 2*time.Second)
		if err != nil {
			return nil, err
		}
		return newFramed(c), nil
	}
	fc, err := try()
	if err == nil {
		return fc, nil
	}
	if !autostart {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			return nil, errNoHost
		}
		return nil, err
	}
	if err := ensureSecureDir(dir, uid); err != nil {
		return nil, err
	}
	if err := spawnHost(dir); err != nil {
		return nil, fmt.Errorf("starting host: %w", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fc, err := try(); err == nil {
			return fc, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("host did not start in time (see %s)", filepath.Join(dir, "host.log"))
}

// request sends a hello and waits for the host's verdict.
func request(fc *framed, hl hello) (okReply, error) {
	var r okReply
	b, _ := json.Marshal(hl)
	if err := fc.write(msgHello, b); err != nil {
		return r, err
	}
	t, p, err := fc.read()
	if err != nil {
		return r, fmt.Errorf("host closed the connection: %w", err)
	}
	switch t {
	case msgOK:
		return r, json.Unmarshal(p, &r)
	case msgErr:
		return r, errors.New(string(p))
	}
	return r, fmt.Errorf("unexpected reply type %d", t)
}

// connect dials (starting the host if needed) and sends hl; it retries once
// when the host was caught while shutting down.
func connect(hl hello, autostart bool) (*framed, okReply, error) {
	cfg, _, err := loadEnv()
	if err != nil {
		return nil, okReply{}, err
	}
	uid := os.Getuid()
	for attempt := 0; ; attempt++ {
		fc, err := dialHost(&cfg, uid, autostart)
		if err != nil {
			return nil, okReply{}, err
		}
		r, err := request(fc, hl)
		if err == nil {
			return fc, r, nil
		}
		fc.c.Close()
		msg := err.Error()
		if attempt < 3 && (strings.Contains(msg, errHostClosingMsg) || strings.Contains(msg, "host unavailable")) {
			time.Sleep(300 * time.Millisecond)
			continue
		}
		return nil, okReply{}, err
	}
}

func clientEnv() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if envAllowed(k) {
			env[k] = v
		}
	}
	return env
}

func termSize() (rows, cols uint16) {
	if r, c, err := getWinsize(os.Stdout); err == nil && r > 0 && c > 0 {
		return r, c
	}
	return 24, 80
}

// ---------------------------------------------------------------- commands

func cmdNew(args []string) error {
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	name := fs.String("n", "", "session name (default: automatic sN)")
	detached := fs.Bool("d", false, "create the session without attaching")
	attachIfExists := fs.Bool("A", false, "attach if the named session already exists")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rows, cols := termSize()
	hl := hello{Op: "new", Name: *name, Cmd: fs.Args(), Rows: rows, Cols: cols,
		Env: clientEnv(), Create: *attachIfExists, Detached: *detached}
	if !*detached && !(isTerminal(os.Stdin) && isTerminal(os.Stdout)) {
		return errors.New("a terminal is required (use -d to create without attaching)")
	}
	fc, r, err := connect(hl, true)
	if err != nil {
		return err
	}
	if *detached {
		fc.c.Close()
		fmt.Println(r.Session.Name)
		return nil
	}
	return runAttach(fc, r, false)
}

func cmdAttach(args []string) error {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	ro := fs.Bool("r", false, "read-only (watch the session, send no input)")
	create := fs.Bool("c", false, "create the session if it does not exist")
	lay := fs.String("l", "", "open a saved layout in panes (ptscry-tui)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *lay != "" {
		return cmdTUI(append([]string{"-l", *lay}, fs.Args()...))
	}
	if fs.NArg() > 1 {
		return errors.New("usage: ptscry attach [-r] [-c] [name]")
	}
	if !(isTerminal(os.Stdin) && isTerminal(os.Stdout)) {
		return errors.New("a terminal is required")
	}
	rows, cols := termSize()
	hl := hello{Op: "attach", Name: fs.Arg(0), Rows: rows, Cols: cols, Env: clientEnv(),
		ReadOnly: *ro, Create: *create}
	fc, r, err := connect(hl, *create)
	if err != nil {
		if errors.Is(err, errNoHost) {
			return errors.New("no such session: no sessions exist")
		}
		return err
	}
	return runAttach(fc, r, *ro)
}

func cmdKill(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: ptscry kill <name>")
	}
	fc, r, err := connect(hello{Op: "kill", Name: args[0]}, false)
	if err != nil {
		if errors.Is(err, errNoHost) {
			return fmt.Errorf("no such session: %q", args[0])
		}
		return err
	}
	fc.c.Close()
	fmt.Println(r.Message)
	return nil
}

// cmdFreeze / cmdThaw stop and resume every process of a session.
func cmdFreeze(args []string) error { return cmdFreezeThaw("freeze", args) }
func cmdThaw(args []string) error   { return cmdFreezeThaw("thaw", args) }

func cmdFreezeThaw(op string, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: ptscry %s <name>", op)
	}
	fc, r, err := connect(hello{Op: op, Name: args[0]}, false)
	if err != nil {
		if errors.Is(err, errNoHost) {
			return fmt.Errorf("no such session: %q", args[0])
		}
		return err
	}
	fc.c.Close()
	fmt.Println(r.Message)
	return nil
}

func cmdStop(args []string) error {
	fc, r, err := connect(hello{Op: "stop"}, false)
	if err != nil {
		if errors.Is(err, errNoHost) {
			fmt.Println("no host running")
			return nil
		}
		return err
	}
	fc.c.Close()
	fmt.Println(r.Message)
	return nil
}

func cmdConfig(args []string) error {
	cfg, u, err := loadEnv()
	if err != nil {
		return err
	}
	uid := os.Getuid()
	fmt.Printf("# %s\n", versionLine())
	fmt.Printf("config file          %s\n", configPath())
	fmt.Printf("user                 %s (uid %d)\n", u.Username, uid)
	fmt.Printf("session_lifecycle    %s\n", cfg.Lifecycle)
	fmt.Printf("idle_timeout         %s\n", cfg.IdleTimeout)
	if cfg.MaxSessions == 0 {
		fmt.Printf("max_sessions         unlimited\n")
	} else {
		fmt.Printf("max_sessions         %d\n", cfg.MaxSessions)
	}
	fmt.Printf("max_lifetime         %s\n", cfg.MaxLifetime)
	fmt.Printf("enabled              %v\n", cfg.Enabled)
	fmt.Printf("access_group         %q\n", cfg.AccessGroup)
	fmt.Printf("allow_root           %v\n", cfg.AllowRoot)
	fmt.Printf("mode                 %s\n", map[bool]string{true: "standalone (PTSCRY_STANDALONE=1)", false: "managed by the monitor at " + cfg.MonitorSocket}[standalone()])
	fmt.Printf("replay_bytes         %d\n", cfg.ReplayBytes)
	fmt.Printf("allow_command        %v\n", cfg.AllowCommand)
	fmt.Printf("record               %s\n", cfg.Record)
	fmt.Printf("socket               %s\n", filepath.Join(resolveSocketDir(&cfg, uid), "host.sock"))
	ap := cfg.AuditLog
	if ap == "" {
		ap = defaultAuditPath(uid, u.HomeDir)
	}
	fmt.Printf("audit_log            %s\n", ap)
	fmt.Printf("audit_syslog         %v\n", cfg.AuditSyslog)
	return nil
}

// ------------------------------------------------------------------ attach

type attachResult struct {
	kind string // exit | bye | lost
	code int
	msg  string
}

func runAttach(fc *framed, r okReply, readOnly bool) error {
	name := ""
	if r.Session != nil {
		name = r.Session.Name
	}
	prefix := clientPrefix(r.Prefix) // before raw mode: it may print a warning
	restore, err := makeRaw(os.Stdin)
	if err != nil {
		fc.c.Close()
		return err
	}
	restored := false
	doRestore := func() {
		if !restored {
			restored = true
			restore()
		}
	}
	defer doRestore()

	// keyboard
	keys := make(chan []byte, 8)
	go func() {
		defer close(keys)
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				keys <- append([]byte(nil), buf[:n]...)
			}
			if err != nil {
				return
			}
		}
	}()

	// window size changes
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	resize := make(chan [2]uint16, 4)
	go func() {
		for range winch {
			r, c := termSize()
			select {
			case resize <- [2]uint16{r, c}:
			default:
			}
		}
	}()

	ts := &termSide{keys: keys, out: os.Stdout, resize: resize, size: termSize,
		dial: func(hl hello) (*framed, okReply, error) {
			return connect(hl, hl.Op == "new")
		},
		prefix: prefix, noPrefix: os.Getenv("PTSCRY_NO_PREFIX") == "1", readOnly: readOnly, env: clientEnv()}
	lr := runAttachLoop(ts, fc, r)
	name = lr.name
	res := attachResult{kind: lr.kind, code: lr.code, msg: lr.msg}
	if res.kind == "gone" {
		res = attachResult{kind: "bye", msg: "detached (end of input)"}
	}
	doRestore()

	switch res.kind {
	case "exit":
		fmt.Fprintf(os.Stderr, "[session %s exited with status %d]\n", name, res.code)
		if res.code != 0 {
			return &exitStatus{res.code}
		}
		return nil
	case "bye":
		fmt.Fprintf(os.Stderr, "[%s (session %s)]\n", res.msg, name)
		return nil
	}
	fmt.Fprintf(os.Stderr, "[connection to host lost (session %s still running if the host is alive)]\n", name)
	return &exitStatus{1}
}
