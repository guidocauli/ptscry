//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// session.go: one PTY session (shell process, output pump, attached clients).

package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type outMsg struct {
	t byte
	p []byte
}

// hclient is a client attached to a session (host side). Output is queued on
// a bounded channel and written by a dedicated goroutine, so a slow or stuck
// client can never block the PTY pump: when its queue overflows it is
// disconnected.
type hclient struct {
	fc       *framed
	readOnly bool
	out      chan outMsg
	wdone    chan struct{}

	cmu    sync.Mutex
	closed bool
	final  *outMsg

	// what the client watches in the user's other sessions (ptscry prefix m / M)
	watch      watchState
	toldWriter time.Time // when it was last told that another client holds the keyboard
}

// watchState is the activity / silence watch of one client (janitor only,
// except set, which takes cmu).
type watchState struct {
	flags    byte
	silence  time.Duration
	since    time.Time
	actAlert map[*session]time.Time // last activity alert per session
	silAlert map[*session]time.Time // the quiet period already reported
}

func newClient(fc *framed, readOnly bool) *hclient {
	return &hclient{fc: fc, readOnly: readOnly, out: make(chan outMsg, 512), wdone: make(chan struct{})}
}

// slowClientGrace is how long the pump waits for a client whose queue is full
// before giving up on it. A terminal that is busy drawing a flood of output
// (cat of a big file) falls behind for a moment: the shell is made to wait, as
// on any terminal, instead of the client being thrown out; one that does not
// recover within this time is still disconnected, so it cannot stall the
// session for good.
const slowClientGrace = 5 * time.Second

func (c *hclient) push(m outMsg) bool {
	deadline := time.Now().Add(slowClientGrace)
	for {
		c.cmu.Lock()
		if c.closed {
			c.cmu.Unlock()
			return false
		}
		select {
		case c.out <- m:
			c.cmu.Unlock()
			return true
		default:
		}
		if time.Now().After(deadline) {
			c.final = &outMsg{msgBye, []byte("client too slow, disconnected")}
			c.closed = true
			close(c.out)
			c.cmu.Unlock()
			return false
		}
		c.cmu.Unlock()
		time.Sleep(time.Millisecond)
	}
}

func (c *hclient) finish(m outMsg) {
	c.cmu.Lock()
	defer c.cmu.Unlock()
	if c.closed {
		return
	}
	c.final = &m
	c.closed = true
	close(c.out)
}

func (c *hclient) isFinished() bool {
	c.cmu.Lock()
	defer c.cmu.Unlock()
	return c.closed
}

func (c *hclient) writer() {
	defer close(c.wdone)
	for m := range c.out {
		if err := c.fc.write(m.t, m.p); err != nil {
			_ = c.fc.c.Close()
			return
		}
	}
	c.cmu.Lock()
	f := c.final
	c.cmu.Unlock()
	if f != nil {
		_ = c.fc.write(f.t, f.p)
	}
	_ = c.fc.c.Close()
}

type session struct {
	h *host
	// key is the name the session was created with: the monitor, the launcher
	// and the recorder know it by that, so it never changes. name is what the
	// user sees and may be renamed (nameMu).
	key     string
	nameMu  sync.Mutex
	name    string
	pid     int
	ptmx    *os.File
	cmd     *exec.Cmd // standalone / direct mode: the host's own child
	exitCh  chan int  // launcher mode: exit status, delivered by the monitor
	cmdline string
	created time.Time

	mu         sync.Mutex
	ring       *ring
	clients    map[*hclient]struct{}
	rows, cols uint16
	last       time.Time
	alt        bool
	notices    []time.Time // recent notify, for the rate limit
	lastOut    time.Time   // last output of the program (activity watch)
	writer     *hclient    // the client holding the keyboard (nil: every client may type)
	sending    bool        // a send-keys write is pending
	rec        recSink
	killReason string
	frozen     bool // processes stopped with SIGSTOP
	job        bool // keep the result when it ends (run -d)
	finished   bool // the process has ended and the output is complete
	exitCode   int

	pumpDone chan struct{}
	exited   chan struct{}
}

func (s *session) info() sessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sessionInfo{
		Name: s.Name(), PID: s.pid, Created: s.created, Last: s.last,
		Attached: len(s.clients), Rows: s.rows, Cols: s.cols, Cmd: s.cmdline,
		Cwd: procCwd(s.pid), Frozen: s.frozen,
	}
}

