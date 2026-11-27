//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// monitor.go: the privileged monitor (production mode).
//
// The monitor is the only part that runs as root and the only one that
// writes audit data. It
//   - authenticates every local client with SO_PEERCRED (the kernel vouches
//     for the uid; no passwords, no tokens),
//   - applies policy (access group, enabled, session limits, allowed commands),
//   - starts one host per user with that user's credentials and a clean
//     environment, over a private control channel,
//   - hands authenticated client connections to the host (SCM_RIGHTS): the
//     monitor is never in the data path,
//   - writes the hash-chained audit log and the session recordings, from
//     events and data the hosts stream to it.
//
// How the user got to the machine (SSH certificate, FIDO2, TOTP...) is
// sshd's business: ptscry sits behind it. The monitor records the kernel's
// audit login uid / session id of the caller so every action is attributable
// to the authenticated login, even after su/sudo.

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
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	defaultAuditLog  = "/var/log/ptscry/audit.log"
	defaultRecordTop = "/var/log/ptscry/rec"
	maxHandshakes    = 128 // concurrent unauthenticated requests, all users
	maxHandshakesUID = 32  // ... per uid (a controller may start many jobs at once)
)

type msess struct {
	Name     string
	PID      int
	Created  time.Time
	Cmd      string
	Attached int
}

type mhost struct {
	m    *monitor
	uid  int
	user string
	pid  int
	proc *os.Process
	ctl  *ctlConn
	cfg  config // policy the host was started with

	gid   uint32 // identity as the monitor resolved it
	gids  []uint32
	home  string
	shell string

	mu       sync.Mutex
	sessions map[string]*msess
	reserved map[uint64]time.Time
	launches map[string]*launch // session processes (launcher mode)
	recs     map[string]*recorder
	exited   chan struct{}

	frozen   atomic.Bool   // control channel paused for a monitor re-exec
	paused   atomic.Bool   // ... and confirmed stopped
	loopDone chan struct{} // closed when the reader loop returns

	evMu           sync.Mutex
	evSec          int64
	evCount, evHot int
}

type monitor struct {
	cfgPath string
	exe     string

	cfgMu  sync.RWMutex
	cf     *configFile
	global config

	audit *chainAudit
	ln    *net.UnixListener

	mu       sync.Mutex
	hosts    map[int]*mhost
	inflight map[int]int
	stopping bool
	reqSeq   atomic.Uint64
	sem      chan struct{}

	spawning atomic.Int32 // launcher starts in progress
	frozen   atomic.Bool  // a re-exec is under way: no new work
	stopReq  atomic.Bool  // a stop was requested: a re-exec must not go ahead
	reexecMu sync.Mutex
	// freezeMu: every request handler holds it for reading while it works; a
	// re-exec takes it for writing, which waits for the handlers to finish and
	// keeps new ones from starting.
	freezeMu      sync.RWMutex
	freezeWait    time.Duration // test hook; 0 = 45 s
	freezeHeld    bool
	pausedHosts   []*mhost
	pausedLaunchs []*launch

	resMu sync.Mutex // serialises session-limit decisions
	rl    map[int]*bucket

	fe         *feState // SSH front-end bookkeeping (tickets, TOTP state)
	feWake     chan struct{}
	feMu       sync.Mutex
	fePID      int
	feInitJSON []byte
	feMon      *os.File // monitor end of the front-end's control socket
	feUID      int
	feStarted  time.Time  // when the current front-end process was started
	feAdopt    *adoptedFE // front-end carried over a re-exec, to supervise instead of starting
}

// Per-uid request rate limit: every request is audited (fsync), so an
// unbounded stream of them would be a way to fill the disk.
const (
	rateBurst  = 40
	ratePerSec = 10.0
	// A host whose events keep exceeding this rate is killed.
	hostEventRate = 500
	// A session slot reserved by the monitor but never used is released after this.
	reserveTTL = 30 * time.Second
	// Policy is re-checked against running hosts this often.
	revalidateEvery = time.Minute
)

type bucket struct {
	tokens   float64
	last     time.Time
	denied   int
	perSec   float64 // per-user limit from policy (0 = default)
	lastNote time.Time
}

// allow takes a token for uid. When denied it returns, at most every 10 s, the
// number of requests refused since the last note (to be audited once).
func (m *monitor) allow(uid int) (ok bool, note int) {
	m.mu.Lock()
	rate := float64(ratePerSec)
	if b := m.rl[uid]; b != nil && b.perSec > 0 {
		rate = b.perSec
	}
	m.mu.Unlock()
	return m.allowRate(uid, rate, rate*4)
}

// setRate records the policy's rate_limit for uid (requests/s, burst 4x). It
// takes effect from the next request; until a user's policy has been resolved
// the default applies.
func (m *monitor) setRate(uid, perSec int) {
	if uid == 0 || perSec < 1 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rl == nil {
		m.rl = map[int]*bucket{}
	}
	b := m.rl[uid]
	if b == nil {
		b = &bucket{tokens: float64(perSec) * 4, last: time.Now()}
		m.rl[uid] = b
	}
	b.perSec = float64(perSec)
}

