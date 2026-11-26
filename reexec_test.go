//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A paused control channel keeps the host's messages and delivers them when
// the monitor resumes (the failed re-exec path).
func TestFreezeUnfreeze(t *testing.T) {
	dir := t.TempDir()
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "s"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	monEnd, hostEnd := os.NewFile(uintptr(fds[0]), "m"), os.NewFile(uintptr(fds[1]), "h")
	mc, err := net_fileConn(monEnd)
	monEnd.Close()
	if err != nil {
		t.Fatal(err)
	}
	hc, err := net_fileConn(hostEnd)
	hostEnd.Close()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _, _ = child.Process.Wait() }()
	proc := child.Process
	m := &monitor{ln: ln, hosts: map[int]*mhost{}, sem: make(chan struct{}, 4), audit: &chainAudit{path: filepath.Join(dir, "a.log"), prev: genesisHash}}
	if err := m.audit.openFile(); err != nil {
		t.Fatal(err)
	}
	mh := &mhost{m: m, uid: 4242, user: "u", pid: proc.Pid, proc: proc, ctl: newCtlConn(mc),
		launches: map[string]*launch{}, sessions: map[string]*msess{}, reserved: map[uint64]time.Time{},
		recs: map[string]*recorder{}, exited: make(chan struct{})}
	m.hosts[4242] = mh
	mh.startReader()

	send := func(name string) {
		b := []byte(`{"event":"session_new","fields":{"session":"` + name + `","pid":1,"cmd":"x"}}`)
		if _, err := hc.Write(append([]byte{ctlEvent}, b...)); err != nil {
			t.Fatal(err)
		}
	}
	has := func(name string) bool {
		mh.mu.Lock()
		defer mh.mu.Unlock()
		return mh.sessions[name] != nil
	}
	waitFor := func(name string, want bool) {
		for i := 0; i < 100; i++ {
			if has(name) == want {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("session %s present=%v, want %v", name, has(name), want)
	}
	send("one")
	waitFor("one", true)

	if err := m.freeze(); err != nil {
		t.Fatal(err)
	}
	send("two")
	time.Sleep(200 * time.Millisecond)
	if has("two") {
		t.Fatal("a paused channel must not be served")
	}
	if syscall.Kill(proc.Pid, 0) != nil {
		t.Fatal("the host must not be touched by a pause")
	}
	m.unfreeze()
	waitFor("two", true) // delivered after the resume, nothing lost
	send("three")
	waitFor("three", true)
	if syscall.Kill(proc.Pid, 0) != nil {
		t.Fatal("the host was killed")
	}
}

func TestSockFDInheritable(t *testing.T) {
	a, b, _ := socketPair(t)
	defer a.Close()
	defer b.Close()
	fd, err := sockFD(a)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	flags, _, e := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
	if e != 0 || flags&syscall.FD_CLOEXEC != 0 {
		t.Fatalf("duplicate must survive exec (flags=%d err=%v)", flags, e)
	}
	syscall.CloseOnExec(fd)
	flags, _, _ = syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
	if flags&syscall.FD_CLOEXEC == 0 {
		t.Fatal("CloseOnExec did not stick")
	}
}

func socketPair(t *testing.T) (*net.UnixConn, *net.UnixConn, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	f1, f2 := os.NewFile(uintptr(fds[0]), "a"), os.NewFile(uintptr(fds[1]), "b")
	a, _ := net_fileConn(f1)
	b, _ := net_fileConn(f2)
	f1.Close()
	f2.Close()
	return a, b, nil
}

func TestFEStateRoundTrip(t *testing.T) {
	f := newFeState()
	f.setUID(77)
	tk, _ := f.issue("mario")
	f.mu.Lock()
	f.totp["mario"] = &totpState{last: 99, fails: 2, locked: time.Now().Add(time.Minute)}
	f.mu.Unlock()
	d := f.export()
	g := newFeState()
	g.setUID(77)
	g.restore(d)
	if !g.valid(tk, "mario") || g.valid(tk, "other") {
		t.Fatal("ticket not carried over (or not bound to its user)")
	}
	g.mu.Lock()
	s := g.totp["mario"]
	g.mu.Unlock()
	if s == nil || s.last != 99 || s.fails != 2 {
		t.Fatal("TOTP counter/lockout not carried over")
	}
}

func TestRecorderStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	r, err := newRecorder(dir, "s", "output", "xterm", 24, 80, 0)
	if err != nil {
		t.Fatal(err)
	}
	r.output([]byte("hello "))
	fd, err := sockFD(r.f)
	if err != nil {
		t.Fatal(err)
	}
	st := r.export(fd)
	r.f.Close() // the old image is gone
	r2 := recorderFromState(st)
	r2.output([]byte("world"))
	r2.close()
	b, _ := os.ReadFile(r.Path)
	if !(len(b) > 0) || string(b) == "" {
		t.Fatal("empty recording")
	}
	if !contains2(string(b), "hello ") || !contains2(string(b), "world") {
		t.Fatalf("recording lost data across the carry-over:\n%s", b)
	}
}

func contains2(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// A re-exec that gives up because a request is still running must leave the
// control channels exactly as they were: one reader per host, in order.
func TestFreezeEarlyFailure(t *testing.T) {
	dir := t.TempDir()
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "s"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	a, b, _ := socketPair(t)
	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _, _ = child.Process.Wait() }()
	m := &monitor{ln: ln, hosts: map[int]*mhost{}, freezeWait: 300 * time.Millisecond,
		audit: &chainAudit{path: filepath.Join(dir, "a.log"), prev: genesisHash}}
	if err := m.audit.openFile(); err != nil {
		t.Fatal(err)
	}
	mh := &mhost{m: m, uid: 4243, user: "u", pid: child.Process.Pid, proc: child.Process, ctl: newCtlConn(a),
		launches: map[string]*launch{}, sessions: map[string]*msess{}, reserved: map[uint64]time.Time{},
		recs: map[string]*recorder{}, exited: make(chan struct{})}
	m.hosts[4243] = mh
	mh.startReader()

	m.freezeMu.RLock() // a request handler is busy
	if err := m.freeze(); err == nil {
		t.Fatal("freeze must give up while a handler is running")
	}
	m.unfreeze()
	m.freezeMu.RUnlock()

	send := func(ev, name string) {
		msg := `{"event":"` + ev + `","fields":{"session":"` + name + `","pid":1,"cmd":"x"}}`
		if _, err := b.Write(append([]byte{ctlEvent}, msg...)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 60; i++ { // two readers would reorder these
		send("session_new", "n")
		send("session_end", "n")
	}
	send("session_new", "last")
	for i := 0; i < 100; i++ {
		mh.mu.Lock()
		last := mh.sessions["last"] != nil
		n := mh.sessions["n"] != nil
		mh.mu.Unlock()
		if last {
			if n {
				t.Fatal("events were processed out of order: a duplicate reader is running")
			}
			if syscall.Kill(child.Process.Pid, 0) != nil {
				t.Fatal("the host was killed")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("events not processed")
}