// pump copies PTY output to the replay ring, the recorder and all clients.
func (s *session) pump() {
	defer close(s.pumpDone)
	buf := make([]byte, 32*1024)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			s.mu.Lock()
			s.last = time.Now()
			s.lastOut = s.last
			s.alt = altScreenState(chunk, s.alt)
			if s.ring != nil {
				s.ring.Write(chunk)
			}
			if s.rec != nil {
				s.rec.output(chunk)
			}
			for c := range s.clients {
				if !c.push(outMsg{msgData, chunk}) {
					delete(s.clients, c)
				}
			}
			s.mu.Unlock()
		}
		if err != nil {
			return // EIO when the last slave descriptor closes
		}
	}
}

func exitCodeOf(ps *os.ProcessState) int {
	if ps == nil {
		return -1
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok {
		if ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ws.ExitStatus()
	}
	return ps.ExitCode()
}

func (s *session) waitLoop() {
	var code int
	if s.exitCh != nil {
		code = <-s.exitCh
	} else {
		_ = s.cmd.Wait()
		code = exitCodeOf(s.cmd.ProcessState)
	}
	// Let the pump drain what the shell printed last; background processes
	// may still hold the slave open, so do not wait forever.
	select {
	case <-s.pumpDone:
	case <-time.After(750 * time.Millisecond):
	}
	_ = s.ptmx.Close()
	select {
	case <-s.pumpDone:
	case <-time.After(2 * time.Second):
	}

	var st [4]byte
	binary.BigEndian.PutUint32(st[:], uint32(int32(code)))
	s.mu.Lock()
	s.finished, s.exitCode = true, code
	notified := make([]*hclient, 0, len(s.clients))
	for c := range s.clients {
		c.finish(outMsg{msgExit, st[:]})
		notified = append(notified, c)
	}
	s.clients = map[*hclient]struct{}{}
	rec := s.rec
	s.rec = nil
	reason := s.killReason
	s.mu.Unlock()
	if rec != nil {
		rec.close()
	}
	close(s.exited)
	// Let every client receive the exit status before the host may exit
	// (exit_on_last_shell): otherwise the process can end with the final
	// frame still queued and the client would see a lost connection.
	for _, c := range notified {
		select {
		case <-c.wdone:
		case <-time.After(2 * time.Second):
		}
	}
	s.h.sessionEnded(s, code, reason, rec)
}

func (s *session) attachClient(c *hclient, rows, cols uint16) {
	s.mu.Lock()
	if rows > 0 && cols > 0 {
		s.rows, s.cols = rows, cols
	}
	var replay []byte
	if s.alt {
		replay = []byte("\x1b[?1049h") // full-screen app: it will redraw on SIGWINCH
	} else if s.ring != nil {
		snap, trunc := s.ring.Snapshot()
		replay = cleanReplay(snap, trunc)
	}
	for off := 0; off < len(replay); off += 32 * 1024 {
		end := off + 32*1024
		if end > len(replay) {
			end = len(replay)
		}
		c.push(outMsg{msgData, replay[off:end]})
	}
	if s.finished {
		// A short job can end before its client attaches: give it the output
		// and the exit status instead of leaving it waiting for nothing.
		var st [4]byte
		binary.BigEndian.PutUint32(st[:], uint32(int32(s.exitCode)))
		c.finish(outMsg{msgExit, st[:]})
		s.mu.Unlock()
		return
	}
	s.clients[c] = struct{}{}
	s.last = time.Now()
	s.mu.Unlock()
	if rows > 0 && cols > 0 {
		_ = setWinsize(s.ptmx, rows, cols)
	}
	s.redraw()
}

func (s *session) detachClient(c *hclient) {
	s.mu.Lock()
	delete(s.clients, c)
	s.last = time.Now()
	s.mu.Unlock()
}

func (s *session) resize(rows, cols uint16) {
	s.mu.Lock()
	s.rows, s.cols = rows, cols
	s.mu.Unlock()
	_ = setWinsize(s.ptmx, rows, cols)
}

// redraw asks the foreground application to repaint (full-screen programs
// such as vim or htop redraw on SIGWINCH).
func (s *session) redraw() {
	if pg, err := foregroundPgrp(s.ptmx); err == nil && pg > 0 {
		_ = syscall.Kill(-pg, syscall.SIGWINCH)
	}
}

func (s *session) write(p []byte) {
	s.mu.Lock()
	s.last = time.Now()
	rec := s.rec
	s.mu.Unlock()
	if rec != nil {
		rec.inputData(p)
	}
	_, _ = s.ptmx.Write(p)
}

// kill hangs up the session; a straggler is killed after a grace period.
func (s *session) kill(reason string) {
	s.mu.Lock()
	if s.killReason == "" {
		s.killReason = reason
	}
	s.mu.Unlock()
	s.thaw()             // a stopped process would not see the hangup until it runs again
	if s.exitCh != nil { // the monitor signals the session: it can even if the user became root in it
		b, _ := json.Marshal(map[string]string{"session": s.key})
		_ = s.h.ctl.send(ctlKillSess, b)
		return
	}
	_ = syscall.Kill(-s.pid, syscall.SIGHUP)
	_ = syscall.Kill(s.pid, syscall.SIGHUP)
	time.AfterFunc(3*time.Second, func() {
		select {
		case <-s.exited:
		default:
			_ = syscall.Kill(-s.pid, syscall.SIGKILL)
			_ = syscall.Kill(s.pid, syscall.SIGKILL)
		}
	})
}

func (s *session) idleFor() (time.Duration, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.last), len(s.clients)
}