func (m *monitor) allowRate(uid int, perSec, burst float64) (ok bool, note int) {
	if uid == 0 {
		return true, 0
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rl == nil {
		m.rl = map[int]*bucket{}
	}
	if len(m.rl) > 4096 {
		for k, b := range m.rl {
			if now.Sub(b.last) > time.Minute {
				delete(m.rl, k)
			}
		}
	}
	b := m.rl[uid]
	if b == nil {
		b = &bucket{tokens: burst, last: now}
		m.rl[uid] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * perSec
	if b.tokens > burst {
		b.tokens = burst
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	b.denied++
	if now.Sub(b.lastNote) >= 10*time.Second {
		note, b.denied, b.lastNote = b.denied, 0, now
	}
	return false, note
}

// checkTrusted refuses a file, or any directory above it, that someone other
// than root could change: that would let them replace the binary or the
// configuration the root monitor trusts. Symbolic links are resolved first.
// Sticky world-writable directories such as /tmp are accepted (others cannot
// rename or remove root's entries there), and so is a group-writable one
// whose group has no members (Debian's /usr/local is root:staff 2775).
func checkTrusted(path string, mustExist bool) error {
	return checkTrustedBy(path, mustExist, 0)
}

// checkTrustedBy is checkTrusted with more acceptable owners: for a
// single-user installation (standalone mode, no root monitor) the user's own
// files are as trusted as root's, as long as nobody else can change them.
func checkTrustedBy(path string, mustExist bool, owners ...uint32) error {
	ownerOK := func(uid uint32) bool {
		for _, o := range owners {
			if uid == o {
				return true
			}
		}
		return false
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		if os.IsNotExist(err) && !mustExist {
			return nil
		}
		return err
	}
	for p := real; ; p = filepath.Dir(p) {
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || !ownerOK(st.Uid) {
			if len(owners) == 1 && owners[0] == 0 {
				return fmt.Errorf("%s is not owned by root", p)
			}
			return fmt.Errorf("%s is owned by uid %d (only root or you should own it)", p, st.Uid)
		}
		perm := fi.Mode().Perm()
		sticky := fi.IsDir() && fi.Mode()&os.ModeSticky != 0
		if perm&0o002 != 0 && !sticky {
			return fmt.Errorf("%s is writable by everyone (mode %v)", p, perm)
		}
		if perm&0o020 != 0 && !sticky && st.Gid != 0 && groupHasMembers(st.Gid) {
			return fmt.Errorf("%s is writable by group %d, which has members (mode %v)", p, st.Gid, perm)
		}
		if p == "/" || p == "." {
			return nil
		}
	}
}

// groupHasMembers reports whether a local group lists any member in /etc/group.
// On error it answers true: when in doubt, refuse.
func groupHasMembers(gid uint32) bool {
	b, err := os.ReadFile("/etc/group")
	if err != nil {
		return true
	}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Split(l, ":")
		if len(f) >= 4 && f[2] == strconv.FormatUint(uint64(gid), 10) {
			return strings.TrimSpace(f[3]) != ""
		}
	}
	return false // no local group by that gid: nobody can be in it
}

// checkPAMService refuses to start with a PAM setup that cannot work: a missing
// service file would make PAM fall back to "other", which usually denies all.
func checkPAMService(svc string) error {
	if svc == "off" {
		return nil
	}
	if !launcherAvailable {
		return errors.New("pam_service is set but this binary was built without cgo: rebuild with `make build`, or set pam_service = off")
	}
	if err := pamAvailable(); err != nil {
		return fmt.Errorf("pam_service is set but %w (install libpam, or set pam_service = off)", err)
	}
	for _, d := range []string{"/etc/pam.d/", "/usr/lib/pam.d/", "/usr/etc/pam.d/"} {
		if _, err := os.Stat(d + svc); err == nil {
			return checkTrusted(d+svc, true)
		}
	}
	return fmt.Errorf("PAM service file /etc/pam.d/%s not found (install contrib/pam.d/ptscry, or set pam_service = off)", svc)
}

// ensureRootDir creates dir if needed and verifies it is a real directory
// owned by root and not writable by group or others: audit data, recordings
// and the monitor socket must not live where a user could swap them.
func ensureRootDir(dir string, mode os.FileMode) error {
	if err := os.MkdirAll(dir, mode); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok || st.Uid != 0 || fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s must be a directory owned by root and not writable by group or others", dir)
	}
	return nil
}

func cmdMonitor(args []string) error {
	fs := flag.NewFlagSet("monitor", flag.ContinueOnError)
	cpath := fs.String("c", defaultConfigPath, "configuration file")
	testOnly := fs.Bool("t", false, "test the configuration and the installation, then exit (like sshd -t)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if os.Getuid() != 0 {
		return errors.New("the monitor must run as root")
	}
	if *testOnly {
		return monitorSelfTest(*cpath)
	}
	if err := setNotDumpable(); err != nil {
		return fmt.Errorf("hardening: %w", err)
	}
	// First of all, before anything can start a process: if we are the new image
	// of a re-exec, take over the inherited descriptors (and make them private again).
	resumed, err := takeResume()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := checkTrusted(exe, true); err != nil {
		return fmt.Errorf("refusing to run: %w", err)
	}
	if err := checkTrusted(*cpath, false); err != nil {
		return fmt.Errorf("refusing to run: %w", err)
	}
	cf, err := parseConfigFile(*cpath)
	if err != nil {
		return err
	}
	m := &monitor{
		cfgPath: *cpath, exe: exe,
		hosts: map[int]*mhost{}, inflight: map[int]int{}, fe: newFeState(), feWake: make(chan struct{}, 1),
		sem: make(chan struct{}, maxHandshakes),
	}
	m.cf, m.global = cf, cf.resolve("", nil)

	auditPath := m.global.AuditLog
	if auditPath == "" {
		auditPath = defaultAuditLog
	}
	if auditPath == "off" {
		return errors.New("audit_log = off is not allowed for the monitor")
	}
	if err := ensureRootDir(filepath.Dir(auditPath), 0o750); err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	if m.audit, err = openChainAudit(auditPath, m.global.AuditSyslog, m.global.AuditGroup); err != nil {
		return fmt.Errorf("audit: %w", err)
	}

	if err := checkPAMService(m.global.PAMService); err != nil {
		return err
	}

	sock := m.global.MonitorSocket
	if err := ensureRootDir(filepath.Dir(sock), 0o755); err != nil {
		return fmt.Errorf("monitor socket: %w", err)
	}
	var ln *net.UnixListener
	if resumed != nil {
		// The socket is the previous image's, still bound and with clients queued on it.
		f := os.NewFile(uintptr(resumed.Listener), "listener")
		l, err := net.FileListener(f)
		f.Close()
		if err != nil {
			return fmt.Errorf("listener: %w", err)
		}
		var ok bool
		if ln, ok = l.(*net.UnixListener); !ok {
			return errors.New("listener is not a unix socket")
		}
	} else {
		if c, err := net.DialTimeout("unix", sock, time.Second); err == nil {
			c.Close()
			return errors.New("another monitor is already running")
		}
		if fi, err := os.Lstat(sock); err == nil && fi.Mode()&os.ModeSocket != 0 {
			_ = os.Remove(sock)
		}
		old := syscall.Umask(0)
		ln, err = net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
		syscall.Umask(old)
		if err != nil {
			return err
		}
		// Any local user may connect; the kernel-provided credentials decide.
		if err := os.Chmod(sock, 0o666); err != nil {
			return err
		}
	}
	ln.SetUnlinkOnClose(true)
	m.ln = ln

	m.audit.log("monitor_start", fields{"pid": os.Getpid(), "version": progVersion, "resumed": resumed != nil,
		"config": *cpath, "socket": sock, "yama_ptrace_scope": readTrim("/proc/sys/kernel/yama/ptrace_scope"),
		"access_group": m.global.AccessGroup, "admin_group": m.global.AdminGroup, "allow_root": m.global.AllowRoot,
		"pam_service": m.global.PAMService, "frontend": m.global.Frontend})

	if resumed != nil {
		if err := m.resume(resumed); err != nil {
			m.audit.log("monitor_resume_failed", fields{"error": clip(err.Error(), 300)})
			return fmt.Errorf("resuming after re-exec: %w", err)
		}
	}
	sig := make(chan os.Signal, 4)
	signal.Notify(sig, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT, syscall.SIGUSR2)
	go m.revalidateLoop()
	go m.frontendLoop()
	go func() {
		for s := range sig {
			if s == syscall.SIGHUP {
				m.reload()
				continue
			}
			if s == syscall.SIGUSR2 {
				go func() { _ = m.reexec("signal") }()
				continue
			}
			m.stopReq.Store(true)
			m.shutdown("signal " + s.String())
			return
		}
	}()

	for {
		c, err := ln.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				break
			}
			time.Sleep(20 * time.Millisecond) // also while paused for a re-exec: the backlog keeps the callers
			continue
		}
		select {
		case m.sem <- struct{}{}:
			go func() {
				defer func() { <-m.sem }()
				m.handleClient(c)
			}()
		default:
			c.Close() // overloaded: shed load rather than queue
		}
	}
	// ln closed by shutdown(): wait for it to finish.
	time.Sleep(200 * time.Millisecond)
	return nil
}

