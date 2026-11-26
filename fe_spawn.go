//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// fe_spawn.go: the monitor starts and supervises the SSH front-end.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

// feInit is everything the front-end gets, over its private channel (fd 3).
// It never reads the configuration file or any secret file itself.
type feInit struct {
	Listen         string   `json:"listen"`
	HostKey        string   `json:"host_key"` // PEM
	CAKeys         []string `json:"ca_keys,omitempty"`
	KeysEnabled    bool     `json:"keys_enabled"`
	FIDO2          bool     `json:"fido2"`
	TOTP           bool     `json:"totp"`
	CertMaxSeconds int64    `json:"cert_max_s"`
	MaxConns       int      `json:"max_conns"`
	RatePerMin     int      `json:"rate_per_min"`
	Seccomp        bool     `json:"seccomp"`
	MonitorSocket  string   `json:"monitor_socket"`
	DefaultSession string   `json:"default_session"`
}

// buildFeInit validates the front-end settings and reads the files they name.
// Used at start and at every reload (a bad configuration is refused).
func buildFeInit(g config) (*feInit, *identity, error) {
	id, err := lookupIdentityByName(g.FrontendUser)
	if err != nil {
		return nil, nil, fmt.Errorf("frontend_user %q: %v", g.FrontendUser, err)
	}
	uid, _ := strconv.Atoi(id.u.Uid)
	if uid == 0 {
		return nil, nil, errors.New("frontend_user must not be root")
	}
	// The front-end account is trusted to speak for users: it must be a
	// dedicated service account, not somebody who also uses ptscry or logs in.
	if interactiveShell(lookupShell(uid)) {
		return nil, nil, fmt.Errorf("frontend_user %s has an interactive shell: use a dedicated account with /usr/sbin/nologin", id.u.Username)
	}
	if g.AccessGroup != "" && contains(id.groups, g.AccessGroup) {
		return nil, nil, fmt.Errorf("frontend_user %s must not be a member of access_group %s", id.u.Username, g.AccessGroup)
	}
	if g.AdminGroup != "" && contains(id.groups, g.AdminGroup) {
		return nil, nil, fmt.Errorf("frontend_user %s must not be a member of admin_group %s", id.u.Username, g.AdminGroup)
	}
	in := &feInit{
		Listen: g.FrontendListen, KeysEnabled: g.FrontendKeysDir != "none", FIDO2: g.FrontendFIDO2,
		TOTP: g.FrontendTOTP, CertMaxSeconds: int64(g.FrontendCertMax / time.Second),
		MaxConns: g.FrontendMaxConns, RatePerMin: g.FrontendRate, Seccomp: g.FrontendSeccomp, MonitorSocket: g.MonitorSocket, DefaultSession: "main",
	}
	hk, err := readRootFile(g.FrontendHostKey, 16<<10, true)
	if err != nil {
		return nil, nil, fmt.Errorf("frontend_host_key: %v", err)
	}
	signer, err := ssh.ParsePrivateKey(hk)
	if err != nil {
		return nil, nil, fmt.Errorf("frontend_host_key: %v", err)
	}
	if signer.PublicKey().Type() != ssh.KeyAlgoED25519 {
		return nil, nil, errors.New("frontend_host_key must be an ed25519 key")
	}
	in.HostKey = string(hk)
	if g.FrontendCAKeys != "none" {
		raw, err := readRootFile(g.FrontendCAKeys, 64<<10, false)
		if err != nil {
			return nil, nil, fmt.Errorf("frontend_ca_keys: %v", err)
		}
		for _, ln := range strings.Split(string(raw), "\n") {
			ln = strings.TrimSpace(ln)
			if ln == "" || strings.HasPrefix(ln, "#") {
				continue
			}
			if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(ln)); err != nil {
				return nil, nil, fmt.Errorf("frontend_ca_keys: %v", err)
			}
			in.CAKeys = append(in.CAKeys, ln)
		}
	}
	if len(in.CAKeys) == 0 && !in.KeysEnabled {
		return nil, nil, errors.New("front-end enabled but neither CA keys nor authorized keys are configured")
	}
	if g.FrontendKeysDir != "none" {
		if err := checkTrusted(g.FrontendKeysDir, true); err != nil {
			return nil, nil, fmt.Errorf("frontend_authorized_keys_dir: %v", err)
		}
	}
	if g.FrontendTOTP {
		if g.FrontendTOTPDir == "none" {
			return nil, nil, errors.New("frontend_totp = yes needs frontend_totp_dir")
		}
		if err := checkTrusted(g.FrontendTOTPDir, true); err != nil {
			return nil, nil, fmt.Errorf("frontend_totp_dir: %v", err)
		}
	}
	return in, id, nil
}

