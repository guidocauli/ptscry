//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// reexec.go: replace the monitor's program in place, keeping the sessions alive.
//
// `systemctl restart` ends every session (fail closed: nothing may run without
// the monitor that audits it). To install a new version or recycle the monitor
// without that, the monitor re-executes itself *as the same process*:
//   - same pid, so systemd and the cgroup see no restart;
//   - the hosts, launchers and front-end are its children and stay its children;
//   - the sockets that tie them to it (hosts' and launchers' control channels,
//     the front-end's, the listening socket, the open recordings) are inherited
//     by the new image, so nothing needs to be re-established, found or trusted
//     anew: there is no rendezvous point that a user could squat.
// The state that is not in a descriptor (who runs what, limits, front-end
// tickets, TOTP lockouts) is passed through an anonymous file that only this
// process holds. Hosts keep running their old program until they exit; the
// control protocol between monitor and host must therefore stay compatible
// between adjacent versions.
//
// Trigger: SIGUSR2 or `ptscry admin reexec` (root only). If anything goes wrong
// before the exec, the monitor resumes where it was.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const resumeEnv = "PTSCRY_RESUME_FD"

type resumeFile struct {
	Version  int          `json:"version"`
	From     string       `json:"from_version"`
	Listener int          `json:"listener_fd"`
	ReqSeq   uint64       `json:"req_seq"`
	Hosts    []resumeHost `json:"hosts"`
	FE       *resumeFE    `json:"fe,omitempty"`
	FEState  feStateDump  `json:"fe_state"`
}

type resumeHost struct {
	UID      int                 `json:"uid"`
	User     string              `json:"user"`
	PID      int                 `json:"pid"`
	CtlFD    int                 `json:"ctl_fd"`
	Cfg      config              `json:"cfg"`
	GID      uint32              `json:"gid"`
	GIDs     []uint32            `json:"gids"`
	Home     string              `json:"home"`
	Shell    string              `json:"shell"`
	Sessions []msess             `json:"sessions"`
	Reserved map[uint64]int64    `json:"reserved"`
	Launches []resumeLaunch      `json:"launches"`
	Recs     map[string]recState `json:"recs"`
}

type resumeLaunch struct {
	Name        string `json:"name"`
	ShellPID    int    `json:"shell_pid"`
	LauncherPID int    `json:"launcher_pid"`
	FD          int    `json:"fd"`
}

type resumeFE struct {
	PID      int    `json:"pid"`
	UID      int    `json:"uid"`
	FD       int    `json:"fd"`
	InitJSON string `json:"init"`
	Started  int64  `json:"started_ns"`
}

const resumeVersion = 1

// pendingTimers counts the monitor's own delayed actions (the SIGKILL that
// follows a SIGHUP, ...): they live in this process and would be lost by an
// exec, so a re-exec waits until there are none.
var pendingTimers atomic.Int32

func afterTracked(d time.Duration, f func()) {
	pendingTimers.Add(1)
	time.AfterFunc(d, func() {
		defer pendingTimers.Add(-1)
		f()
	})
}

// sockFD returns a duplicate of the descriptor behind c that survives exec
// (dup clears close-on-exec), without touching c's own blocking mode.
func sockFD(c interface {
	SyscallConn() (syscall.RawConn, error)
}) (int, error) {
	rc, err := c.SyscallConn()
	if err != nil {
		return -1, err
	}
	nfd := -1
	var derr error
	if err := rc.Control(func(fd uintptr) { nfd, derr = syscall.Dup(int(fd)) }); err != nil {
		return -1, err
	}
	return nfd, derr
}

func (m *monitor) hostList() []*mhost {
	m.mu.Lock()
	defer m.mu.Unlock()
	hs := make([]*mhost, 0, len(m.hosts))
	for _, h := range m.hosts {
		hs = append(hs, h)
	}
	return hs
}

func (h *mhost) launchList() []*launch {
	h.mu.Lock()
	defer h.mu.Unlock()
	ls := make([]*launch, 0, len(h.launches))
	for _, l := range h.launches {
		ls = append(ls, l)
	}
	return ls
}