func readTrim(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return "n/a"
	}
	return strings.TrimSpace(string(b))
}

func (m *monitor) reload() {
	if m.frozen.Load() {
		m.audit.log("config_reload_failed", fields{"error": "a re-exec is in progress; try again"})
		return
	}
	if err := checkTrusted(m.cfgPath, false); err != nil {
		m.audit.log("config_reload_failed", fields{"error": err.Error()})
		return
	}
	cf, err := parseConfigFile(m.cfgPath)
	if err != nil {
		m.audit.log("config_reload_failed", fields{"error": err.Error()})
		return
	}
	ng := cf.resolve("", nil)
	if err := checkPAMService(ng.PAMService); err != nil {
		m.audit.log("config_reload_failed", fields{"error": err.Error()})
		return
	}
	if ng.Frontend {
		if _, _, err := buildFeInit(ng); err != nil {
			m.audit.log("config_reload_failed", fields{"error": "frontend: " + clip(err.Error(), 300)})
			return
		}
	}
	m.cfgMu.Lock()
	m.cf, m.global = cf, cf.resolve("", nil)
	m.cfgMu.Unlock()
	m.audit.setGroup(m.global.AuditGroup)
	if err := m.audit.reopen(); err != nil {
		fmt.Fprintf(os.Stderr, "ptscry: audit reopen failed: %v\n", err)
	}
	m.audit.log("config_reloaded", fields{"config": m.cfgPath, "pam_service": m.global.PAMService,
		"access_group": m.global.AccessGroup, "allow_root": m.global.AllowRoot})
	go m.revalidate()
	select {
	case m.feWake <- struct{}{}:
	default:
	}
}

func (m *monitor) revalidateLoop() {
	for range time.Tick(revalidateEvery) {
		m.revalidate()
	}
}

// revalidate applies the current policy to the hosts that are already running:
// a user who has been disabled, removed from access_group or deleted loses
// their sessions (a revoked right must not outlive the policy change). A failed
// directory lookup changes nothing: an SSSD outage must not kill everyone.
func (m *monitor) revalidate() {
	m.mu.Lock()
	hs := make([]*mhost, 0, len(m.hosts))
	for _, h := range m.hosts {
		hs = append(hs, h)
	}
	m.mu.Unlock()
	cf, global := m.snapshot()
	for _, h := range hs {
		id, err := lookupIdentity(h.uid)
		reason := ""
		if err != nil {
			var unk user.UnknownUserIdError
			if errors.As(err, &unk) {
				reason = "account no longer exists"
			}
		} else {
			cfg := cf.resolve(id.u.Username, id.groups)
			switch {
			case !cfg.Enabled:
				reason = "account disabled by policy"
			case !global.mayAccess(h.uid, id.groups):
				reason = "no longer a member of access group " + global.AccessGroup
			}
		}
		if reason == "" {
			continue
		}
		m.audit.log("policy_revoked", fields{"uid": h.uid, "user": h.user, "reason": reason})
		b, _ := json.Marshal(map[string]string{"reason": "policy: " + reason})
		_ = h.ctl.send(ctlShutdown, b)
		afterTracked(10*time.Second, h.terminateAll)
	}
}

func (m *monitor) snapshot() (*configFile, config) {
	m.cfgMu.RLock()
	defer m.cfgMu.RUnlock()
	return m.cf, m.global
}

func (m *monitor) shutdown(reason string) {
	m.stopReq.Store(true)
	m.mu.Lock()
	m.stopping = true
	hs := make([]*mhost, 0, len(m.hosts))
	for _, h := range m.hosts {
		hs = append(hs, h)
	}
	m.mu.Unlock()
	m.audit.log("monitor_shutdown", fields{"reason": reason})
	_ = m.ln.Close()
	m.stopFrontend()
	for _, h := range hs {
		b, _ := json.Marshal(map[string]string{"reason": "monitor shutdown"})
		_ = h.ctl.send(ctlShutdown, b)
	}
	deadline := time.After(15 * time.Second)
	for _, h := range hs {
		select {
		case <-h.exited:
		case <-deadline:
		}
	}
	for _, h := range hs {
		h.terminateAll()
	}
	m.feMu.Lock()
	fepid := m.fePID
	m.feMu.Unlock()
	for i := 0; i < 40 && fepid > 0 && syscall.Kill(fepid, 0) == nil; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	m.audit.close()
	os.Exit(0)
}

// ------------------------------------------------------------------ requests

func (m *monitor) enterHandshake(uid int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopping || m.inflight[uid] >= maxHandshakesUID {
		return false
	}
	m.inflight[uid]++
	return true
}

func (m *monitor) leaveHandshake(uid int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inflight[uid]--; m.inflight[uid] <= 0 {
		delete(m.inflight, uid)
	}
}

func validName(s string) bool { return s == "" || nameRe.MatchString(s) }

