//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// launch_monitor.go: the monitor's side of session launching.
//
// A host does not start shells itself: it asks the monitor (ctlSpawn), which
//  - re-checks the request against the policy the host was started with,
//  - allocates the terminal as root (so the host cannot hand over a descriptor
//    of its choosing) and gives the user ownership of the slave side,
//  - starts a launcher (launcher.go) which runs PAM and the user's process,
//  - gives the host the master side and the pid, and tells it when the
//    process has exited (ctlExited).
// Signals to a session go through the monitor too (ctlKillSess): it can end a
// session even after the user has become root inside it.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// launchRequest is what a host asks for. Only the user's own wishes are in it;
// identity, shell, home and policy come from the monitor.
type launchRequest struct {
	Session string            `json:"session"`
	Argv    []string          `json:"argv,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Rows    uint16            `json:"rows"`
	Cols    uint16            `json:"cols"`
}

type spawnReply struct {
	Session string `json:"session"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	PID     int    `json:"pid,omitempty"`
}

type exitedMsg struct {
	Session string `json:"session"`
	Code    int    `json:"code"`
}

const (
	launchTimeout = 30 * time.Second // PAM (SSSD, ...) may be slow, but not forever
	termGrace     = 3 * time.Second
)

// launch is one session process (plus its launcher) known to the monitor.
type launch struct {
	name string

	mu       sync.Mutex
	shellPID int
	exited   bool
	proc     *os.Process // the launcher
	conn     *net.UnixConn
	once     sync.Once
	followWG sync.WaitGroup
	paused   bool
}