// freeze stops the monitor taking on new work and waits until nothing is
// half-done. On error the caller must call unfreeze.
func (m *monitor) freeze() error {
	m.frozen.Store(true)
	_ = m.ln.SetDeadline(time.Now()) // no new clients: they queue on the socket
	wait := 45 * time.Second         // a PAM stack may be slow
	if m.freezeWait > 0 {
		wait = m.freezeWait
	}
	deadline := time.Now().Add(wait)
	// Wait until every request handler has finished, and keep new ones out.
	for !m.freezeMu.TryLock() {
		if time.Now().After(deadline) {
			return errors.New("requests still in progress")
		}
		time.Sleep(20 * time.Millisecond)
	}
	m.freezeHeld = true

	hs := m.hostList()
	for _, h := range hs {
		h.frozen.Store(true)
		m.pausedHosts = append(m.pausedHosts, h)
		_ = h.ctl.c.SetReadDeadline(time.Now())
	}
	for _, h := range hs {
		select {
		case <-h.loopDone:
			h.paused.Store(true)
		case <-time.After(5 * time.Second):
			return fmt.Errorf("control channel of %s did not pause", h.user)
		}
	}
	// A pause may have caught a request being served: wait for it to finish.
	for m.spawning.Load() > 0 {
		if time.Now().After(deadline) {
			return errors.New("launches still in progress")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, h := range hs {
		for _, l := range h.launchList() {
			l.mu.Lock()
			c := l.conn
			l.mu.Unlock()
			if c != nil {
				m.pausedLaunchs = append(m.pausedLaunchs, l)
				_ = c.SetReadDeadline(time.Now())
			}
		}
	}
	for _, l := range m.pausedLaunchs {
		done := make(chan struct{})
		go func() { l.followWG.Wait(); close(done) }()
		select {
		case <-done:
			l.paused = true
		case <-time.After(3 * time.Second):
			return errors.New("a launcher channel did not pause")
		}
	}
	// Delayed kills (SIGHUP then SIGKILL) would be lost by the exec.
	for pendingTimers.Load() > 0 {
		if time.Now().After(deadline) {
			return errors.New("sessions are being terminated; try again in a moment")
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}

// unfreeze puts everything back after a failed re-exec (or an aborted one).
func (m *monitor) unfreeze() {
	m.frozen.Store(false)
	_ = m.ln.SetDeadline(time.Time{})
	for _, l := range m.pausedLaunchs {
		l.mu.Lock()
		c, done := l.conn, l.exited
		l.mu.Unlock()
		if c == nil || done {
			continue
		}
		_ = c.SetReadDeadline(time.Time{})
		if !l.paused {
			// Not confirmed stopped: it may still be about to stop.
			stopped := make(chan struct{})
			go func() { l.followWG.Wait(); close(stopped) }()
			select {
			case <-stopped:
			case <-time.After(300 * time.Millisecond):
				continue // still reading: it carries on by itself
			}
		}
		l.paused = false
		if l.exitedNow() {
			continue
		}
		// find its host
		for _, h := range m.pausedHosts {
			h.mu.Lock()
			own := h.launches[l.name] == l
			h.mu.Unlock()
			if own {
				l.follow(h, c)
				break
			}
		}
	}
	m.pausedLaunchs = nil
	for _, h := range m.pausedHosts {
		h.frozen.Store(false)
		_ = h.ctl.c.SetReadDeadline(time.Time{})
		if !h.paused.Load() {
			select {
			case <-h.loopDone:
			case <-time.After(300 * time.Millisecond):
				continue // still serving: it carries on by itself
			}
		}
		h.paused.Store(false)
		h.startReader()
	}
	m.pausedHosts = nil
	if m.freezeHeld {
		m.freezeHeld = false
		m.freezeMu.Unlock()
	}
}

func (l *launch) exitedNow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.exited
}

// stateFile makes the anonymous file the state travels in. It must be on a
// memory filesystem: it holds the front-end's host key.
func stateFile() (*os.File, error) {
	var last error
	for _, dir := range []string{"/run", "/dev/shm"} {
		f, err := os.CreateTemp(dir, "ptscry-resume-*")
		if err != nil {
			last = err
			continue
		}
		_ = os.Remove(f.Name())
		var st syscall.Statfs_t
		const tmpfsMagic, ramfsMagic = 0x01021994, 0x858458f6
		if err := syscall.Fstatfs(int(f.Fd()), &st); err != nil || (st.Type != tmpfsMagic && st.Type != ramfsMagic) {
			f.Close()
			last = fmt.Errorf("%s is not a memory filesystem", dir)
			continue
		}
		return f, nil
	}
	return nil, fmt.Errorf("no memory filesystem for the resume state: %v", last)
}

// ownChild reports whether pid is a child of this process.
func ownChild(pid int) bool {
	_, pp, ok := procStat(pid)
	return ok && pp == os.Getpid()
}

// reexec replaces this process's program with the one installed at the same
// path. It returns only if that failed (and the monitor is then running as before).
func (m *monitor) reexec(reason string) (err error) {
	if !m.reexecMu.TryLock() {
		return errors.New("a re-exec is already in progress")
	}
	defer m.reexecMu.Unlock()
	if m.stopReq.Load() || m.isStopping() {
		return errors.New("the monitor is stopping")
	}
	exe := strings.TrimSuffix(m.exe, " (deleted)")
	fail := func(e error) error {
		m.audit.log("monitor_reexec_failed", fields{"reason": reason, "error": clip(e.Error(), 300)})
		return e
	}
	// The new program must run and accept the current configuration: never
	// replace a working monitor with something that cannot start.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, verr := exec.CommandContext(ctx, exe, "version").Output()
	if verr != nil || !strings.Contains(string(out), progName) {
		return fail(fmt.Errorf("%s does not run (%v)", exe, verr))
	}
	newVersion := strings.TrimSpace(string(out))
	if tout, terr := exec.CommandContext(ctx, exe, "monitor", "-t", "-c", m.cfgPath).CombinedOutput(); terr != nil {
		return fail(fmt.Errorf("the new program refuses this installation: %s", strings.TrimSpace(string(tout))))
	}
	m.audit.log("monitor_reexec", fields{"reason": reason, "exe": exe, "from_version": progVersion,
		"to": clip(newVersion, 120), "pid": os.Getpid()})

	if err := m.freeze(); err != nil {
		m.unfreeze()
		return fail(err)
	}
	var opened []int // descriptors made inheritable: closed again unless we get to exec
	defer func() {
		if err != nil {
			m.unfreeze()
		}
	}()

	// Snapshot first: nothing below may wait for m.mu while holding ForkLock.
	hs := m.hostList()
	// Nothing may start a process between the first dup and the exec, or it
	// would inherit these descriptors.
	m.feMu.Lock()
	defer m.feMu.Unlock()
	syscall.ForkLock.Lock()
	defer syscall.ForkLock.Unlock()
	abort := func(e error) error { // runs before the locks are released
		for _, fd := range opened {
			syscall.Close(fd)
		}
		opened = nil
		return fail(e)
	}
	keep := func(fd int, e error) (int, error) {
		if e != nil {
			return -1, e
		}
		opened = append(opened, fd)
		return fd, nil
	}
	st := resumeFile{Version: resumeVersion, From: progVersion, ReqSeq: m.reqSeq.Load()}
	if st.Listener, err = keep(sockFD(m.ln)); err != nil {
		return abort(err)
	}
	for _, h := range hs {
		rh := resumeHost{UID: h.uid, User: h.user, PID: h.pid, Cfg: h.cfg, GID: h.gid, GIDs: h.gids,
			Home: h.home, Shell: h.shell, Reserved: map[uint64]int64{}, Recs: map[string]recState{}}
		if rh.CtlFD, err = keep(sockFD(h.ctl.c)); err != nil {
			return abort(err)
		}
		h.mu.Lock()
		for _, s := range h.sessions {
			rh.Sessions = append(rh.Sessions, *s)
		}
		for r, t := range h.reserved {
			rh.Reserved[r] = t.UnixNano()
		}
		recs := map[string]*recorder{}
		for n, r := range h.recs {
			recs[n] = r
		}
		h.mu.Unlock()
		for _, l := range h.launchList() {
			l.mu.Lock()
			c, proc, shell, done := l.conn, l.proc, l.shellPID, l.exited
			l.mu.Unlock()
			if done || c == nil || proc == nil || shell == 0 {
				continue // gone, or never got going
			}
			rl := resumeLaunch{Name: l.name, ShellPID: shell, LauncherPID: proc.Pid}
			if rl.FD, err = keep(sockFD(c)); err != nil {
				return abort(err)
			}
			rh.Launches = append(rh.Launches, rl)
		}
		for n, r := range recs {
			fd, e := keep(sockFD(r.f))
			if e != nil {
				return abort(e)
			}
			rh.Recs[n] = r.export(fd)
		}
		st.Hosts = append(st.Hosts, rh)
	}
	if m.fePID > 0 && m.feMon != nil && m.fe.getUID() != 0 {
		fd, e := keep(sockFD(m.feMon))
		if e != nil {
			return abort(e)
		}
		st.FE = &resumeFE{PID: m.fePID, UID: m.feUID, FD: fd, InitJSON: string(m.feInitJSON), Started: m.feStarted.UnixNano()}
	}
	st.FEState = m.fe.export()

	// The state goes through an anonymous file in memory that only we can see.
	tmp, e := stateFile()
	if e != nil {
		return abort(e)
	}
	defer tmp.Close()
	b, _ := json.Marshal(&st)
	if _, e := tmp.Write(b); e != nil {
		return abort(e)
	}
	sfd, e := syscall.Dup(int(tmp.Fd()))
	if e != nil {
		return abort(e)
	}
	opened = append(opened, sfd)
	if _, e := syscall.Seek(sfd, 0, 0); e != nil {
		return abort(e)
	}
	if m.stopReq.Load() {
		return abort(errors.New("the monitor is stopping"))
	}
	if pendingTimers.Load() > 0 {
		return abort(errors.New("sessions are being terminated; try again in a moment"))
	}

	env := append(os.Environ(), resumeEnv+"="+strconv.Itoa(sfd))
	if e := m.audit.suspend(); e != nil {
		return abort(e)
	}
	e = syscall.Exec(exe, os.Args, env)
	// Only reached if exec failed.
	if re := m.audit.reopen(); re != nil {
		fmt.Fprintf(os.Stderr, "ptscry: audit reopen after failed re-exec: %v\n", re)
	}
	return abort(fmt.Errorf("exec %s: %v", exe, e))
}

// ------------------------------------------------------------------ resume

// takeResume returns the state left by the previous image, or nil. It also
// makes every descriptor the state mentions close-on-exec again, before any
// child is started, so that none of them can leak into a user's process.
func takeResume() (*resumeFile, error) {
	v := os.Getenv(resumeEnv)
	if v == "" {
		return nil, nil
	}
	_ = os.Unsetenv(resumeEnv)
	fd, err := strconv.Atoi(v)
	if err != nil || fd < 3 {
		return nil, errors.New("invalid " + resumeEnv)
	}
	syscall.CloseOnExec(fd)
	f := os.NewFile(uintptr(fd), "resume")
	defer f.Close()
	var st resumeFile
	if err := json.NewDecoder(f).Decode(&st); err != nil {
		return nil, fmt.Errorf("resume state: %w", err)
	}
	if st.Version != resumeVersion {
		return nil, fmt.Errorf("resume state version %d not understood", st.Version)
	}
	mark := func(fd int) {
		if fd >= 3 {
			syscall.CloseOnExec(fd)
		}
	}
	mark(st.Listener)
	for _, h := range st.Hosts {
		mark(h.CtlFD)
		for _, l := range h.Launches {
			mark(l.FD)
		}
		for _, r := range h.Recs {
			mark(r.FD)
		}
	}
	if st.FE != nil {
		mark(st.FE.FD)
	}
	return &st, nil
}

func fdConn(fd int, name string) (*net.UnixConn, error) {
	f := os.NewFile(uintptr(fd), name)
	if f == nil {
		return nil, errors.New("bad descriptor")
	}
	defer f.Close()
	return net_fileConn(f)
}

// resume rebuilds the monitor's picture of the world from the previous image's
// state. Every pid is checked against what the kernel says before it is trusted:
// the hosts, launchers and the front-end must be our own children.
func (m *monitor) resume(st *resumeFile) error {
	m.reqSeq.Store(st.ReqSeq)
	nsess, nl, lost := 0, 0, 0
	for _, rh := range st.Hosts {
		conn, err := fdConn(rh.CtlFD, "ctl")
		if err != nil {
			return fmt.Errorf("host %s: %w", rh.User, err)
		}
		proc, _ := os.FindProcess(rh.PID)
		mh := &mhost{
			m: m, uid: rh.UID, user: rh.User, pid: rh.PID, proc: proc, ctl: newCtlConn(conn), cfg: rh.Cfg,
			gid: rh.GID, gids: rh.GIDs, home: rh.Home, shell: rh.Shell,
			launches: map[string]*launch{}, sessions: map[string]*msess{},
			reserved: map[uint64]time.Time{}, recs: map[string]*recorder{}, exited: make(chan struct{}),
		}
		for i := range rh.Sessions {
			s := rh.Sessions[i]
			mh.sessions[s.Name] = &s
		}
		nsess += len(rh.Sessions)
		for r, ns := range rh.Reserved {
			mh.reserved[r] = time.Unix(0, ns)
		}
		for n, rs := range rh.Recs {
			mh.recs[n] = recorderFromState(rs)
		}
		hostOK := ownChild(rh.PID) && procOwner(rh.PID) == uint32(rh.UID)
		for _, rl := range rh.Launches {
			lc, err := fdConn(rl.FD, "launcher")
			if err != nil {
				return fmt.Errorf("launcher of %s/%s: %w", rh.User, rl.Name, err)
			}
			lp, _ := os.FindProcess(rl.LauncherPID)
			l := &launch{name: rl.Name, shellPID: rl.ShellPID, proc: lp, conn: lc}
			mh.launches[rl.Name] = l
			nl++
			if !ownChild(rl.LauncherPID) {
				// Not our child any more: the launcher is gone, and with it the session.
				l.finish(mh, -1, "launcher lost across re-exec")
				lc.Close()
				continue
			}
			l.follow(mh, lc)
			go func(l *launch, lp *os.Process) { // reap the launcher whatever happens
				_, _ = lp.Wait()
				// Its "exit" report may be queued already: let it win, it has the real status.
				time.Sleep(300 * time.Millisecond)
				l.finish(mh, -1, "launcher exited")
			}(l, lp)
		}
		m.mu.Lock()
		m.hosts[mh.uid] = mh
		m.mu.Unlock()
		if !hostOK {
			// The host died while the monitor was being replaced: end its sessions and
			// close its recordings, exactly as when it dies at any other time.
			lost++
			go mh.cleanup(errors.New("host lost across re-exec"))
			continue
		}
		mh.startReader()
		go func() {
			ps, err := proc.Wait()
			if err == nil && ps != nil && !ps.Success() {
				err = errors.New(ps.String())
			}
			mh.cleanup(err)
		}()
	}
	m.fe.restore(st.FEState)
	if st.FE != nil {
		if ownChild(st.FE.PID) && procOwner(st.FE.PID) == uint32(st.FE.UID) && st.FE.UID != 0 {
			proc, _ := os.FindProcess(st.FE.PID)
			mon := os.NewFile(uintptr(st.FE.FD), "fe")
			done := make(chan error, 1)
			go func() { _, err := proc.Wait(); done <- err }()
			m.feMu.Lock()
			m.fePID, m.feMon, m.feUID, m.feInitJSON = st.FE.PID, mon, st.FE.UID, []byte(st.FE.InitJSON)
			m.feStarted = time.Unix(0, st.FE.Started)
			m.feMu.Unlock()
			m.fe.mu.Lock()
			m.fe.uid = st.FE.UID
			m.fe.mu.Unlock()
			m.feAdopt = &adoptedFE{proc: proc, done: done, mon: mon, init: []byte(st.FE.InitJSON), started: m.feStarted}
		} else {
			syscall.Close(st.FE.FD)
		}
	}
	m.audit.log("monitor_resumed", fields{"from_version": st.From, "to_version": progVersion,
		"hosts": len(m.hosts), "sessions": nsess, "launchers": nl, "hosts_lost": lost, "front_end": m.feAdopt != nil})
	return nil
}

type adoptedFE struct {
	proc    *os.Process
	done    chan error
	mon     *os.File
	init    []byte
	started time.Time
}