// validOpFields checks the fields of the small session commands: what each
// one needs, and nothing it does not use.
func validOpFields(hl *hello) error {
	if hl.NewName != "" && (hl.Op != "rename" || !nameRe.MatchString(hl.NewName)) {
		return errors.New("invalid new name")
	}
	if hl.Text != "" && !opsWithText[hl.Op] {
		return errors.New("unexpected text")
	}
	if len(hl.Text) > maxOpText {
		return errors.New("text too long")
	}
	switch hl.Op {
	case "rename":
		if hl.Name == "" || hl.NewName == "" {
			return errors.New("rename needs the old and the new name")
		}
	case "notify", "send-keys":
		if hl.Name == "" || hl.Text == "" {
			return errors.New("a session name and a text are needed")
		}
	case "layout-set", "layout-get", "layout-del":
		if !validLayoutName(hl.Name) {
			return errors.New("invalid layout name")
		}
		if hl.Op == "layout-set" && hl.Text == "" {
			return errors.New("empty layout")
		}
	}
	switch hl.Op {
	case "rename", "notify", "send-keys", "layout-set", "layout-get", "layout-list", "layout-del":
		if len(hl.Cmd) > 0 || len(hl.Env) > 0 || hl.ReadOnly || hl.Create || hl.Detached || hl.Job || hl.Remove {
			return errors.New("unexpected fields in request")
		}
	}
	return nil
}

func validHello(hl *hello) error {
	switch hl.Op {
	case "new", "attach", "list", "kill", "freeze", "thaw", "stop", "admin-list", "admin-kill", "admin-stop", "admin-reexec",
		"fe-keys", "fe-gate", "fe-totp", "fe-report",
		"rename", "notify", "send-keys", "layout-set", "layout-get", "layout-list", "layout-del":
	default:
		return fmt.Errorf("unknown operation %q", hl.Op)
	}
	if !validName(hl.Name) {
		return errors.New("invalid session name")
	}
	if err := validOpFields(hl); err != nil {
		return err
	}
	if hl.User != "" && (len(hl.User) > 128 || strings.ContainsAny(hl.User, "/\x00")) {
		return errors.New("invalid user")
	}
	if p := hl.Proxy; p != nil {
		if len(p.User) > 64 || len(p.Remote) > 128 || len(p.Fingerprint) > 128 || len(p.KeyType) > 64 ||
			len(p.CertID) > 256 || len(p.Method) > 32 || len(p.Ticket) > 64 || len(p.Code) > 16 || len(p.Sources) > 16 {
			return errors.New("invalid proxy data")
		}
		if strings.HasPrefix(hl.Op, "admin-") {
			return errors.New("administration is not available over the front-end")
		}
	} else if strings.HasPrefix(hl.Op, "fe-") {
		return errors.New("invalid request")
	}
	if len(hl.Cmd) > 64 {
		return errors.New("too many command arguments")
	}
	total := 0
	for _, a := range hl.Cmd {
		if len(a) > 4096 || strings.ContainsRune(a, 0) {
			return errors.New("invalid command argument")
		}
		total += len(a)
	}
	if total > 32*1024 { // must also fit in one control message to the host
		return errors.New("command line too long")
	}
	if len(hl.Env) > 64 {
		return errors.New("too many environment entries")
	}
	for k, v := range hl.Env {
		if len(k) > 64 || len(v) > 1024 {
			return errors.New("environment entry too long")
		}
	}
	return nil
}

func (m *monitor) handleClient(uc *net.UnixConn) {
	defer uc.Close()
	m.freezeMu.RLock()
	defer m.freezeMu.RUnlock()
	cred, err := peerCred(uc)
	if err != nil {
		return
	}
	uid := int(cred.Uid)
	if !m.enterHandshake(uid) {
		return
	}
	defer m.leaveHandshake(uid)

	fc := newFramed(uc)
	_ = uc.SetReadDeadline(time.Now().Add(5 * time.Second))
	t, p, err := fc.read()
	_ = uc.SetReadDeadline(time.Time{})
	if err != nil || t != msgHello {
		return
	}
	isFE := m.fe.getUID() != 0 && uid == m.fe.getUID()
	if ok, note := m.allow(uid); !ok && !isFE { // front-end requests are limited per target user
		if note > 0 {
			m.audit.log("rate_limited", fields{"uid": uid, "peer_pid": cred.Pid, "refused": note})
		}
		replyErr(fc, "too many requests, slow down")
		return
	}
	var hl hello
	if err := json.Unmarshal(p, &hl); err != nil {
		replyErr(fc, "malformed request")
		return
	}
	if err := validHello(&hl); err != nil {
		replyErr(fc, err.Error())
		return
	}
	if !m.audit.probe() {
		replyErr(fc, "audit log unavailable: refusing to operate")
		return
	}
	req := m.reqSeq.Add(1)
	if hl.Proxy != nil || strings.HasPrefix(hl.Op, "fe-") {
		if !isFE {
			m.audit.log("fe_denied", fields{"req": req, "peer_uid": uid, "peer_pid": cred.Pid, "reason": "not the front-end"})
			replyErr(fc, "access denied")
			return
		}
		m.handleProxy(fc, &hl, cred, req, func(h *hello, c *syscall.Ucred) { m.handleUser(fc, uc, h, c, req) })
		return
	}
	attr := procAttr(cred.Pid)
	attr["req"], attr["op"], attr["peer_uid"], attr["peer_pid"] = req, hl.Op, uid, cred.Pid
	if hl.Name != "" {
		attr["session"] = hl.Name
	}
	m.audit.log("request", attr)

	if strings.HasPrefix(hl.Op, "admin-") {
		m.handleAdmin(fc, &hl, cred, req)
		return
	}
	m.handleUser(fc, uc, &hl, cred, req)
}

func (m *monitor) deny(fc *framed, req uint64, uid int, user, reason, msg string) {
	m.audit.log("access_denied", fields{"req": req, "uid": uid, "user": user, "reason": reason})
	if msg == "" {
		msg = "access denied"
	}
	replyErr(fc, msg)
}