// frontendLoop keeps the front-end running as the configuration says. It
// restarts it when it dies (with back-off) or when its settings change.
func (m *monitor) frontendLoop() {
	backoff := time.Second
	var cur []byte
	lastFEErr := ""
	for {
		if m.isStopping() {
			return
		}
		if a := m.feAdopt; a != nil { // came across a monitor re-exec: keep it running
			m.feAdopt = nil
			started := a.started
			reason := m.superviseFrontend(a.proc, a.done, a.init)
			a.mon.Close()
			if m.isStopping() {
				return
			}
			m.audit.log("frontend_exit", fields{"reason": reason})
			if time.Since(started) > time.Minute {
				backoff = time.Second
			}
			m.feWait(backoff)
			continue
		}
		_, g := m.snapshot()
		if !g.Frontend {
			m.fe.setUID(0)
			m.feWait(time.Minute)
			continue
		}
		in, id, err := buildFeInit(g)
		if err != nil {
			m.audit.log("frontend_config_error", fields{"error": clip(err.Error(), 300)})
			if msg := err.Error(); msg != lastFEErr { // also in the journal, once per distinct error
				lastFEErr = msg
				fmt.Fprintf(os.Stderr, "ptscry: front-end not started: %s\n", clip(msg, 300))
			}
			m.feWait(30 * time.Second)
			continue
		}
		lastFEErr = ""
		b, _ := json.Marshal(in)
		cur = b
		started := time.Now()
		reason := m.runFrontend(in, id, cur)
		if m.isStopping() {
			return
		}
		m.audit.log("frontend_exit", fields{"reason": reason})
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		m.feWait(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (m *monitor) isStopping() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopping
}

// feWait sleeps for d, or less when a reload asks for re-evaluation.
func (m *monitor) feWait(d time.Duration) {
	select {
	case <-time.After(d):
	case <-m.feWake:
	}
}

// runFrontend starts the process and returns why it ended.
func (m *monitor) runFrontend(in *feInit, id *identity, initJSON []byte) string {
	uid, _ := strconv.Atoi(id.u.Uid)
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return "socketpair: " + err.Error()
	}
	monEnd, feEnd := os.NewFile(uintptr(fds[0]), "mon"), os.NewFile(uintptr(fds[1]), "fe")
	defer monEnd.Close()

	cmd := exec.Command("/proc/self/exe", "frontend")
	cmd.Args[0] = strings.TrimSuffix(m.exe, " (deleted)")
	cmd.Dir = "/"
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/", "PTSCRY_FE_FD=3"}
	cmd.ExtraFiles = []*os.File{feEnd}
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:     true,
		Credential: &syscall.Credential{Uid: uint32(uid), Gid: id.gid, Groups: []uint32{}},
	}
	m.feMu.Lock() // a monitor re-exec must not run while the front-end is being (re)started
	m.fe.setUID(uid)
	if err := cmd.Start(); err != nil {
		m.feMu.Unlock()
		feEnd.Close()
		m.fe.setUID(0)
		return "start: " + err.Error()
	}
	feEnd.Close()
	_ = monEnd.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := monEnd.Write(append(initJSON, '\n')); err != nil {
		m.feMu.Unlock()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		m.fe.setUID(0)
		return "init: " + err.Error()
	}
	m.fePID = cmd.Process.Pid
	m.feInitJSON = append([]byte(nil), initJSON...)
	m.feMon, m.feUID, m.feStarted = monEnd, uid, time.Now()
	m.feMu.Unlock()
	m.audit.log("frontend_start", fields{"pid": cmd.Process.Pid, "uid": uid, "user": id.u.Username,
		"listen": in.Listen, "fido2_required": in.FIDO2, "totp": in.TOTP, "ca_keys": len(in.CAKeys),
		"plain_keys": in.KeysEnabled, "cert_max_s": in.CertMaxSeconds})

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return m.superviseFrontend(cmd.Process, done, initJSON)
}

// superviseFrontend watches a running front-end until it ends, or restarts it
// (by returning) when the configuration changes. Also used for a front-end
// carried over a monitor re-exec.
func (m *monitor) superviseFrontend(proc *os.Process, done chan error, initJSON []byte) string {
	for {
		select {
		case err := <-done:
			m.fe.setUID(0)
			if err != nil {
				return "process ended: " + err.Error()
			}
			return "process ended"
		case <-m.feWake:
			// Reload or shutdown: restart only if the settings really changed.
			_, g := m.snapshot()
			if m.isStopping() || !g.Frontend {
				_ = proc.Signal(syscall.SIGTERM)
				m.killAfter(proc, done)
				m.fe.setUID(0)
				return "stopped"
			}
			nin, _, err := buildFeInit(g)
			if err != nil {
				m.audit.log("frontend_config_error", fields{"error": clip(err.Error(), 300)})
				continue
			}
			nb, _ := json.Marshal(nin)
			if !bytes.Equal(nb, initJSON) {
				_ = proc.Signal(syscall.SIGTERM)
				m.killAfter(proc, done)
				m.fe.setUID(0)
				return "configuration changed"
			}
		}
	}
}

func (m *monitor) killAfter(proc *os.Process, done chan error) {
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = proc.Kill()
		<-done
	}
}

// stopFrontend is called at shutdown.
func (m *monitor) stopFrontend() {
	select {
	case m.feWake <- struct{}{}:
	default:
	}
}
