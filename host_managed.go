//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// host_managed.go: host started by the monitor (production mode).

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// initMsg is the first message the monitor sends: the host takes all its
// policy from here and never reads the configuration file itself.
type initMsg struct {
	UID  int    `json:"uid"`
	User string `json:"user"`
	Home string `json:"home"`
	Cfg  config `json:"cfg"`
}

// clientMsg accompanies a handed-over client connection.
type clientMsg struct {
	Hello hello  `json:"hello"`
	Req   uint64 `json:"req"`
	PID   int32  `json:"pid"`
	UID   uint32 `json:"uid"`
}

type recAck struct {
	Session string `json:"session"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	File    string `json:"file,omitempty"`
	path    string
}

// hardenSelf makes the host unreadable and untraceable by other processes of
// the same user (including the user's own shell, which is its child): no
// ptrace, no /proc/PID/mem, no core dump. Together with kernel.yama.ptrace_scope
// >= 1 this is what keeps audit data in the host out of the user's reach.
func hardenSelf() error {
	if err := setNotDumpable(); err != nil {
		return err
	}
	syscall.Umask(0o022) // sessions get a conventional umask
	return nil
}

// setNotDumpable blocks ptrace, /proc/PID/mem and core dumps for this process
// (not inherited across exec, so it never reaches the user's shell).
func setNotDumpable() error {
	const prSetDumpable = 4
	if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetDumpable, 0, 0, 0, 0, 0); e != 0 {
		return e
	}
	return nil
}

func runManagedHost() error {
	fdn, err := strconv.Atoi(os.Getenv("PTSCRY_CTL_FD"))
	if err != nil || fdn < 3 {
		return errors.New("invalid PTSCRY_CTL_FD")
	}
	f := os.NewFile(uintptr(fdn), "ctl")
	conn, err := net.FileConn(f)
	f.Close()
	if err != nil {
		return fmt.Errorf("control channel: %w", err)
	}
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return errors.New("control channel is not a unix socket")
	}
	if err := hardenSelf(); err != nil {
		return fmt.Errorf("hardening: %w", err)
	}
	cc := newCtlConn(uc)

	t, p, _, err := cc.recv(false)
	if err != nil || t != ctlInit {
		return errors.New("expected init message from monitor")
	}
	var im initMsg
	if err := json.Unmarshal(p, &im); err != nil {
		return fmt.Errorf("init message: %w", err)
	}
	if im.UID != os.Getuid() {
		return errors.New("init message does not match our uid")
	}

	h := &host{
		cfg: im.Cfg, uid: im.UID, username: im.User, home: im.Home,
		audit: &ctlAudit{cc: cc}, ctl: cc, recAcks: map[string]chan recAck{},
		sessions: map[string]*session{}, done: make(chan struct{}),
		emptySince: time.Now(),
		spawns:     map[string]chan spawnResult{}, exits: map[string]chan int{},
		byeAck: make(chan struct{}),
	}
	h.audit.log("host_start", fields{"pid": os.Getpid(), "lifecycle": h.cfg.Lifecycle,
		"version": progVersion, "mode": "managed"})

	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		s := <-sig
		h.shutdown("signal " + s.String())
	}()
	go h.janitor()
	go h.ctlLoop()

	<-h.done
	h.drainClients()
	h.goodbye()
	h.audit.log("host_stop", fields{"pid": os.Getpid()})
	return nil
}

// ctlLoop serves the monitor. Losing the channel is fatal by design: a host
// that can no longer be audited must not keep running sessions.
func (h *host) ctlLoop() {
	for {
		t, p, f, err := h.ctl.recv(true)
		if err != nil {
			h.abandon()
			return
		}
		switch t {
		case ctlClient:
			if f == nil {
				continue
			}
			h.active.Add(1) // counted now: drainClients must not miss it
			go func() {
				defer h.active.Add(-1)
				h.handoff(p, f)
			}()
		case ctlKill:
			var m struct{ Session, Reason string }
			if json.Unmarshal(p, &m) == nil {
				if s := h.get(m.Session); s != nil {
					s.kill(m.Reason)
				}
			}
		case ctlShutdown:
			var m struct{ Reason string }
			_ = json.Unmarshal(p, &m)
			go h.shutdown("monitor: " + m.Reason)
		case ctlByeAck:
			h.byeOnce.Do(func() { close(h.byeAck) })
		case ctlSpawned:
			h.onSpawned(p, f)
		case ctlExited:
			h.onExited(p)
		case ctlRecAck:
			var a recAck
			if json.Unmarshal(p, &a) == nil {
				h.recMu.Lock()
				ch := h.recAcks[a.Session]
				h.recMu.Unlock()
				if ch != nil {
					select {
					case ch <- a:
					default:
					}
				}
			}
		}
		if f != nil && t != ctlClient && t != ctlSpawned {
			f.Close()
		}
	}
}

// goodbye tells the monitor that this host is ending and waits for its
// acknowledgement: the clients it handed over in the meantime are answered
// (the host is closing: the client retries and reaches a new host) instead of
// being lost with the control socket.
func (h *host) goodbye() {
	if err := h.ctl.send(ctlBye, nil); err != nil {
		return
	}
	select {
	case <-h.byeAck:
	case <-time.After(3 * time.Second): // an older monitor: it does not answer
	}
	h.drainClients()
}

// handoff serves a client connection passed by the monitor.
func (h *host) handoff(p []byte, f *os.File) {
	var m clientMsg
	if err := json.Unmarshal(p, &m); err != nil {
		f.Close()
		return
	}
	conn, err := net.FileConn(f)
	f.Close()
	if err != nil {
		return
	}
	uc := conn.(*net.UnixConn)
	defer uc.Close()
	h.dispatch(newFramed(uc), &m.Hello, peerInfo{uid: m.UID, pid: m.PID, req: m.Req})
}

// ---------------------------------------------------------------- audit relay

type ctlAudit struct{ cc *ctlConn }

func (a *ctlAudit) log(event string, f fields) {
	b, err := json.Marshal(struct {
		Event  string `json:"event"`
		Fields fields `json:"fields"`
	}{event, f})
	if err == nil {
		_ = a.cc.send(ctlEvent, b)
	}
}

func (a *ctlAudit) close() {}

// ---------------------------------------------------------------- recording relay

// ctlRecorder streams session data to the monitor, which writes the
// recording as root. The user never touches the recording file.
type ctlRecorder struct {
	cc    *ctlConn
	name  string
	file  string
	input bool
}

func (h *host) openCtlRecorder(name, mode, term string, rows, cols uint16) (recSink, error) {
	ch := make(chan recAck, 1)
	h.recMu.Lock()
	h.recAcks[name] = ch
	h.recMu.Unlock()
	defer func() {
		h.recMu.Lock()
		delete(h.recAcks, name)
		h.recMu.Unlock()
	}()
	b, _ := json.Marshal(map[string]any{"session": name, "mode": mode, "term": term, "rows": rows, "cols": cols})
	if err := h.ctl.send(ctlRecOpen, b); err != nil {
		return nil, err
	}
	select {
	case a := <-ch:
		if !a.OK {
			return nil, errors.New(a.Error)
		}
		return &ctlRecorder{cc: h.ctl, name: name, file: a.File, input: mode == "all"}, nil
	case <-time.After(10 * time.Second):
		return nil, errors.New("monitor did not answer the recording request")
	}
}

func (r *ctlRecorder) data(kind byte, b []byte) {
	for len(b) > 0 {
		n := len(b)
		if n > ctlChunk {
			n = ctlChunk
		}
		msg := make([]byte, 0, 2+len(r.name)+n)
		msg = append(msg, kind, byte(len(r.name)))
		msg = append(msg, r.name...)
		msg = append(msg, b[:n]...)
		_ = r.cc.send(ctlRecData, msg)
		b = b[n:]
	}
}

func (r *ctlRecorder) output(b []byte) { r.data('o', b) }

func (r *ctlRecorder) inputData(b []byte) {
	if r.input {
		r.data('i', b)
	}
}

func (r *ctlRecorder) close() { _ = r.cc.send(ctlRecClose, []byte(r.name)) }

func (r *ctlRecorder) path() string { return r.file }

// abandon is the end of a host that has lost its monitor: nothing can be
// audited or signalled through the monitor any more, so every session is hung
// up here (best effort, as the user) and the process ends at once. Sessions
// that were started through the monitor are also ended by their launchers,
// which see the monitor go away too, and by the kernel when the terminal
// masters close with this process.
func (h *host) abandon() {
	h.mu.Lock()
	h.closing = true
	ss := make([]*session, 0, len(h.sessions))
	for _, s := range h.sessions {
		ss = append(ss, s)
	}
	h.mu.Unlock()
	for _, s := range ss {
		_ = syscall.Kill(-s.pid, syscall.SIGHUP)
		_ = syscall.Kill(s.pid, syscall.SIGHUP)
		if s.ptmx != nil {
			_ = s.ptmx.Close()
		}
	}
	time.Sleep(200 * time.Millisecond)
	os.Exit(1)
}