func (m *monitor) handleUser(fc *framed, uc *net.UnixConn, hl *hello, cred *syscall.Ucred, req uint64) {
	uid := int(cred.Uid)
	id, err := lookupIdentity(uid)
	if err != nil {
		m.deny(fc, req, uid, "", "identity lookup failed: "+err.Error(), "")
		return
	}
	user := id.u.Username
	cf, global := m.snapshot()
	cfg := cf.resolve(user, id.groups)
	m.setRate(uid, cfg.RateLimit)
	if !cfg.Enabled {
		m.deny(fc, req, uid, user, "account disabled by policy", "")
		return
	}
	if !global.mayAccess(uid, id.groups) {
		m.deny(fc, req, uid, user, "not a member of access group "+global.AccessGroup, "")
		return
	}

	switch hl.Op {
	case "list", "kill", "freeze", "thaw", "stop", "rename", "notify", "send-keys":
		mh := m.getHost(uid)
		if mh == nil {
			switch hl.Op {
			case "list":
				_ = replyOK(fc, okReply{})
			case "kill", "freeze", "thaw", "rename", "notify", "send-keys":
				replyErr(fc, fmt.Sprintf("no such session: %q", hl.Name))
			default:
				_ = replyOK(fc, okReply{Message: "no host running"})
			}
			return
		}
		if err := mh.handoff(uc, hl, cred, req); err != nil {
			replyErr(fc, "host unavailable, retry")
		}
		return
	}

	// new / attach
	if len(hl.Cmd) > 0 && !cfg.AllowCommand {
		m.deny(fc, req, uid, user, "allow_command = no", "running arbitrary commands is disabled by configuration")
		return
	}
	shell := cfg.Shell
	if shell == "" {
		shell = lookupShell(uid)
	}
	if !interactiveShell(shell) {
		m.deny(fc, req, uid, user, "account has no interactive shell", "")
		return
	}
	if strings.HasPrefix(hl.Op, "layout-") {
		// Layouts live in the user's home and outlive the host: the host (the
		// user's own process, never the monitor) reads and writes them, so it
		// is started if needed; with no sessions it goes away again.
		mh := m.getHost(uid)
		if mh == nil {
			if mh, err = m.spawnHost(id, cfg, shell); err != nil {
				m.audit.log("host_spawn_failed", fields{"req": req, "uid": uid, "user": user, "error": err.Error()})
				replyErr(fc, "cannot start session host")
				return
			}
		}
		if err := mh.handoff(uc, hl, cred, req); err != nil {
			replyErr(fc, "host unavailable, retry")
		}
		return
	}
	needsCreate := hl.Op == "new" || (hl.Op == "attach" && hl.Create && hl.Name != "")
	mh := m.getHost(uid)
	if mh == nil {
		if !needsCreate {
			replyErr(fc, "no such session: none exist")
			return
		}
		if mh, err = m.spawnHost(id, cfg, shell); err != nil {
			m.audit.log("host_spawn_failed", fields{"req": req, "uid": uid, "user": user, "error": err.Error()})
			replyErr(fc, "cannot start session host")
			return
		}
	}
	mh.mu.Lock()
	if hl.Name != "" && mh.sessions[hl.Name] != nil {
		needsCreate = false
	}
	mh.mu.Unlock()
	if needsCreate {
		// One decision at a time, so concurrent requests cannot overshoot a limit.
		m.resMu.Lock()
		own, total := m.sessionCounts(mh)
		switch {
		case cfg.MaxSessions > 0 && own >= cfg.MaxSessions:
			m.resMu.Unlock()
			m.audit.log("limit_reached", fields{"req": req, "uid": uid, "user": user,
				"limit": cfg.MaxSessions, "sessions": own})
			replyErr(fc, fmt.Sprintf("session limit reached (%d)", cfg.MaxSessions))
			return
		case global.MaxTotalSessions > 0 && total >= global.MaxTotalSessions:
			m.resMu.Unlock()
			m.audit.log("limit_reached", fields{"req": req, "uid": uid, "user": user,
				"scope": "total", "limit": global.MaxTotalSessions, "sessions": total})
			replyErr(fc, "the server-wide session limit has been reached")
			return
		}
		mh.mu.Lock()
		mh.reserved[req] = time.Now()
		mh.mu.Unlock()
		m.resMu.Unlock()
	}
	if err := mh.handoff(uc, hl, cred, req); err != nil {
		mh.release(req)
		replyErr(fc, "host unavailable, retry")
	}
}

// sessionCounts returns the sessions (plus pending reservations) of mh and of
// all hosts together. Reservations nobody claimed for reserveTTL are dropped.
func (m *monitor) sessionCounts(mh *mhost) (own, total int) {
	m.mu.Lock()
	hs := make([]*mhost, 0, len(m.hosts))
	for _, h := range m.hosts {
		hs = append(hs, h)
	}
	m.mu.Unlock()
	now := time.Now()
	for _, h := range hs {
		h.mu.Lock()
		for r, t := range h.reserved {
			if now.Sub(t) > reserveTTL {
				delete(h.reserved, r)
			}
		}
		n := len(h.sessions) + len(h.reserved)
		h.mu.Unlock()
		total += n
		if h == mh {
			own = n
		}
	}
	return own, total
}

func (m *monitor) getHost(uid int) *mhost {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hosts[uid]
}

func (mh *mhost) release(req uint64) {
	mh.mu.Lock()
	delete(mh.reserved, req)
	mh.mu.Unlock()
}

// handoff passes the client connection to the host. The monitor's own copy
// is closed by the caller; from here on the data does not touch the monitor.
func (mh *mhost) handoff(uc *net.UnixConn, hl *hello, cred *syscall.Ucred, req uint64) error {
	b, _ := json.Marshal(clientMsg{Hello: *hl, Req: req, PID: cred.Pid, UID: cred.Uid})
	f, err := uc.File()
	if err != nil {
		return err
	}
	defer f.Close()
	return mh.ctl.sendFD(ctlClient, b, f)
}

// ------------------------------------------------------------------ hosts

