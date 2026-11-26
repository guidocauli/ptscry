//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// host.go: per-user session host. Owns the PTYs of one user and serves
// new/attach/list/kill/stop requests.
//
// Two modes:
//   managed     started by the monitor (root) with a private control channel
//               (see ctl.go, host_managed.go). No socket of its own: clients
//               are handed over by the monitor. Policy, audit and recordings
//               all go through the monitor. This is the production mode.
//   standalone  `ptscry host` run by the user, listening on a private Unix
//               socket. Audit and recordings are written by the user's own
//               process, so they are NOT tamper-resistant. For development
//               and unprivileged use only.
//
// Lifecycle (session_lifecycle in the configuration):
//   exit_on_last_shell   the host exits when its last session ends
//   persistent_per_user  the host stays up until stopped (idle_timeout still
//                        reaps detached idle sessions)

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// nameRe: a session name. Not starting with "-": it would read as an option
// on a command line ("new -A -n -d").
var nameRe = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._-]{0,63}$`)

const errHostClosingMsg = "host is shutting down"

type peerInfo struct {
	uid uint32
	pid int32
	req uint64 // monitor request id (managed mode)
}

func (p peerInfo) fields(extra fields) fields {
	f := fields{"peer_pid": p.pid}
	if p.req != 0 {
		f["req"] = p.req
	}
	for k, v := range extra {
		f[k] = v
	}
	return f
}

type host struct {
	active   atomic.Int32 // client connections being served
	cfg      config
	uid      int
	username string
	home     string
	sockPath string
	audit    auditor
	ln       *net.UnixListener // standalone mode only
	ctl      *ctlConn          // managed mode only

	recMu   sync.Mutex
	recAcks map[string]chan recAck

	launchMu sync.Mutex
	spawns   map[string]chan spawnResult // waiting for the monitor's answer
	exits    map[string]chan int         // exit status of launcher-run sessions

	mu         sync.Mutex
	sessions   map[string]*session
	results    map[string]*jobResult // finished detached jobs (run -d)
	closing    bool
	emptySince time.Time // when the host last had zero sessions
	once       sync.Once
	done       chan struct{}
}

// emptyGrace is how long an exit_on_last_shell host may stay up with no
// sessions at all (e.g. the first create request failed or the client died).
const emptyGrace = 5 * time.Second

func cmdHost(args []string) error {
	if os.Getenv("PTSCRY_CTL_FD") != "" {
		return runManagedHost()
	}
	cfg, u, err := loadEnv()
	if err != nil {
		return err
	}
	uid := os.Getuid()
	dir := resolveSocketDir(&cfg, uid)
	if err := ensureSecureDir(dir, uid); err != nil {
		return err
	}
	sockPath := filepath.Join(dir, "host.sock")
	if c, err := net.DialTimeout("unix", sockPath, time.Second); err == nil {
		c.Close()
		return errors.New("a host is already running for this user")
	}
	a, err := openAudit(&cfg, u.Username, uid, u.HomeDir)
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	_ = os.Remove(sockPath) // stale socket
	old := syscall.Umask(0o177)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sockPath, Net: "unix"})
	syscall.Umask(old)
	if err != nil {
		a.close()
		return err
	}
	ln.SetUnlinkOnClose(true)

	h := &host{
		cfg: cfg, uid: uid, username: u.Username, home: u.HomeDir,
		sockPath: sockPath, audit: a, ln: ln,
		sessions: map[string]*session{}, done: make(chan struct{}),
		emptySince: time.Now(),
	}
	a.log("host_start", fields{"pid": os.Getpid(), "lifecycle": cfg.Lifecycle,
		"socket": sockPath, "version": progVersion, "mode": "standalone"})

	signal.Ignore(syscall.SIGHUP)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		s := <-sig
		h.shutdown("signal " + s.String())
	}()
	go h.janitor()
	go h.acceptLoop()

	<-h.done
	h.drainClients()
	a.log("host_stop", fields{"pid": os.Getpid()})
	a.close()
	return nil
}

func (h *host) acceptLoop() {
	for {
		c, err := h.ln.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			h.audit.log("accept_error", fields{"error": err.Error()})
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go h.handle(c)
	}
}

func peerCred(c *net.UnixConn) (*syscall.Ucred, error) {
	rc, err := c.SyscallConn()
	if err != nil {
		return nil, err
	}
	var cred *syscall.Ucred
	var cerr error
	if err := rc.Control(func(fd uintptr) {
		cred, cerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return nil, err
	}
	return cred, cerr
}

func replyOK(fc *framed, r okReply) error {
	b, _ := json.Marshal(r)
	return fc.write(msgOK, b)
}

func replyErr(fc *framed, msg string) {
	_ = fc.write(msgErr, []byte(msg))
}

// handle serves one standalone-mode connection.
func (h *host) handle(uc *net.UnixConn) {
	defer uc.Close()
	cred, err := peerCred(uc)
	if err != nil {
		return
	}
	// The socket is 0600 in a 0700 directory, but verify anyway.
	if int(cred.Uid) != h.uid && cred.Uid != 0 {
		h.audit.log("connect_denied", fields{"peer_uid": cred.Uid, "peer_pid": cred.Pid})
		return
	}
	fc := newFramed(uc)
	_ = uc.SetReadDeadline(time.Now().Add(5 * time.Second))
	t, p, err := fc.read()
	_ = uc.SetReadDeadline(time.Time{})
	if err != nil || t != msgHello {
		return
	}
	var hl hello
	if err := json.Unmarshal(p, &hl); err != nil {
		replyErr(fc, "malformed request")
		return
	}
	h.dispatch(fc, &hl, peerInfo{uid: cred.Uid, pid: cred.Pid})
}

// dispatch executes one already-parsed request on fc.
func (h *host) dispatch(fc *framed, hl *hello, peer peerInfo) {
	h.active.Add(1)
	defer h.active.Add(-1)
	if h.opsDispatch(fc, hl, peer) {
		return
	}
	switch hl.Op {
	case "list":
		_ = replyOK(fc, okReply{Sessions: h.list(), Jobs: h.listJobs()})

	case "kill":
		s := h.get(hl.Name)
		if s == nil && h.dropResult(hl.Name) { // kill on a finished job discards its result
			h.audit.log("job_discard", peer.fields(fields{"session": hl.Name}))
			_ = replyOK(fc, okReply{Message: "discarded " + hl.Name})
			return
		}
		if s == nil {
			replyErr(fc, fmt.Sprintf("no such session: %q", hl.Name))
			return
		}
		h.audit.log("kill", peer.fields(fields{"session": s.Name()}))
		s.kill("killed by client")
		_ = replyOK(fc, okReply{Message: "killed " + s.Name()})

	case "freeze", "thaw":
		s := h.get(hl.Name)
		if s == nil {
			replyErr(fc, fmt.Sprintf("no such session: %q", hl.Name))
			return
		}
		if hl.Op == "freeze" {
			if err := s.freeze(); err != nil {
				replyErr(fc, err.Error())
				return
			}
			h.audit.log("freeze", peer.fields(fields{"session": s.Name()}))
			_ = replyOK(fc, okReply{Message: "frozen " + s.Name()})
		} else {
			s.thaw()
			h.audit.log("thaw", peer.fields(fields{"session": s.Name()}))
			_ = replyOK(fc, okReply{Message: "thawed " + s.Name()})
		}

	case "stop":
		h.audit.log("stop_requested", peer.fields(nil))
		_ = replyOK(fc, okReply{Message: "host stopping"})
		go h.shutdown("stop requested")

	case "new":
		if hl.Name != "" && hl.Create {
			if s := h.get(hl.Name); s != nil {
				h.serveAttach(fc, s, hl, peer, false)
				return
			}
		}
		s, err := h.createSession(hl.Name, hl, peer)
		h.newResult(peer, err)
		if err != nil {
			replyErr(fc, err.Error())
			return
		}
		if hl.Detached {
			info := s.info()
			_ = replyOK(fc, okReply{Session: &info, Created: true})
			return
		}
		h.serveAttach(fc, s, hl, peer, true)

	case "attach":
		s, err := h.pick(hl.Name)
		if err != nil {
			if hl.Name != "" {
				if r := h.getResult(hl.Name); r != nil {
					h.serveResult(fc, r, hl, peer)
					return
				}
			}
			if hl.Create && hl.Name != "" && strings.HasPrefix(err.Error(), "no such session") {
				s, err = h.createSession(hl.Name, hl, peer)
				h.newResult(peer, err)
				if err != nil {
					replyErr(fc, err.Error())
					return
				}
				h.serveAttach(fc, s, hl, peer, true)
				return
			}
			replyErr(fc, err.Error())
			return
		}
		h.serveAttach(fc, s, hl, peer, false)

	default:
		replyErr(fc, fmt.Sprintf("unknown operation %q", hl.Op))
	}
}

// newResult tells the monitor whether a creation attempt succeeded, so it
// can release the session-limit reservation it made for the request.
func (h *host) newResult(peer peerInfo, err error) {
	if h.ctl == nil || peer.req == 0 {
		return
	}
	m := map[string]any{"req": peer.req, "ok": err == nil}
	if err != nil {
		m["error"] = err.Error()
	}
	b, _ := json.Marshal(m)
	_ = h.ctl.send(ctlNewResult, b)
}

func (h *host) get(name string) *session {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessions[name]
}

// pick resolves a session name; with an empty name it succeeds only when
// exactly one session exists.
func (h *host) pick(name string) (*session, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if name != "" {
		if s := h.sessions[name]; s != nil {
			return s, nil
		}
		return nil, fmt.Errorf("no such session: %q", name)
	}
	switch len(h.sessions) {
	case 0:
		return nil, errors.New("no such session: none exist")
	case 1:
		for _, s := range h.sessions {
			return s, nil
		}
	}
	names := make([]string, 0, len(h.sessions))
	for n := range h.sessions {
		names = append(names, n)
	}
	sort.Strings(names)
	return nil, fmt.Errorf("several sessions exist, pick one: %s", strings.Join(names, ", "))
}

func (h *host) list() []sessionInfo {
	h.mu.Lock()
	ss := make([]*session, 0, len(h.sessions))
	for _, s := range h.sessions {
		ss = append(ss, s)
	}
	h.mu.Unlock()
	out := make([]sessionInfo, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// maxClients is how many clients may be attached to one session at a time.
const maxClients = 32

func (h *host) serveAttach(fc *framed, s *session, hl *hello, peer peerInfo, created bool) {
	if s.info().Attached >= maxClients {
		replyErr(fc, "too many clients attached to this session")
		return
	}
	c := newClient(fc, hl.ReadOnly)
	info := s.info()
	if err := replyOK(fc, okReply{Session: &info, Created: created, Prefix: h.cfg.DetachPrefix}); err != nil {
		return
	}
	go c.writer()
	s.attachClient(c, hl.Rows, hl.Cols)
	h.audit.log("attach", peer.fields(fields{"session": s.Name(),
		"readonly": hl.ReadOnly, "clients": s.info().Attached}))

	reason := "connection lost"
	var lastRedraw, lastTake time.Time
loop:
	for {
		t, p, err := fc.read()
		if err != nil {
			break
		}
		switch t {
		case msgData:
			if !c.readOnly && s.mayType(c) {
				s.write(p)
			}
		case msgResize:
			if r, cl, ok := decodeResize(p); ok && !c.readOnly && s.mayType(c) {
				s.resize(r, cl)
			}
		case msgTake:
			if !c.readOnly && len(p) == 1 && p[0] <= 2 && time.Since(lastTake) > 300*time.Millisecond {
				lastTake = time.Now()
				on := p[0] == 1
				if p[0] == 2 { // toggle
					s.mu.Lock()
					on = s.writer != c
					s.mu.Unlock()
				}
				s.take(c, on, peer)
			}
		case msgRedraw:
			// after a notice drawn over a full-screen program; at most twice a second
			if !c.readOnly && time.Since(lastRedraw) > 500*time.Millisecond {
				lastRedraw = time.Now()
				s.redraw()
			}
		case msgWatch:
			c.setWatch(p)
		case msgDetach:
			reason = "detached"
			break loop
		}
	}
	s.take(c, false, peer) // a writer that leaves gives the keyboard back
	s.detachClient(c)
	wasFinished := c.isFinished()
	c.finish(outMsg{msgBye, []byte(reason)})
	<-c.wdone
	if !wasFinished {
		h.audit.log("detach", peer.fields(fields{"session": s.Name(), "reason": reason}))
	}
}

func (h *host) shell() string {
	if h.cfg.Shell != "" {
		return h.cfg.Shell
	}
	if sh := os.Getenv("SHELL"); strings.HasPrefix(sh, "/") {
		return sh
	}
	if _, err := os.Stat("/bin/bash"); err == nil {
		return "/bin/bash"
	}
	return "/bin/sh"
}

func envAllowed(k string) bool {
	return k == "TERM" || k == "COLORTERM" || k == "LANG" || k == "LANGUAGE" || strings.HasPrefix(k, "LC_")
}

func (h *host) childEnv(name string, cli map[string]string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "TERM" || strings.HasPrefix(k, "PTSCRY") {
			continue
		}
		env = append(env, kv)
	}
	term := "xterm-256color"
	for k, v := range cli {
		if !envAllowed(k) || len(v) > 256 || strings.ContainsRune(v, 0) {
			continue
		}
		if k == "TERM" {
			term = v
			continue
		}
		env = append(env, k+"="+v)
	}
	return append(env, "TERM="+term, "PTSCRY=1", "PTSCRY_SESSION="+name)
}

func (h *host) autoName() string {
	for i := 1; ; i++ {
		n := "s" + strconv.Itoa(i)
		if !h.nameTaken(n) {
			return n
		}
	}
}

// nameTaken tells whether a name is in use: by a session, or as the creation
// name of a session renamed since (the monitor and the launcher still know
// it by that). The caller holds h.mu.
func (h *host) nameTaken(n string) bool {
	if _, ok := h.sessions[n]; ok {
		return true
	}
	for _, s := range h.sessions {
		if s.key == n {
			return true
		}
	}
	return false
}

// endSession forgets an ended session (by its current name, if it is still
// that session) and its launch record (by its creation name).
func (h *host) endSession(s *session) { h.dropSessionAs(s, "", s.key) }

// rename gives a session a new name.
func (h *host) rename(old, nw string) (*session, error) {
	if !nameRe.MatchString(nw) {
		return nil, errors.New("invalid session name (allowed: letters, digits, . _ -; max 64)")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.sessions[old]
	if s == nil {
		return nil, fmt.Errorf("no such session: %q", old)
	}
	if nw == old {
		return s, nil
	}
	if h.nameTaken(nw) && s.key != nw {
		return nil, fmt.Errorf("session %q already exists", nw)
	}
	if _, ok := h.sessions[nw]; ok {
		return nil, fmt.Errorf("session %q already exists", nw)
	}
	if _, ok := h.results[nw]; ok {
		return nil, fmt.Errorf("a finished job is called %q (collect or kill it first)", nw)
	}
	delete(h.sessions, old)
	h.sessions[nw] = s
	s.setName(nw)
	return s, nil
}

// openRecorder opens the recording sink for a new session: a local file in
// standalone mode, a stream to the monitor in managed mode.
func (h *host) openRecorder(name, term string, rows, cols uint16) (recSink, error) {
	if h.ctl != nil {
		return h.openCtlRecorder(name, h.cfg.Record, term, rows, cols)
	}
	dir := h.cfg.RecordDir
	if dir == "" {
		dir = defaultRecordDir(h.uid, h.home)
	}
	return newRecorder(dir, name, h.cfg.Record, term, rows, cols, h.cfg.RecordMaxBytes)
}

func (h *host) createSession(name string, hl *hello, peer peerInfo) (*session, error) {
	argv := hl.Cmd
	if len(argv) > 0 && !h.cfg.AllowCommand {
		return nil, errors.New("running arbitrary commands is disabled by configuration")
	}
	if len(argv) == 0 {
		argv = []string{h.shell()}
	}
	rows, cols := hl.Rows, hl.Cols
	if rows == 0 || cols == 0 {
		rows, cols = 24, 80
	}

	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		return nil, errors.New(errHostClosingMsg)
	}
	if name == "" {
		name = h.autoName()
	} else if !nameRe.MatchString(name) {
		h.mu.Unlock()
		return nil, errors.New("invalid session name (allowed: letters, digits, . _ -; max 64)")
	}
	if h.nameTaken(name) {
		h.mu.Unlock()
		return nil, fmt.Errorf("session %q already exists", name)
	}
	delete(h.results, name) // a new session replaces an old job result of the same name
	if h.cfg.MaxSessions > 0 && len(h.sessions) >= h.cfg.MaxSessions {
		h.mu.Unlock()
		return nil, fmt.Errorf("session limit reached (%d)", h.cfg.MaxSessions)
	}
	s := &session{
		h: h, key: name, name: name, cmdline: joinCmd(argv), created: time.Now(), last: time.Now(),
		clients: map[*hclient]struct{}{}, rows: rows, cols: cols,
		pumpDone: make(chan struct{}), exited: make(chan struct{}),
	}
	s.job = hl.Job && h.cfg.JobRetention > 0
	if h.cfg.ReplayBytes > 0 {
		s.ring = newRing(h.cfg.ReplayBytes)
	}
	h.sessions[name] = s // reserve the name
	h.mu.Unlock()

	fail := func(err error) (*session, error) {
		h.endSession(s) // it may have been renamed meanwhile
		return nil, err
	}

	// Open the recorder before anything runs: a session that must be
	// recorded never starts unrecorded.
	var rec recSink
	if h.cfg.Record != "off" {
		term := hl.Env["TERM"]
		if term == "" {
			term = "xterm-256color"
		}
		var err error
		if rec, err = h.openRecorder(name, term, rows, cols); err != nil {
			return fail(fmt.Errorf("recording: %w", err))
		}
	}
	failRec := func(err error) (*session, error) {
		if rec != nil {
			rec.close()
		}
		return fail(err)
	}

	if h.useLauncher() {
		master, pid, exitCh, err := h.launch(name, argv, hl.Env, rows, cols)
		if err != nil {
			return failRec(err)
		}
		s.ptmx, s.pid, s.rec, s.exitCh = master, pid, rec, exitCh
	} else {
		master, slave, err := openPTY()
		if err != nil {
			return failRec(fmt.Errorf("allocating pty: %w", err))
		}
		_ = setWinsize(master, rows, cols)

		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		cmd.Env = h.childEnv(name, hl.Env)
		if fi, err := os.Stat(h.home); err == nil && fi.IsDir() {
			cmd.Dir = h.home
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
		if err := cmd.Start(); err != nil {
			master.Close()
			slave.Close()
			return failRec(fmt.Errorf("starting %q: %w", argv[0], err))
		}
		slave.Close()
		s.cmd, s.ptmx, s.pid, s.rec = cmd, master, cmd.Process.Pid, rec
	}

	recPath := ""
	if rec != nil {
		recPath = rec.path()
	}
	h.audit.log("session_new", peer.fields(fields{"session": name, "pid": s.pid, "cmd": s.cmdline,
		"record": h.cfg.Record, "record_file": recPath}))
	go s.pump()
	go s.waitLoop()
	return s, nil
}

// dropSession removes a session from the table and applies the lifecycle rule.
func (h *host) dropSession(name string) { h.dropSessionAs(nil, name, name) }

// dropSessionAs removes the session s (or the one listed as name) and the
// launch record of key, its creation name.
func (h *host) dropSessionAs(s *session, name, key string) {
	h.forgetLaunch(key)
	h.mu.Lock()
	if s != nil {
		// by its current name, and only if that name is still this session
		// (checked under the same lock as the delete: a rename and a new
		// session of the old name may come in between otherwise)
		if n := s.Name(); h.sessions[n] == s {
			delete(h.sessions, n)
		}
	} else if name != "" {
		delete(h.sessions, name)
	}
	empty := len(h.sessions) == 0
	if empty {
		h.emptySince = time.Now()
	}
	stop := false
	if empty && len(h.results) == 0 && !h.closing && h.cfg.Lifecycle == lifeExit {
		h.closing = true
		stop = true
	}
	closing := h.closing
	h.mu.Unlock()
	if stop {
		h.audit.log("host_exit", fields{"reason": "last session ended"})
		if h.ln != nil {
			_ = h.ln.Close() // also unlinks the socket
		}
	}
	if closing && empty {
		h.finish()
	}
}

func (h *host) sessionEnded(s *session, code int, reason string, rec recSink) {
	if reason == "" {
		reason = "exited"
	}
	f := fields{"session": s.Name(), "pid": s.pid, "exit_code": code, "reason": reason,
		"duration_s": int(time.Since(s.created).Seconds())}
	if rec != nil {
		f["record_file"] = rec.path()
	}
	h.audit.log("session_end", f)
	h.keepResult(s, code, reason)
	h.endSession(s)
}

func (h *host) shutdown(reason string) {
	h.mu.Lock()
	h.closing = true
	list := make([]*session, 0, len(h.sessions))
	for _, s := range h.sessions {
		list = append(list, s)
	}
	h.mu.Unlock()
	h.audit.log("host_shutdown", fields{"reason": reason})
	if h.ln != nil {
		_ = h.ln.Close()
	}
	for _, s := range list {
		s.kill("host shutdown")
	}
	if len(list) == 0 {
		h.finish()
	}
}

func (h *host) finish() {
	h.once.Do(func() {
		if h.ln != nil {
			_ = h.ln.Close()
		}
		close(h.done)
	})
}

// janitor enforces idle_timeout and max_lifetime and, in exit_on_last_shell
// mode, stops a host that has had no sessions for too long.
func (h *host) janitor() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-h.done:
			return
		case <-t.C:
			h.mu.Lock()
			ss := make([]*session, 0, len(h.sessions))
			for _, s := range h.sessions {
				ss = append(ss, s)
			}
			h.expireResults()
			lingering := !h.closing && len(ss) == 0 && len(h.results) == 0 && h.cfg.Lifecycle == lifeExit &&
				time.Since(h.emptySince) > emptyGrace
			h.mu.Unlock()
			if lingering {
				h.shutdown("no sessions")
				return
			}
			h.checkWatches()
			for _, s := range ss {
				if h.cfg.MaxLifetime > 0 && time.Since(s.created) > h.cfg.MaxLifetime {
					h.audit.log("lifetime_kill", fields{"session": s.Name(),
						"age_s": int(time.Since(s.created).Seconds())})
					s.kill("max lifetime")
					continue
				}
				if h.cfg.IdleTimeout > 0 {
					if idle, attached := s.idleFor(); attached == 0 && idle > h.cfg.IdleTimeout {
						h.audit.log("idle_kill", fields{"session": s.Name(), "idle_s": int(idle.Seconds())})
						s.kill("idle timeout")
					}
				}
			}
		}
	}
}

// drainClients gives the connections still being served a moment to receive
// their last frames (a short job's output and exit status) before the process
// exits.
func (h *host) drainClients() {
	for i := 0; i < 300 && h.active.Load() > 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
}
