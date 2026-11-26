//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// fe_monitor.go: monitor side of the SSH front-end.
//
// The front-end is an unprivileged process that terminates SSH. It proves
// possession of a key (or certificate), but the monitor stays the authority:
//   - it applies the access policy (enabled, access_group, shell);
//   - it verifies the TOTP second factor itself (secrets are root-only), so a
//     compromised front-end cannot skip it;
//   - it hands out short-lived tickets: a front-end request to open a session
//     for a user is honoured only with a ticket the monitor issued for that user;
//   - every action is audited with the remote address and the key used.

package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var feUserRe = regexp.MustCompile(`^[A-Za-z0-9._@][A-Za-z0-9._@-]{0,63}$`)

const (
	feTicketTTL    = 12 * time.Hour
	feMaxTickets   = 8192
	feTicketsUser  = 64
	totpStep       = 30
	totpMaxFails   = 5
	totpLockFor    = 15 * time.Minute
	feKeysMaxBytes = 64 << 10
	feKeysMaxLines = 200
)

type feTicket struct {
	user    string
	expires time.Time
	issued  time.Time
}

type totpState struct {
	mu     sync.Mutex // held for a whole verification: parallel guesses cannot beat the lockout
	last   int64      // highest counter accepted (replay protection)
	fails  int
	locked time.Time
}

// feState is what the monitor keeps about its front-end.
type feState struct {
	mu      sync.Mutex
	uid     int // 0 = no front-end running
	tickets map[string]*feTicket
	totp    map[string]*totpState
}

func newFeState() *feState {
	return &feState{tickets: map[string]*feTicket{}, totp: map[string]*totpState{}}
}

func (f *feState) setUID(uid int) {
	f.mu.Lock()
	f.uid = uid
	f.tickets = map[string]*feTicket{} // a new front-end process starts with no tickets
	f.mu.Unlock()
}

func (f *feState) getUID() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.uid
}

func (f *feState) issue(user string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	t := hex.EncodeToString(b[:])
	now := time.Now()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.tickets) >= feMaxTickets/2 {
		for k, v := range f.tickets {
			if now.After(v.expires) {
				delete(f.tickets, k)
			}
		}
	}
	if len(f.tickets) >= feMaxTickets {
		return "", errors.New("too many open logins")
	}
	// Per-user cap: drop the oldest of that user's tickets.
	n, oldest, oldestK := 0, now, ""
	for k, v := range f.tickets {
		if v.user == user {
			n++
			if v.issued.Before(oldest) {
				oldest, oldestK = v.issued, k
			}
		}
	}
	if n >= feTicketsUser && oldestK != "" {
		delete(f.tickets, oldestK)
	}
	f.tickets[t] = &feTicket{user: user, expires: now.Add(feTicketTTL), issued: now}
	return t, nil
}