func (m *monitor) spawnHost(id *identity, cfg config, shell string) (*mhost, error) {
	uid, _ := strconv.Atoi(id.u.Uid)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopping {
		return nil, errors.New("monitor is stopping")
	}
	if mh := m.hosts[uid]; mh != nil {
		return mh, nil
	}
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	monEnd, hostEnd := os.NewFile(uintptr(fds[0]), "mon"), os.NewFile(uintptr(fds[1]), "host")
	conn, err := net_fileConn(monEnd)
	monEnd.Close()
	if err != nil {
		hostEnd.Close()
		return nil, err
	}

	// Run the very binary the monitor is running (not whatever the path points
	// to now): no race with an upgrade, and monitor and hosts always match.
	cmd := exec.Command("/proc/self/exe", "host")
	cmd.Args[0] = strings.TrimSuffix(m.exe, " (deleted)")
	cmd.Dir = "/"
	cmd.Env = []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=" + id.u.HomeDir, "USER=" + id.u.Username, "LOGNAME=" + id.u.Username,
		"SHELL=" + shell, "PTSCRY_CTL_FD=3",
	}
	cmd.ExtraFiles = []*os.File{hostEnd}
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:     true,
		Credential: &syscall.Credential{Uid: uint32(uid), Gid: id.gid, Groups: id.gids},
	}
	if err := cmd.Start(); err != nil {
		hostEnd.Close()
		conn.Close()
		return nil, err
	}
	hostEnd.Close()

	mh := &mhost{
		m: m, uid: uid, user: id.u.Username, pid: cmd.Process.Pid, proc: cmd.Process,
		ctl: newCtlConn(conn), cfg: cfg,
		gid: id.gid, gids: id.gids, home: id.u.HomeDir, shell: shell,
		launches: map[string]*launch{},
		sessions: map[string]*msess{}, reserved: map[uint64]time.Time{},
		recs: map[string]*recorder{}, exited: make(chan struct{}),
	}
	im, _ := json.Marshal(initMsg{UID: uid, User: id.u.Username, Home: id.u.HomeDir, Cfg: cfg})
	if err := mh.ctl.send(ctlInit, im); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		conn.Close()
		return nil, err
	}
	m.hosts[uid] = mh
	m.audit.log("host_spawn", fields{"uid": uid, "user": mh.user, "host_pid": mh.pid,
		"lifecycle": cfg.Lifecycle, "record": cfg.Record, "max_sessions": cfg.MaxSessions})
	mh.startReader()
	go func() {
		err := cmd.Wait()
		mh.cleanup(err)
	}()
	return mh, nil
}

func net_fileConn(f *os.File) (*net.UnixConn, error) {
	c, err := net.FileConn(f)
	if err != nil {
		return nil, err
	}
	uc, ok := c.(*net.UnixConn)
	if !ok {
		c.Close()
		return nil, errors.New("not a unix socket")
	}
	return uc, nil
}

// cleanup runs once the host process is gone.
func (mh *mhost) cleanup(waitErr error) {
	m := mh.m
	m.mu.Lock()
	if m.hosts[mh.uid] == mh {
		delete(m.hosts, mh.uid)
	}
	m.mu.Unlock()

	mh.mu.Lock()
	lost := make([]*msess, 0, len(mh.sessions))
	for _, s := range mh.sessions {
		lost = append(lost, s)
	}
	mh.sessions = map[string]*msess{}
	recs := mh.recs
	mh.recs = map[string]*recorder{}
	mh.mu.Unlock()
	for name, r := range recs {
		r.close()
		m.audit.log("record_close", fields{"uid": mh.uid, "user": mh.user, "session": name, "file": r.Path, "reason": "host lost"})
	}
	for _, s := range lost {
		m.audit.log("session_end", fields{"uid": mh.uid, "user": mh.user, "session": s.Name,
			"pid": s.PID, "reason": "host lost", "src": "monitor"})
	}
	status := "exit 0"
	if waitErr != nil {
		status = waitErr.Error()
	}
	m.audit.log("host_exit", fields{"uid": mh.uid, "user": mh.user, "host_pid": mh.pid, "status": status})
	mh.terminateAll() // no session may outlive the host that serves it
	mh.ctl.c.Close()
	close(mh.exited)
}

var hostEvents = map[string]bool{
	"host_start": true, "host_stop": true, "host_exit": true, "host_shutdown": true,
	"session_new": true, "session_end": true, "attach": true, "detach": true,
	"kill": true, "freeze": true, "thaw": true, "stop_requested": true, "idle_kill": true, "lifetime_kill": true,
	"job_collect": true, "job_discard": true,
	"rename": true, "notify": true, "send_keys": true, "layout_set": true, "layout_del": true,
	"keyboard_take": true, "keyboard_release": true,
}

func num(v any) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

func str(v any) string {
	if s, ok := v.(string); ok {
		if len(s) > 512 {
			s = s[:512]
		}
		return s
	}
	return ""
}

// startReader runs the loop that serves the host's control channel.
func (mh *mhost) startReader() {
	done := make(chan struct{})
	mh.loopDone = done
	go func() {
		defer close(done)
		mh.readLoop()
	}()
}

func (mh *mhost) readLoop() {
	for {
		t, p, _, err := mh.ctl.recv(false)
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) { // only a re-exec sets a deadline here
				if mh.frozen.Load() {
					return // pausing: the host stays
				}
				continue
			}
			// A host we can no longer talk to is a host we cannot audit.
			_ = mh.proc.Kill()
			return
		}
		switch t {
		case ctlEvent:
			mh.onEvent(p)
		case ctlNewResult:
			if mh.controlFlood() {
				return
			}
			var r struct {
				Req uint64 `json:"req"`
			}
			if json.Unmarshal(p, &r) == nil {
				mh.release(r.Req)
			}
		case ctlRecOpen:
			mh.onRecOpen(p)
		case ctlRecData:
			mh.onRecData(p)
		case ctlRecClose:
			mh.onRecClose(string(p))
		case ctlSpawn:
			if mh.controlFlood() {
				return
			}
			if mh.cfg.PAMService != "off" {
				mh.m.spawning.Add(1)
				go func() {
					defer mh.m.spawning.Add(-1)
					mh.onSpawn(p)
				}()
			}
		case ctlKillSess:
			if mh.controlFlood() {
				return
			}
			mh.onKillSess(p)
		}
	}
}

// hostFieldKeys are the only fields a host may attach to its events; values
// must be scalars. Everything else is dropped, so a host cannot bloat or
// shape audit records (identity fields are added by the monitor afterwards).
var hostFieldKeys = map[string]bool{
	"session": true, "pid": true, "cmd": true, "clients": true, "req": true,
	"reason": true, "peer_pid": true, "readonly": true, "record": true,
	"record_file": true, "exit_code": true, "duration_s": true, "lifecycle": true,
	"version": true, "mode": true, "age_s": true, "idle_s": true,
	"from": true, "bytes": true, "layout": true,
}

func cleanHostFields(in map[string]any) fields {
	out := fields{}
	for k, v := range in {
		if !hostFieldKeys[k] {
			continue
		}
		switch x := v.(type) {
		case string:
			max := 512
			if k == "record_file" {
				max = 1024
			}
			if len(x) > max {
				x = x[:max]
			}
			out[k] = x
		case float64, bool:
			out[k] = x
		}
	}
	return out
}