// terminate hangs up the session's process group; stragglers are killed after
// a grace period. It works whatever uid the processes have by now.
func (l *launch) terminate() {
	l.mu.Lock()
	pid, done := l.shellPID, l.exited
	proc := l.proc
	l.mu.Unlock()
	if done {
		return
	}
	if pid == 0 { // still starting: kill the launcher
		if proc != nil {
			_ = proc.Kill()
		}
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGHUP)
	_ = syscall.Kill(pid, syscall.SIGHUP)
	afterTracked(termGrace, func() {
		l.mu.Lock()
		done := l.exited
		l.mu.Unlock()
		if !done {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
}

// buildLaunchSpec validates a request against the host's policy and builds the
// specification for the launcher. Pure function: unit-testable.
func buildLaunchSpec(mh *mhost, rq *launchRequest) (*launchSpec, error) {
	if !nameRe.MatchString(rq.Session) {
		return nil, errors.New("invalid session name")
	}
	if len(rq.Argv) > 64 {
		return nil, errors.New("too many command arguments")
	}
	total := 0
	for _, a := range rq.Argv {
		if len(a) > 4096 || strings.ContainsRune(a, 0) {
			return nil, errors.New("invalid command argument")
		}
		total += len(a)
	}
	if total > 32*1024 {
		return nil, errors.New("command line too long")
	}
	if len(rq.Argv) > 0 && !mh.cfg.AllowCommand {
		return nil, errors.New("running arbitrary commands is disabled by configuration")
	}
	argv := rq.Argv
	if len(argv) == 0 {
		argv = []string{mh.shell}
	}
	if len(rq.Env) > 64 {
		return nil, errors.New("too many environment entries")
	}
	env := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=" + mh.home, "USER=" + mh.user, "LOGNAME=" + mh.user, "SHELL=" + mh.shell,
	}
	term := "xterm-256color"
	for k, v := range rq.Env {
		if !envAllowed(k) || len(v) > 256 || strings.ContainsRune(v, 0) {
			continue
		}
		if k == "TERM" {
			term = v
			continue
		}
		env = append(env, k+"="+v)
	}
	env = append(env, "TERM="+term, "PTSCRY=1", "PTSCRY_SESSION="+rq.Session)
	return &launchSpec{
		Service: mh.cfg.PAMService, Session: rq.Session,
		UID: uint32(mh.uid), GID: mh.gid, GIDs: mh.gids, User: mh.user, Home: mh.home,
		Argv: argv, Env: env,
	}, nil
}

var (
	ttyGIDOnce sync.Once
	ttyGIDVal  = -1
)

// ttyGID is the group that owns terminal devices ("tty"), or -1.
func ttyGID() int {
	ttyGIDOnce.Do(func() {
		if g, err := user.LookupGroup("tty"); err == nil {
			if n, err := strconv.Atoi(g.Gid); err == nil {
				ttyGIDVal = n
			}
		}
	})
	return ttyGIDVal
}

// newTerminal allocates a pseudoterminal as root and hands the slave side to
// the user (what grantpt() does): owner uid, group tty, mode 0620.
func newTerminal(uid, gid int, rows, cols uint16) (master, slave *os.File, err error) {
	master, slave, err = openPTY()
	if err != nil {
		return nil, nil, err
	}
	fail := func(e error) (*os.File, *os.File, error) {
		master.Close()
		slave.Close()
		return nil, nil, e
	}
	mode := os.FileMode(0o620)
	g := ttyGID()
	if g < 0 {
		g, mode = gid, 0o600
	}
	if err := slave.Chown(uid, g); err != nil {
		return fail(fmt.Errorf("terminal ownership: %w", err))
	}
	if err := slave.Chmod(mode); err != nil {
		return fail(fmt.Errorf("terminal mode: %w", err))
	}
	if rows == 0 || cols == 0 {
		rows, cols = 24, 80
	}
	_ = setWinsize(master, rows, cols)
	return master, slave, nil
}

// launchCount returns how many session processes exist, in total.
func (m *monitor) launchCount() int {
	m.mu.Lock()
	hs := make([]*mhost, 0, len(m.hosts))
	for _, h := range m.hosts {
		hs = append(hs, h)
	}
	m.mu.Unlock()
	n := 0
	for _, h := range hs {
		h.mu.Lock()
		n += len(h.launches)
		h.mu.Unlock()
	}
	return n
}

// onSpawn serves ctlSpawn. It runs in its own goroutine: PAM can take a while.
func (mh *mhost) onSpawn(p []byte) {
	var rq launchRequest
	reply := func(r spawnReply, master *os.File) {
		b, _ := json.Marshal(r)
		if master != nil && r.OK {
			if err := mh.ctl.sendFD(ctlSpawned, b, master); err != nil {
				mh.m.audit.log("launch_failed", fields{"uid": mh.uid, "user": mh.user, "session": r.Session,
					"stage": "handoff", "error": err.Error()})
				mh.mu.Lock()
				l := mh.launches[r.Session]
				mh.mu.Unlock()
				if l != nil { // nobody will ever attach to it: do not leave it running
					l.terminate()
				}
			}
			return
		}
		_ = mh.ctl.send(ctlSpawned, b)
	}
	if err := json.Unmarshal(p, &rq); err != nil {
		return
	}
	deny := func(why, public string) {
		mh.m.audit.log("launch_failed", fields{"uid": mh.uid, "user": mh.user, "session": rq.Session,
			"stage": "request", "error": why})
		reply(spawnReply{Session: rq.Session, Error: public}, nil)
	}
	spec, err := buildLaunchSpec(mh, &rq)
	if err != nil {
		deny(err.Error(), err.Error())
		return
	}
	_, global := mh.m.snapshot()

	// Caps, enforced here as well as at request time: they must hold even if a
	// host misbehaves.
	mh.m.resMu.Lock()
	mh.mu.Lock()
	_, dup := mh.launches[rq.Session]
	own := len(mh.launches)
	mh.mu.Unlock()
	total := mh.m.launchCount()
	switch {
	case dup:
		mh.m.resMu.Unlock()
		deny("session already running", "session already exists")
		return
	case mh.cfg.MaxSessions > 0 && own >= mh.cfg.MaxSessions:
		mh.m.resMu.Unlock()
		deny("session limit", fmt.Sprintf("session limit reached (%d)", mh.cfg.MaxSessions))
		return
	case global.MaxTotalSessions > 0 && total >= global.MaxTotalSessions:
		mh.m.resMu.Unlock()
		deny("server-wide session limit", "the server-wide session limit has been reached")
		return
	}
	l := &launch{name: rq.Session}
	mh.mu.Lock()
	mh.launches[rq.Session] = l
	mh.mu.Unlock()
	mh.m.resMu.Unlock()

	drop := func() {
		mh.mu.Lock()
		if mh.launches[rq.Session] == l {
			delete(mh.launches, rq.Session)
		}
		mh.mu.Unlock()
	}
	master, slave, err := newTerminal(mh.uid, int(mh.gid), rq.Rows, rq.Cols)
	if err != nil {
		drop()
		mh.m.audit.log("launch_failed", fields{"uid": mh.uid, "user": mh.user, "session": rq.Session,
			"stage": "terminal", "error": err.Error()})
		reply(spawnReply{Session: rq.Session, Error: "cannot allocate a terminal"}, nil)
		return
	}
	defer master.Close()

	pid, started, stage, err := mh.runLauncher(l, spec, slave)
	if err != nil {
		drop()
		mh.m.audit.log("launch_failed", fields{"uid": mh.uid, "user": mh.user, "session": rq.Session,
			"stage": stage, "error": err.Error(), "pam_service": spec.Service})
		public := "cannot set up the session"
		if stage == "account" {
			public = "session refused by the system access policy (PAM)"
		}
		reply(spawnReply{Session: rq.Session, Error: public}, nil)
		return
	}
	// loginuid and audit session are what the KERNEL says about the new process,
	// read by the launcher while the process was still its child (so the pid
	// cannot have been reused), not what anybody claims.
	mh.m.audit.log("session_launch", fields{"uid": mh.uid, "user": mh.user, "session": rq.Session,
		"shell_pid": pid, "launcher_pid": l.proc.Pid, "pam_service": spec.Service,
		"loginuid": started.LoginUID, "audit_session": started.AuditSession,
		"cmd": clip(strings.Join(spec.Argv, " "), 512)})
	reply(spawnReply{Session: rq.Session, OK: true, PID: pid}, master)
}

// runLauncher starts the launcher process and waits for it to report that the
// session process is running (or why not). Afterwards a goroutine follows the
// session until it ends.
func (mh *mhost) runLauncher(l *launch, spec *launchSpec, slave *os.File) (pid int, started launchMsg, stage string, err error) {
	defer slave.Close()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return 0, launchMsg{}, "socket", err
	}
	monEnd, lEnd := os.NewFile(uintptr(fds[0]), "mon"), os.NewFile(uintptr(fds[1]), "launcher")
	conn, err := net_fileConn(monEnd)
	monEnd.Close()
	if err != nil {
		lEnd.Close()
		return 0, launchMsg{}, "socket", err
	}
	cmd := exec.Command("/proc/self/exe", "launch")
	cmd.Args = []string{strings.TrimSuffix(mh.m.exe, " (deleted)"), "launch"}
	cmd.Dir = "/"
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	cmd.ExtraFiles = []*os.File{lEnd, slave}
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		lEnd.Close()
		conn.Close()
		return 0, launchMsg{}, "start", err
	}
	lEnd.Close()
	l.mu.Lock()
	l.proc = cmd.Process
	l.mu.Unlock()
	go func() { // reap the launcher whatever happens
		_ = cmd.Wait()
		l.finish(mh, -1, "launcher exited")
	}()

	b, _ := json.Marshal(spec)
	if _, err := conn.Write(b); err != nil {
		_ = cmd.Process.Kill()
		conn.Close()
		return 0, launchMsg{}, "spec", err
	}
	read := func() (launchMsg, error) { return readLaunchMsg(conn) }
	_ = conn.SetReadDeadline(time.Now().Add(launchTimeout))
	m, err := read()
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		_ = cmd.Process.Kill()
		conn.Close()
		return 0, launchMsg{}, "timeout", fmt.Errorf("no answer from the launcher: %w", err)
	}
	if m.T != "started" || m.PID <= 0 {
		conn.Close()
		st := m.Stage
		if st == "" {
			st = "launcher"
		}
		return 0, launchMsg{}, st, errors.New(m.Error)
	}
	l.mu.Lock()
	l.shellPID = m.PID
	l.conn = conn
	l.mu.Unlock()
	l.follow(mh, conn)
	return m.PID, m, "", nil
}