func (f *feState) valid(ticket, user string) bool {
	if len(ticket) != 32 {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.tickets[ticket]
	if t == nil {
		return false
	}
	if time.Now().After(t.expires) {
		delete(f.tickets, ticket)
		return false
	}
	return subtle.ConstantTimeCompare([]byte(t.user), []byte(user)) == 1
}

// revokeUser drops every ticket of user (policy revoked, TOTP lockout).
func (f *feState) revokeUser(user string) {
	f.mu.Lock()
	for k, v := range f.tickets {
		if v.user == user {
			delete(f.tickets, k)
		}
	}
	f.mu.Unlock()
}

// ------------------------------------------------------------------ TOTP

func hotp(secret []byte, counter int64) string {
	var c [8]byte
	binary.BigEndian.PutUint64(c[:], uint64(counter))
	mac := hmac.New(sha1.New, secret)
	mac.Write(c[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := (uint32(sum[off]&0x7f) << 24) | (uint32(sum[off+1]) << 16) | (uint32(sum[off+2]) << 8) | uint32(sum[off+3])
	return fmt.Sprintf("%06d", v%1000000)
}

// totpMatch returns the counter whose code equals code, within +-1 step of now.
func totpMatch(secret []byte, code string, now time.Time) (int64, bool) {
	if len(code) != 6 {
		return 0, false
	}
	cur := now.Unix() / totpStep
	found, hit := int64(0), false
	for c := cur - 1; c <= cur+1; c++ { // always evaluate all three: no timing signal
		if subtle.ConstantTimeCompare([]byte(hotp(secret, c)), []byte(code)) == 1 {
			found, hit = c, true
		}
	}
	return found, hit
}

func decodeTOTPSecret(s string) ([]byte, error) {
	s = strings.ToUpper(strings.NewReplacer(" ", "", "-", "", "\n", "", "\r", "", "=", "").Replace(s))
	b, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(s)
	if err != nil || len(b) < 10 {
		return nil, errors.New("invalid TOTP secret (base32, at least 80 bits)")
	}
	return b, nil
}

// readRootFile reads a small file that must be root-owned and not accessible
// to anyone else when secret is true (not writable by others otherwise).
func readRootFile(path string, max int64, secret bool) ([]byte, error) {
	if err := checkTrusted(path, true); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || !fi.Mode().IsRegular() || st.Uid != 0 {
		return nil, fmt.Errorf("%s must be a regular file owned by root", path)
	}
	if secret && fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s must not be accessible to group/others (mode 0600)", path)
	}
	if fi.Size() > max {
		return nil, fmt.Errorf("%s is too large", path)
	}
	buf := make([]byte, fi.Size())
	if _, err := f.ReadAt(buf, 0); err != nil && fi.Size() > 0 {
		return nil, err
	}
	return buf, nil
}

// ------------------------------------------------------------------ requests

func (m *monitor) feSnapshot() (config, *feState) {
	_, g := m.snapshot()
	return g, m.fe
}

// handleProxy serves a request from the front-end (peer uid == front-end uid).
func (m *monitor) handleProxy(fc *framed, hl *hello, cred *syscall.Ucred, req uint64, handle func(*hello, *syscall.Ucred)) {
	p := hl.Proxy
	global, fe := m.feSnapshot()
	if p == nil || !feUserRe.MatchString(p.User) {
		replyErr(fc, "malformed request")
		return
	}
	switch hl.Op {
	case "fe-report":
		if ok, _ := m.allowRate(-1, 20, 40); !ok {
			replyErr(fc, "slow down")
			return
		}
		src := []string{}
		for i, s := range p.Sources {
			if i >= 5 {
				break
			}
			src = append(src, clip(strings.Map(printable, s), 64))
		}
		cnt := p.Count
		if cnt < 0 {
			cnt = 0
		}
		m.audit.log("fe_auth_failures", fields{"count": cnt, "sources": src, "window_s": 60})
		_ = replyOK(fc, okReply{})
		return
	case "fe-keys":
		if ok, _ := m.allowRate(-1, 100, 400); !ok {
			replyErr(fc, "slow down")
			return
		}
		keys := m.feAuthorizedKeys(global, p.User)
		_ = replyOK(fc, okReply{Keys: keys})
		return
	}

	// Everything below acts for a user.
	id, err := lookupIdentityByName(p.User)
	if err != nil || id == nil {
		m.audit.log("fe_denied", fields{"req": req, "user": clip(p.User, 64), "remote": clip(p.Remote, 64), "reason": "unknown user"})
		replyErr(fc, "access denied")
		return
	}
	uid, _ := strconv.Atoi(id.u.Uid)
	if uid == 0 || uid == fe.getUID() {
		m.audit.log("fe_denied", fields{"req": req, "user": id.u.Username, "uid": uid, "remote": clip(p.Remote, 64), "reason": "account not allowed over the front-end"})
		replyErr(fc, "access denied")
		return
	}
	if ok, note := m.allow(uid); !ok {
		if note > 0 {
			m.audit.log("rate_limited", fields{"uid": uid, "user": id.u.Username, "via": "frontend", "refused": note})
		}
		replyErr(fc, "too many requests, slow down")
		return
	}
	attr := fields{"req": req, "op": hl.Op, "peer_uid": uid, "peer_pid": cred.Pid, "via": "frontend",
		"user": id.u.Username, "remote": clip(p.Remote, 64), "fp": clip(p.Fingerprint, 80),
		"key_type": clip(p.KeyType, 64), "method": clip(p.Method, 32)}
	if p.CertID != "" {
		attr["cert_id"], attr["cert_serial"] = clip(p.CertID, 128), p.Serial
	}
	if hl.Name != "" {
		attr["session"] = hl.Name
	}

	switch hl.Op {
	case "fe-gate", "fe-totp":
		m.feLogin(fc, hl, id, uid, global, fe, req, attr)
		return
	case "new", "attach", "list", "kill", "freeze", "thaw", "stop",
		"rename", "notify", "send-keys", "layout-set", "layout-get", "layout-list", "layout-del":
		if !fe.valid(p.Ticket, id.u.Username) {
			m.audit.log("fe_denied", fields{"req": req, "user": id.u.Username, "remote": clip(p.Remote, 64), "reason": "no valid ticket"})
			replyErr(fc, "access denied")
			return
		}
		m.audit.log("request", attr)
		hl2 := *hl
		hl2.Proxy = nil
		cred2 := *cred
		cred2.Uid, cred2.Gid = uint32(uid), id.gid
		handle(&hl2, &cred2)
		return
	}
	replyErr(fc, "operation not permitted over the front-end")
}

func printable(r rune) rune {
	if r < 0x20 || r == 0x7f {
		return '?'
	}
	return r
}

// feLogin implements fe-gate (policy; no second factor configured) and
// fe-totp (policy + code). Both end in a ticket or a denial.
func (m *monitor) feLogin(fc *framed, hl *hello, id *identity, uid int, global config, fe *feState, req uint64, attr fields) {
	p := hl.Proxy
	user := id.u.Username
	deny := func(reason, msg string) {
		f := fields{}
		for k, v := range attr {
			f[k] = v
		}
		f["reason"] = reason
		m.audit.log("fe_denied", f)
		if msg == "" {
			msg = "access denied"
		}
		replyErr(fc, msg)
	}
	cf, g2 := m.snapshot()
	cfg := cf.resolve(user, id.groups)
	m.setRate(uid, cfg.RateLimit)
	if !cfg.Enabled {
		deny("account disabled by policy", "")
		return
	}
	if g2.AccessGroup != "" && !contains(id.groups, g2.AccessGroup) {
		deny("not a member of access group "+g2.AccessGroup, "")
		return
	}
	if !interactiveShell(cfg.Shell) && !interactiveShell(lookupShell(uid)) {
		deny("account has no interactive shell", "")
		return
	}

	method := "publickey"
	if global.FrontendTOTP {
		if hl.Op == "fe-gate" {
			// Asking for a ticket without the second factor: never.
			deny("second factor required", "")
			return
		}
		if err := m.feCheckTOTP(user, p.Code, global); err != nil {
			f := fields{}
			for k, v := range attr {
				f[k] = v
			}
			f["reason"] = err.reason
			m.audit.log(err.event, f)
			if err.revoke {
				fe.revokeUser(user)
			}
			replyErr(fc, "access denied")
			return
		}
		method = "publickey+totp"
	} else if hl.Op == "fe-totp" {
		deny("second factor not enabled", "")
		return
	}
	t, err := fe.issue(user)
	if err != nil {
		deny(err.Error(), "")
		return
	}
	attr["method"] = method
	attr["ticket_id"] = t[:8]
	m.audit.log("fe_login", attr)
	_ = replyOK(fc, okReply{Ticket: t})
}

type totpFail struct {
	event, reason string
	revoke        bool
}

// feCheckTOTP verifies a code with replay protection and lockout. A nil
// return means the code is good (and now used up).
func (m *monitor) feCheckTOTP(user, code string, global config) *totpFail {
	if global.FrontendTOTPDir == "none" || !feUserRe.MatchString(user) {
		return &totpFail{"fe_denied", "second factor not available", false}
	}
	fe := m.fe
	now := time.Now()
	fe.mu.Lock()
	st := fe.totp[user]
	if st == nil {
		if len(fe.totp) > 100000 {
			fe.mu.Unlock()
			return &totpFail{"fe_denied", "too many accounts tracked", false}
		}
		st = &totpState{}
		fe.totp[user] = st
	}
	if now.Before(st.locked) {
		fe.mu.Unlock()
		return &totpFail{"fe_totp_failed", "account temporarily locked", false}
	}
	fe.mu.Unlock()
	st.mu.Lock()
	defer st.mu.Unlock()
	fe.mu.Lock()
	if now.Before(st.locked) {
		fe.mu.Unlock()
		return &totpFail{"fe_totp_failed", "account temporarily locked", false}
	}
	fe.mu.Unlock()

	raw, err := readRootFile(filepath.Join(global.FrontendTOTPDir, user), 4096, true)
	var secret []byte
	if err == nil {
		secret, err = decodeTOTPSecret(string(raw))
	}
	if err != nil {
		return &totpFail{"fe_denied", "no usable TOTP secret for this account", false}
	}
	counter, ok := totpMatch(secret, code, now)

	fe.mu.Lock()
	defer fe.mu.Unlock()
	if ok && counter <= st.last {
		ok = false // replay of a code already used
	}
	if ok {
		st.last, st.fails = counter, 0
		return nil
	}
	st.fails++
	if st.fails >= totpMaxFails {
		st.fails, st.locked = 0, now.Add(totpLockFor)
		return &totpFail{"fe_totp_locked", fmt.Sprintf("%d wrong codes: locked for %s", totpMaxFails, totpLockFor), true}
	}
	return &totpFail{"fe_totp_failed", "wrong code", false}
}

// feAuthorizedKeys returns the user's authorized_keys lines (root-owned file in
// frontend_authorized_keys_dir). Missing or untrusted files give no keys.
func (m *monitor) feAuthorizedKeys(global config, user string) []string {
	if global.FrontendKeysDir == "none" || !feUserRe.MatchString(user) {
		return nil
	}
	raw, err := readRootFile(filepath.Join(global.FrontendKeysDir, user), feKeysMaxBytes, false)
	if err != nil {
		if !os.IsNotExist(errors.Unwrap(err)) && !os.IsNotExist(err) {
			m.audit.log("fe_keys_error", fields{"user": user, "error": clip(err.Error(), 200)})
		}
		return nil
	}
	var out []string
	for _, ln := range strings.Split(string(raw), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") || len(ln) > 2048 {
			continue
		}
		out = append(out, ln)
		if len(out) >= feKeysMaxLines {
			break
		}
	}
	return out
}

func lookupIdentityByName(name string) (*identity, error) {
	type res struct {
		uid int
		err error
	}
	ch := make(chan res, 1)
	go func() {
		u, err := lookupUserName(name)
		if err != nil {
			ch <- res{0, err}
			return
		}
		uid, err := strconv.Atoi(u)
		ch <- res{uid, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		id, err := lookupIdentity(r.uid)
		if err != nil {
			return nil, err
		}
		if id.u.Username != name { // exact name only: no aliases, no case folding
			return nil, errors.New("name mismatch")
		}
		return id, nil
	case <-time.After(5 * time.Second):
		return nil, errors.New("user lookup timed out")
	}
}

// ---------------------------------------------------------- carried over a re-exec

type feStateDump struct {
	Tickets map[string]feTicketDump `json:"tickets"`
	TOTP    map[string]totpDump     `json:"totp"`
}

type feTicketDump struct {
	User    string    `json:"user"`
	Expires time.Time `json:"expires"`
	Issued  time.Time `json:"issued"`
}

type totpDump struct {
	Last   int64     `json:"last"`
	Fails  int       `json:"fails"`
	Locked time.Time `json:"locked"`
}

func (f *feState) export() feStateDump {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := feStateDump{Tickets: map[string]feTicketDump{}, TOTP: map[string]totpDump{}}
	for k, t := range f.tickets {
		d.Tickets[k] = feTicketDump{t.user, t.expires, t.issued}
	}
	for u, s := range f.totp {
		s.mu.Lock()
		d.TOTP[u] = totpDump{s.last, s.fails, s.locked}
		s.mu.Unlock()
	}
	return d
}

func (f *feState) restore(d feStateDump) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	for k, t := range d.Tickets {
		if len(k) == 32 && now.Before(t.Expires) {
			f.tickets[k] = &feTicket{user: t.User, expires: t.Expires, issued: t.Issued}
		}
	}
	for u, s := range d.TOTP {
		f.totp[u] = &totpState{last: s.Last, fails: s.Fails, locked: s.Locked}
	}
}