// eventBudget notes one more event from the host and reports whether the host
// has been flooding (more than hostEventRate per second, five seconds running).
func (mh *mhost) floodingEvents() bool {
	mh.evMu.Lock()
	defer mh.evMu.Unlock()
	now := time.Now().Unix()
	if now != mh.evSec {
		if mh.evCount > hostEventRate {
			mh.evHot++
		} else {
			mh.evHot = 0
		}
		mh.evSec, mh.evCount = now, 0
	}
	mh.evCount++
	return mh.evHot >= 5
}

// controlFlood counts a control message against the host's budget and, if the
// host is flooding, kills it (the caller then returns, which also ends the read loop).
func (mh *mhost) controlFlood() bool {
	if !mh.floodingEvents() {
		return false
	}
	mh.m.audit.log("host_flood", fields{"uid": mh.uid, "user": mh.user, "host_pid": mh.pid})
	_ = mh.proc.Kill()
	return true
}

func (mh *mhost) onEvent(p []byte) {
	var e struct {
		Event  string         `json:"event"`
		Fields map[string]any `json:"fields"`
	}
	if json.Unmarshal(p, &e) != nil {
		return
	}
	if mh.floodingEvents() {
		mh.m.audit.log("host_flood", fields{"uid": mh.uid, "user": mh.user, "host_pid": mh.pid})
		_ = mh.proc.Kill()
		return
	}
	if !hostEvents[e.Event] {
		mh.m.audit.log("host_event_unknown", fields{"uid": mh.uid, "user": mh.user, "name": str(e.Event)})
		return
	}
	f := cleanHostFields(e.Fields)
	name, _ := f["session"].(string)
	from, _ := f["from"].(string)
	lay, _ := f["layout"].(string)
	if name != "" && !nameRe.MatchString(name) || from != "" && !nameRe.MatchString(from) || lay != "" && !validLayoutName(lay) {
		mh.m.audit.log("host_event_invalid", fields{"uid": mh.uid, "user": mh.user, "name": e.Event})
		return
	}
	// Identity always comes from what the monitor knows, never from the host.
	f["uid"], f["user"], f["host_pid"], f["src"] = mh.uid, mh.user, mh.pid, "host"

	mh.mu.Lock()
	switch e.Event {
	case "session_new":
		mh.sessions[name] = &msess{Name: name, PID: num(f["pid"]), Created: time.Now(), Cmd: str(f["cmd"])}
		delete(mh.reserved, uint64(num(f["req"])))
	case "session_end":
		delete(mh.sessions, name)
	case "rename":
		if from, _ := f["from"].(string); nameRe.MatchString(from) && name != "" && mh.sessions[name] == nil {
			if s := mh.sessions[from]; s != nil {
				delete(mh.sessions, from)
				s.Name = name
				mh.sessions[name] = s
			}
		}
	case "attach":
		if s := mh.sessions[name]; s != nil {
			s.Attached = num(f["clients"])
		}
	case "detach":
		if s := mh.sessions[name]; s != nil && s.Attached > 0 {
			s.Attached--
		}
	}
	mh.mu.Unlock()
	mh.m.audit.log(e.Event, f)
}

var recSafe = strings.NewReplacer("/", "_", "\x00", "_", " ", "_")

func (mh *mhost) recDir() string {
	_, g := mh.m.snapshot()
	dir := g.RecordDir
	if dir == "" {
		dir = defaultRecordTop
	}
	return filepath.Join(dir, fmt.Sprintf("%d-%s", mh.uid, recSafe.Replace(mh.user)))
}

func (mh *mhost) onRecOpen(p []byte) {
	var r struct {
		Session string `json:"session"`
		Term    string `json:"term"`
		Rows    uint16 `json:"rows"`
		Cols    uint16 `json:"cols"`
	}
	ack := func(ok bool, errmsg, file string) {
		b, _ := json.Marshal(recAck{Session: r.Session, OK: ok, Error: errmsg, File: file})
		_ = mh.ctl.send(ctlRecAck, b)
	}
	if json.Unmarshal(p, &r) != nil || !nameRe.MatchString(r.Session) || len(r.Term) > 64 {
		ack(false, "bad recording request", "")
		return
	}
	// The mode comes from the policy the monitor gave the host, not from the host.
	_, g := mh.m.snapshot()
	top := g.RecordDir
	if top == "" {
		top = defaultRecordTop
	}
	if err := ensureRootDir(top, 0o700); err != nil {
		mh.m.audit.log("record_error", fields{"uid": mh.uid, "user": mh.user, "session": r.Session, "error": err.Error()})
		ack(false, "cannot open recording", "")
		return
	}
	rec, err := newRecorder(mh.recDir(), r.Session, mh.cfg.Record, r.Term, r.Rows, r.Cols, mh.cfg.RecordMaxBytes)
	if err != nil {
		mh.m.audit.log("record_error", fields{"uid": mh.uid, "user": mh.user, "session": r.Session, "error": err.Error()})
		ack(false, "cannot open recording", "")
		return
	}
	mh.mu.Lock()
	if old := mh.recs[r.Session]; old != nil {
		old.close()
	}
	mh.recs[r.Session] = rec
	mh.mu.Unlock()
	mh.m.audit.log("record_open", fields{"uid": mh.uid, "user": mh.user, "session": r.Session,
		"file": rec.Path, "mode": mh.cfg.Record})
	ack(true, "", rec.Path)
}

func (mh *mhost) onRecData(p []byte) {
	if len(p) < 2 || len(p) < 2+int(p[1]) {
		return
	}
	kind, name, data := p[0], string(p[2:2+int(p[1])]), p[2+int(p[1]):]
	mh.mu.Lock()
	r := mh.recs[name]
	mh.mu.Unlock()
	if r == nil {
		return
	}
	switch kind {
	case 'o':
		r.output(data)
	case 'i':
		r.inputData(data)
	}
	if err := r.err(); err != nil {
		// Cannot record any more: stop the session rather than leave it unrecorded.
		mh.m.audit.log("record_error", fields{"uid": mh.uid, "user": mh.user, "session": name, "error": err.Error()})
		mh.onRecClose(name)
		b, _ := json.Marshal(map[string]string{"session": name, "reason": "recording failed"})
		_ = mh.ctl.send(ctlKill, b)
	}
}

func (mh *mhost) onRecClose(name string) {
	mh.mu.Lock()
	r := mh.recs[name]
	delete(mh.recs, name)
	mh.mu.Unlock()
	if r != nil {
		r.close()
		mh.m.audit.log("record_close", fields{"uid": mh.uid, "user": mh.user, "session": name, "file": r.Path})
	}
}