func readLaunchMsg(conn *net.UnixConn) (launchMsg, error) {
	var m launchMsg
	buf := make([]byte, launchMaxMsg)
	n, err := conn.Read(buf)
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(buf[:n], &m)
}

// follow waits for the launcher's "exit" report in the background.
func (l *launch) follow(mh *mhost, conn *net.UnixConn) {
	l.followWG.Add(1)
	go func() { // until the session ends (or the launcher dies)
		defer l.followWG.Done()
		closeIt := true
		defer func() {
			if closeIt {
				conn.Close()
			}
		}()
		for {
			m, err := readLaunchMsg(conn)
			if err != nil {
				if errors.Is(err, os.ErrDeadlineExceeded) { // only a re-exec sets a deadline here
					if mh.m.frozen.Load() {
						closeIt = false // paused: the descriptor is carried over
						return
					}
					continue
				}
				return // launcher gone: its reaper finishes the bookkeeping
			}
			if m.T == "exit" {
				l.finish(mh, m.Code, "exited")
				return
			}
		}
	}()
}

// finish runs once, when the session's process is gone: bookkeeping, audit,
// and the exit notice for the host.
func (l *launch) finish(mh *mhost, code int, why string) {
	l.once.Do(func() {
		l.mu.Lock()
		l.exited = true
		l.mu.Unlock()
		mh.mu.Lock()
		if mh.launches[l.name] == l {
			delete(mh.launches, l.name)
		}
		mh.mu.Unlock()
		mh.m.audit.log("launch_exit", fields{"uid": mh.uid, "user": mh.user, "session": l.name,
			"exit_code": code, "reason": why})
		b, _ := json.Marshal(exitedMsg{Session: l.name, Code: code})
		_ = mh.ctl.send(ctlExited, b)
	})
}

func (mh *mhost) onKillSess(p []byte) {
	var r struct {
		Session string `json:"session"`
	}
	if json.Unmarshal(p, &r) != nil {
		return
	}
	mh.mu.Lock()
	l := mh.launches[r.Session]
	mh.mu.Unlock()
	if l != nil {
		l.terminate()
	}
}

// terminateAll ends every session process of this host.
func (mh *mhost) terminateAll() {
	mh.mu.Lock()
	ls := make([]*launch, 0, len(mh.launches))
	for _, l := range mh.launches {
		ls = append(ls, l)
	}
	mh.mu.Unlock()
	for _, l := range ls {
		l.terminate()
	}
}

// compile-time check that net is used for the socket type
var _ net.Conn = (*net.UnixConn)(nil)
