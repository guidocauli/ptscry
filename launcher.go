//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// launcher.go: `ptscry launch`, the root process that runs one session
// (see launch_cgo.go for what it does and why it exists).
//
// The monitor starts it with fd 3 = a private control socket and fd 4 = the
// terminal's slave side, sends one JSON launchSpec, and then only listens:
// "started" (with the pid), "failed" (stage and reason) or "exit" (status).
// If the control socket closes, the monitor is gone and the session is ended.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// launchSpec is everything the launcher needs. The monitor builds it from its
// own knowledge of the user; nothing in it comes from the user unchecked.
type launchSpec struct {
	Service string   `json:"service"` // PAM service name
	Session string   `json:"session"`
	UID     uint32   `json:"uid"`
	GID     uint32   `json:"gid"`
	GIDs    []uint32 `json:"gids"`
	User    string   `json:"user"`
	Home    string   `json:"home"`
	Argv    []string `json:"argv"`
	Env     []string `json:"env"`
}

// launchMsg is what the launcher tells the monitor.
type launchMsg struct {
	T     string `json:"t"` // started | failed | exit
	PID   int    `json:"pid,omitempty"`
	Stage string `json:"stage,omitempty"`
	Error string `json:"error,omitempty"`
	Code  int    `json:"code,omitempty"`
	// Read from /proc while the process is still our child (so the pid is ours).
	LoginUID     string `json:"loginuid,omitempty"`
	AuditSession string `json:"audit_session,omitempty"`
}

const launchMaxMsg = 128 * 1024

func init() {
	// The kernel lets only the main thread of a process write /proc/self/loginuid,
	// which pam_loginuid does. Keep the main goroutine on the main thread so that
	// the PAM calls made by `ptscry launch` (which run on it) are allowed to.
	runtime.LockOSThread()
}

// protectedEnv are variables PAM must not override.
func protectedEnv(k string) bool {
	return k == "TERM" || k == "PTSCRY" || k == "PTSCRY_SESSION" ||
		k == "HOME" || k == "USER" || k == "LOGNAME" || k == "SHELL"
}

// mergeEnv adds PAM's variables to the base ones (PAM wins, except for the protected ones).
func mergeEnv(base, pamEnv []string) []string {
	out := append([]string(nil), base...)
	have := map[string]int{}
	for i, kv := range out {
		k, _, _ := cutEq(kv)
		have[k] = i
	}
	for _, kv := range pamEnv {
		k, _, ok := cutEq(kv)
		if !ok || k == "" || protectedEnv(k) || len(kv) > 4096 {
			continue
		}
		if i, dup := have[k]; dup {
			out[i] = kv
		} else {
			have[k] = len(out)
			out = append(out, kv)
		}
	}
	return out
}

func cutEq(s string) (k, v string, ok bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '=' {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

func cmdLaunch(args []string) error {
	if !launcherAvailable {
		return errors.New("this build has no session launcher (built without cgo)")
	}
	if os.Getuid() != 0 {
		return errors.New("launch is internal and must run as root, started by the monitor")
	}
	if err := setNotDumpable(); err != nil {
		return err
	}
	cf := os.NewFile(3, "ctl")
	c, err := net.FileConn(cf)
	cf.Close()
	if err != nil {
		return fmt.Errorf("control socket: %w", err)
	}
	conn, ok := c.(*net.UnixConn)
	if !ok {
		return errors.New("control channel is not a unix socket")
	}
	defer conn.Close()
	send := func(m launchMsg) {
		b, _ := json.Marshal(m)
		_, _ = conn.Write(b)
	}
	fail := func(stage string, err error) error {
		send(launchMsg{T: "failed", Stage: stage, Error: err.Error()})
		return err
	}

	buf := make([]byte, launchMaxMsg)
	n, err := conn.Read(buf)
	if err != nil || n == 0 || n >= launchMaxMsg {
		return errors.New("no valid specification from the monitor")
	}
	var sp launchSpec
	if err := json.Unmarshal(buf[:n], &sp); err != nil {
		return fail("spec", err)
	}
	if err := checkSpec(&sp); err != nil {
		return fail("spec", err)
	}
	tty, err := os.Readlink("/proc/self/fd/4")
	if err != nil {
		return fail("tty", err)
	}

	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)

	pamEnv := []string(nil)
	var ph *pamHandle
	if sp.Service != "" {
		ph, pamEnv, err = pamOpen(sp.Service, sp.User, tty)
		if err != nil {
			stage := "pam"
			var pe *pamError
			if errors.As(err, &pe) {
				stage = pe.Stage
			}
			return fail(stage, err)
		}
	}
	env := mergeEnv(sp.Env, pamEnv)

	dir := sp.Home
	pid, err := spawnUser(4, sp.UID, sp.GID, sp.GIDs, dir, sp.Argv, env, sp.UID)
	if err != nil {
		if ph != nil {
			ph.close()
		}
		return fail("exec", err)
	}
	pf := "/proc/" + strconv.Itoa(pid) + "/"
	send(launchMsg{T: "started", PID: pid, LoginUID: procValue(pf + "loginuid"), AuditSession: procValue(pf + "sessionid")})

	exited := make(chan int, 1)
	go func() {
		var ws syscall.WaitStatus
		for {
			if _, err := syscall.Wait4(pid, &ws, 0, nil); err == syscall.EINTR {
				continue
			}
			break
		}
		code := ws.ExitStatus()
		if ws.Signaled() {
			code = 128 + int(ws.Signal())
		}
		exited <- code
	}()
	gone := make(chan struct{})
	go func() { // the monitor never writes again: EOF means it is gone
		b := make([]byte, 16)
		for {
			if _, err := conn.Read(b); err != nil {
				close(gone)
				return
			}
		}
	}()

	terminate := func() {
		_ = syscall.Kill(-pid, syscall.SIGHUP)
		_ = syscall.Kill(pid, syscall.SIGHUP)
		time.AfterFunc(3*time.Second, func() {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		})
	}
	var code int
	select {
	case code = <-exited:
	case <-gone:
		terminate()
		code = <-exited
	case <-signals:
		terminate()
		code = <-exited
	}
	if ph != nil {
		ph.close()
	}
	send(launchMsg{T: "exit", Code: code})
	return nil
}

func procValue(path string) string {
	b, err := os.ReadFile(path)
	if err != nil || len(b) > 32 {
		return "n/a"
	}
	return strings.TrimSpace(string(b))
}

// checkSpec re-validates what the monitor sent. The monitor is trusted, but a
// process that runs as root and then drops to a user id deserves its own check.
func checkSpec(sp *launchSpec) error {
	if len(sp.Argv) == 0 || len(sp.Argv) > 64 || sp.Argv[0] == "" {
		return errors.New("invalid command")
	}
	if len(sp.Env) > 256 || len(sp.GIDs) > 1024 || sp.User == "" || len(sp.User) > 128 {
		return errors.New("invalid specification")
	}
	u, err := user.Lookup(sp.User)
	if err != nil {
		return fmt.Errorf("user lookup: %w", err)
	}
	if u.Uid != strconv.FormatUint(uint64(sp.UID), 10) {
		return errors.New("user name and uid do not match")
	}
	return nil
}