// ------------------------------------------------------------------ admin

func (m *monitor) handleAdmin(fc *framed, hl *hello, cred *syscall.Ucred, req uint64) {
	uid := int(cred.Uid)
	_, global := m.snapshot()
	allowed, who := uid == 0, "root"
	if !allowed && global.AdminGroup != "" {
		if id, err := lookupIdentity(uid); err == nil {
			who = id.u.Username
			allowed = contains(id.groups, global.AdminGroup)
		}
	}
	if !allowed {
		m.deny(fc, req, uid, who, "not an administrator", "")
		return
	}
	switch hl.Op {
	case "admin-list":
		m.mu.Lock()
		hs := make([]*mhost, 0, len(m.hosts))
		for _, h := range m.hosts {
			hs = append(hs, h)
		}
		m.mu.Unlock()
		var out []sessionInfo
		for _, h := range hs {
			h.mu.Lock()
			for _, s := range h.sessions {
				out = append(out, sessionInfo{User: h.user, Name: s.Name, PID: s.PID,
					Created: s.Created, Last: s.Created, Attached: s.Attached, Cmd: s.Cmd})
			}
			h.mu.Unlock()
		}
		_ = replyOK(fc, okReply{Sessions: out})

	case "admin-reexec":
		if uid != 0 {
			m.deny(fc, req, uid, who, "only root may re-exec the monitor", "")
			return
		}
		m.audit.log("admin_reexec", fields{"req": req, "admin_uid": uid, "admin": who})
		_ = replyOK(fc, okReply{Message: "re-exec requested: see the audit log (monitor_resumed, or monitor_reexec_failed)"})
		go func() {
			time.Sleep(200 * time.Millisecond) // let this request finish: the re-exec waits for the handlers
			_ = m.reexec("admin")
		}()

	case "admin-kill", "admin-stop":
		var target *mhost
		m.mu.Lock()
		for _, h := range m.hosts {
			if h.user == hl.User {
				target = h
			}
		}
		m.mu.Unlock()
		if target == nil {
			replyErr(fc, fmt.Sprintf("no running host for user %q", hl.User))
			return
		}
		if hl.Op == "admin-kill" {
			b, _ := json.Marshal(map[string]string{"session": hl.Name, "reason": "killed by administrator " + who})
			_ = target.ctl.send(ctlKill, b)
			// Do not depend on the host alone: end the process ourselves too.
			target.mu.Lock()
			l := target.launches[hl.Name]
			target.mu.Unlock()
			if l != nil {
				afterTracked(2*time.Second, l.terminate)
			}
		} else {
			b, _ := json.Marshal(map[string]string{"reason": "stopped by administrator " + who})
			_ = target.ctl.send(ctlShutdown, b)
			afterTracked(10*time.Second, target.terminateAll)
		}
		m.audit.log(strings.ReplaceAll(hl.Op, "-", "_"), fields{"req": req, "admin_uid": uid, "admin": who,
			"target_user": hl.User, "session": hl.Name})
		_ = replyOK(fc, okReply{Message: "done"})
	}
}

// ------------------------------------------------------------------ attribution

// procAttr collects what the kernel knows about the calling process. The
// audit login uid and session id are set once at login (pam_loginuid) and
// cannot be changed by the user, so they identify the original login even
// after su or sudo. The SSH environment is the caller's own and only claimed.
func procAttr(pid int32) fields {
	f := fields{}
	base := "/proc/" + strconv.Itoa(int(pid))
	if s := readTrim(base + "/loginuid"); s != "n/a" {
		f["loginuid"] = s
	}
	if s := readTrim(base + "/sessionid"); s != "n/a" {
		f["audit_session"] = s
	}
	if tty, err := os.Readlink(base + "/fd/0"); err == nil {
		f["tty"] = clip(tty, 256)
	}
	if exe, err := os.Readlink(base + "/exe"); err == nil {
		f["client_exe"] = clip(exe, 512)
	}
	if env, err := os.ReadFile(base + "/environ"); err == nil {
		for _, kv := range strings.Split(string(env), "\x00") {
			if strings.HasPrefix(kv, "SSH_CONNECTION=") && len(kv) < 256 {
				f["ssh_connection_claimed"] = strings.TrimPrefix(kv, "SSH_CONNECTION=")
			}
		}
	}
	// Verified: is the caller a descendant of a root-owned sshd process?
	p := int(pid)
	for i := 0; i < 32 && p > 1; i++ {
		comm, ppid, ok := procStat(p)
		if !ok {
			break
		}
		if strings.HasPrefix(comm, "sshd") && procOwner(p) == 0 {
			f["via_sshd"], f["sshd_pid"] = true, p
			return f
		}
		p = ppid
	}
	f["via_sshd"] = false
	return f
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func procStat(pid int) (comm string, ppid int, ok bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", 0, false
	}
	s := string(b)
	l, r := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if l < 0 || r < l {
		return "", 0, false
	}
	rest := strings.Fields(s[r+1:])
	if len(rest) < 2 {
		return "", 0, false
	}
	pp, err := strconv.Atoi(rest[1])
	if err != nil {
		return "", 0, false
	}
	return s[l+1 : r], pp, true
}

func procOwner(pid int) uint32 {
	fi, err := os.Stat("/proc/" + strconv.Itoa(pid))
	if err != nil {
		return 1<<32 - 1
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Uid
	}
	return 1<<32 - 1
}

// monitorSelfTest is `ptscry monitor -t`: everything that would stop a monitor
// from starting, checked without starting it or touching any state. A re-exec
// runs it with the new program first, so a bad upgrade or configuration is
// refused while the old monitor (and its sessions) is still running.
func monitorSelfTest(cpath string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := checkTrusted(exe, true); err != nil {
		return fmt.Errorf("binary: %w", err)
	}
	if err := checkTrusted(cpath, false); err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	cf, err := parseConfigFile(cpath)
	if err != nil {
		return err
	}
	g := cf.resolve("", nil)
	if g.AuditLog == "off" {
		return errors.New("audit_log = off is not allowed for the monitor")
	}
	if err := checkPAMService(g.PAMService); err != nil {
		return err
	}
	if g.Frontend {
		if _, _, err := buildFeInit(g); err != nil {
			return fmt.Errorf("frontend: %w", err)
		}
	}
	fmt.Println("configuration OK")
	return nil
}