func joinCmd(argv []string) string {
	return strings.Join(argv, " ")
}

func (s *session) String() string { return fmt.Sprintf("session %s (pid %d)", s.Name(), s.pid) }

// Name is the current name of the session.
func (s *session) Name() string {
	s.nameMu.Lock()
	defer s.nameMu.Unlock()
	return s.name
}

func (s *session) setName(n string) {
	s.nameMu.Lock()
	s.name = n
	s.nameMu.Unlock()
}

// procCwd is the working directory of a process of ours, where /proc has it.
// The host runs as the session's user, so it can always read it; "" elsewhere.
func procCwd(pid int) string {
	if pid <= 0 || runtime.GOOS != "linux" {
		return ""
	}
	p, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd")
	if err != nil || !strings.HasPrefix(p, "/") || len(p) > 4096 || strings.ContainsAny(p, "\x00\n\r") {
		return ""
	}
	return p
}

// sessionPids returns the processes of the session: the ones whose session id
// is the leader's pid (Linux /proc), the leader's and the foreground process
// groups' members, never ptscry itself.
func (s *session) sessionPids() []int {
	seen := map[int]bool{}
	var out []int
	add := func(p int) {
		if p > 1 && p != os.Getpid() && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	add(s.pid)
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == s.pid {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// pid (comm) state ppid pgrp session ...: comm may hold spaces and ')'
		str := string(b)
		i := strings.LastIndexByte(str, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(str[i+1:])
		if len(f) < 4 {
			continue
		}
		if sid, _ := strconv.Atoi(f[3]); sid == s.pid {
			add(pid)
		}
	}
	return out
}

// signalAll sends sig to the session's processes; it reports whether at least
// one was reached.
func (s *session) signalAll(sig syscall.Signal) bool {
	ok := false
	for _, p := range s.sessionPids() {
		if syscall.Kill(p, sig) == nil {
			ok = true
		}
	}
	return ok
}

// freeze stops every process of the session (SIGSTOP): the program is parked
// exactly where it is, the terminal keeps what was typed. thaw undoes it.
func (s *session) freeze() error {
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return fmt.Errorf("session %q has ended", s.Name())
	}
	s.mu.Unlock()
	// twice: a process forked while we were scanning is caught by the second pass
	ok := s.signalAll(syscall.SIGSTOP)
	if s.signalAll(syscall.SIGSTOP) {
		ok = true
	}
	if !ok {
		return fmt.Errorf("cannot stop the processes of session %q", s.Name())
	}
	s.mu.Lock()
	s.frozen = true
	s.mu.Unlock()
	return nil
}

func (s *session) thaw() {
	s.mu.Lock()
	was := s.frozen
	s.frozen = false
	s.mu.Unlock()
	if was {
		s.signalAll(syscall.SIGCONT)
	}
}
