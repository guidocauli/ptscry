//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// doctor.go: `ptscry check [-u user]` (formerly `doctor`, still accepted)
// checks the deployment and one user against what the security model assumes,
// and prints the ISO/IEC 27001:2022 Annex A control each check supports.
// Run it as root for the full picture; any user can run it for themselves.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type doctor struct {
	pass, warn, fail int
	color            bool
}

// paint wraps s in an ANSI colour when the output is a terminal.
func (d *doctor) paint(code, s string) string {
	if !d.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// Levels: OK (green), WARN (yellow), CRIT (red).
func (d *doctor) ok(control, msg string) {
	d.pass++
	fmt.Printf("%s  %-9s %s\n", d.paint("1;32", "OK  "), control, msg)
}
func (d *doctor) warnf(control, msg string) {
	d.warn++
	fmt.Printf("%s  %-9s %s\n", d.paint("1;33", "WARN"), control, msg)
}
func (d *doctor) bad(control, msg string) {
	d.fail++
	fmt.Printf("%s  %-9s %s\n", d.paint("1;31", "CRIT"), control, msg)
}

// wantColor: -color=auto colours a terminal unless NO_COLOR is set or TERM=dumb.
func wantColor(mode string) bool {
	switch mode {
	case "always":
		return true
	case "never":
		return false
	}
	return isTerminal(os.Stdout) && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
}

func orStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (d *doctor) info(msg string) { fmt.Printf("INFO            %s\n", msg) }

// defaultCheckUser is who `ptscry check` looks at without -u: the invoking
// user, which under sudo is SUDO_USER (what the output says; it only decides
// whom to describe, never what is allowed).
func defaultCheckUser() (name string, viaSudo bool) {
	if os.Getuid() == 0 {
		if s := os.Getenv("SUDO_USER"); s != "" && s != "root" {
			return s, true
		}
	}
	if u, err := user.LookupId(strconv.Itoa(os.Getuid())); err == nil {
		return u.Username, false
	}
	return "", false
}

func cmdCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	cpath := fs.String("c", defaultConfigPath, "configuration file")
	usr := fs.String("u", "", "user to check (default: the current user)")
	colorMode := fs.String("color", "auto", "colours: auto, always or never")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *colorMode != "auto" && *colorMode != "always" && *colorMode != "never" {
		return errors.New("-color wants auto, always or never")
	}
	d := &doctor{color: wantColor(*colorMode)}
	fmt.Printf("# %s\n", versionLine())
	if standalone() {
		// a single-user installation (ptscry-gui "senza server", ~/.local): no
		// root monitor, so not the checks of a server
		return checkStandalone(d)
	}
	if os.Getuid() != 0 {
		d.warnf("-", "not running as root: the checks on files, PAM and sshd are incomplete")
	}
	who, viaSudo := *usr, false
	if who == "" {
		who, viaSudo = defaultCheckUser()
	}

	// build: NSS resolution needs cgo
	cgo := "unknown"
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "CGO_ENABLED" {
				cgo = s.Value
			}
		}
	}
	if cgo == "1" {
		d.ok("A.5.16", "binary built with cgo: users and groups resolve through NSS (SSSD/FreeIPA)")
	} else {
		d.warnf("A.5.16", "binary built without cgo: FreeIPA/SSSD users are NOT resolvable (build with CGO_ENABLED=1)")
	}

	// configuration
	cf, err := parseConfigFile(*cpath)
	if err != nil {
		d.bad("A.8.9", "configuration: "+err.Error())
		return summarize(d)
	}
	if err := checkTrusted(*cpath, false); err != nil {
		d.bad("A.8.9", "configuration: "+err.Error())
	} else {
		d.ok("A.8.9", "configuration file is root-owned and not writable by others")
	}
	if exe, err := os.Executable(); err == nil {
		if err := checkTrusted(exe, true); err != nil {
			d.bad("A.8.9", "binary: "+err.Error())
		} else {
			d.ok("A.8.9", "binary is root-owned and not writable by others")
		}
	}
	g := cf.resolve("", nil)
	if g.AccessGroup == "" {
		d.warnf("A.5.15", "access_group is empty: every local user may open sessions (least privilege: set a group)")
	} else if _, err := lookupGroup(g.AccessGroup); err != nil {
		d.bad("A.5.15", fmt.Sprintf("access_group %q does not exist", g.AccessGroup))
	} else {
		d.ok("A.5.15", fmt.Sprintf("access limited to group %q", g.AccessGroup))
	}
	if g.AdminGroup != "" {
		if _, err := lookupGroup(g.AdminGroup); err != nil {
			d.bad("A.8.2", fmt.Sprintf("admin_group %q does not exist", g.AdminGroup))
		}
	}
	for _, rg := range g.AdminRootGroups {
		if _, err := lookupGroup(rg); err != nil {
			d.bad("A.8.2", fmt.Sprintf("admin_root_groups: group %q does not exist", rg))
		} else {
			d.info(fmt.Sprintf("admin_root_groups: the members of %q administer ptscry with root's authority (A.8.2)", rg))
		}
	}
	if g.MaxSessions == 0 {
		d.warnf("A.8.6", "max_sessions is unlimited by default: consider a global cap and unlimited only for selected groups")
	}

	// the user
	if who == "" {
		d.warnf("A.5.15", "cannot tell which user to check: use -u USER")
	} else {
		if viaSudo {
			d.info("checking " + who + " (the user who ran sudo; use -u to choose)")
		}
		checkUser(d, cf, who)
	}

	// kernel
	if s := readTrim("/proc/sys/kernel/yama/ptrace_scope"); s == "n/a" {
		d.warnf("A.8.9", "kernel.yama.ptrace_scope not available: users could trace their own processes, including hosts")
	} else if s == "0" {
		d.warnf("A.8.9", "kernel.yama.ptrace_scope = 0 (set >= 1: protects the audit data held by the host)")
	} else {
		d.ok("A.8.9", "kernel.yama.ptrace_scope = "+s)
	}

	// session launcher / PAM
	if g.PAMService == "off" {
		d.warnf("A.8.5", "pam_service = off: sessions are started without PAM (no pam_limits, no audit login uid, no PAM access rules)")
	} else {
		switch {
		case !launcherAvailable:
			d.warnf("A.8.5", "pam_service = "+g.PAMService+" but this binary has no PAM support (built without cgo)")
		case pamAvailable() != nil:
			d.warnf("A.8.5", "libpam.so.0 cannot be loaded: the monitor will refuse to start")
		default:
			d.ok("A.8.5", "libpam available")
			path := ""
			for _, dir := range []string{"/etc/pam.d/", "/usr/lib/pam.d/", "/usr/etc/pam.d/"} {
				if _, err := os.Stat(dir + g.PAMService); err == nil {
					path = dir + g.PAMService
					break
				}
			}
			if path == "" {
				d.warnf("A.8.5", "PAM service file /etc/pam.d/"+g.PAMService+" not found (install contrib/pam.d/ptscry.*)")
			} else {
				if err := checkTrusted(path, true); err != nil {
					d.warnf("A.8.9", "PAM service file: "+err.Error())
				} else {
					d.ok("A.8.9", path+" is root-owned and not writable by others")
				}
				b, _ := os.ReadFile(path)
				for _, mod := range []string{"pam_loginuid", "pam_limits"} {
					if strings.Contains(string(b), mod) {
						d.ok("A.8.15", path+" uses "+mod)
					} else {
						d.warnf("A.8.15", path+" does not use "+mod+" (audit login uid / resource limits)")
					}
				}
			}
		}
		if _, err := os.Stat("/proc/self/loginuid"); err != nil {
			d.warnf("A.8.15", "this kernel has no audit login uid (/proc/self/loginuid): auditd cannot attribute actions inside sessions")
		}
	}

	checkFrontend(d, g)

	// pseudoterminals are a system-wide resource: every session takes one
	if mx, nr := readTrim("/proc/sys/kernel/pty/max"), readTrim("/proc/sys/kernel/pty/nr"); mx != "n/a" {
		d.ok("A.8.6", "pseudoterminals in use: "+nr+" of "+mx+" (kernel.pty.max)")
		if n, err := strconv.Atoi(mx); err == nil && g.MaxTotalSessions > 0 && g.MaxTotalSessions > n/2 {
			d.warnf("A.8.6", fmt.Sprintf("max_total_sessions = %d is more than half of kernel.pty.max (%d): sessions could starve sshd logins of terminals", g.MaxTotalSessions, n))
		}
	}
	if g.MaxTotalSessions == 0 {
		d.warnf("A.8.6", "max_total_sessions = unlimited: one account could use up every pseudoterminal on the host")
	}

	// monitor
	sock := g.MonitorSocket
	if c, err := net.DialTimeout("unix", sock, 2*time.Second); err != nil {
		d.warnf("A.8.15", "monitor not reachable at "+sock+" (is the service running?)")
	} else {
		c.Close()
		d.ok("A.8.15", "monitor reachable at "+sock)
		if fi, err := os.Stat(sock); err == nil {
			if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid == 0 {
				d.ok("A.5.15", "monitor socket is root-owned")
			} else {
				d.bad("A.5.15", "monitor socket is not root-owned")
			}
		}
	}

	// audit
	ap := g.AuditLog
	if ap == "" {
		ap = defaultAuditLog
	}
	if ap == "off" {
		d.bad("A.8.15", "audit_log = off")
	} else if fi, err := os.Stat(ap); err != nil {
		d.warnf("A.8.15", "audit log not found at "+ap)
	} else {
		if fi.Mode().Perm()&0o007 != 0 {
			d.bad("A.8.15", fmt.Sprintf("audit log is readable/writable by others (%v)", fi.Mode().Perm()))
		} else {
			d.ok("A.8.15", fmt.Sprintf("audit log %s mode %v", ap, fi.Mode().Perm()))
		}
		if g.AuditGroup != "" {
			switch {
			case g.AuditGroup == g.AccessGroup:
				d.bad("A.8.15", fmt.Sprintf("audit_group %q is the access group: every user would read everyone's activity", g.AuditGroup))
			case fi.Mode().Perm()&0o040 == 0:
				d.warnf("A.8.15", fmt.Sprintf("audit_group = %s but the log is not group-readable (monitor not reloaded?)", g.AuditGroup))
			default:
				d.ok("A.8.15", fmt.Sprintf("audit log readable by the group %q only (besides root)", g.AuditGroup))
			}
		} else {
			d.info("audit_group is not set: only root reads the audit log (set audit_group = " + orStr(g.AdminGroup, "ptscry-admins") + " to let the administrators verify it)")
		}
		if f, err := os.Open(ap); err == nil {
			f.Close()
		} else if os.Getuid() != 0 {
			d.info("the audit log is not readable by you (normal: it holds every user's activity; root or the log forwarder reads it)")
		}
		if n, err := verifyChain([]string{ap}); err != nil {
			d.bad("A.8.15", fmt.Sprintf("audit chain: %v", err))
		} else {
			d.ok("A.8.15", fmt.Sprintf("audit hash chain intact (%d records)", n))
		}
	}
	if g.AuditSyslog {
		d.ok("A.8.15", "audit also sent to syslog (forward it off-host for tamper resistance)")
	} else {
		d.warnf("A.8.15", "audit_syslog = no: keep a copy off this host (rsyslog/SIEM), root here can rewrite the local file")
	}

	// time
	if out, err := run("timedatectl", "show", "-p", "NTPSynchronized", "--value"); err == nil {
		if strings.TrimSpace(out) == "yes" {
			d.ok("A.8.17", "clock is NTP-synchronised (audit timestamps are meaningful)")
		} else {
			d.warnf("A.8.17", "clock is not NTP-synchronised")
		}
	}

	// sshd
	switch {
	case who == "":
	case os.Getuid() != 0:
		d.info("the effective sshd policy for " + who + " needs root: run `sudo ptscry check`")
	default:
		checkSSHD(d, who)
	}
	return summarize(d)
}

// checkUser reports what decides whether the monitor lets this user in: the
// same tests it applies (access group, policy, shell), with the reason.
func checkUser(d *doctor, cf *configFile, name string) {
	id, err := lookupIdentityByName(name)
	if err != nil {
		d.bad("A.5.16", fmt.Sprintf("user %q: %v (a binary without cgo cannot see FreeIPA/SSSD users)", name, err))
		return
	}
	uid, _ := strconv.Atoi(id.u.Uid)
	cfg := cf.resolve(name, id.groups)
	if cfg.AccessGroup == "" {
		d.info("access_group is empty: " + name + " may use ptscry (no group required)")
	} else if uid == 0 && cfg.AllowRoot && !contains(id.groups, cfg.AccessGroup) {
		d.ok("A.5.15", "root may use ptscry without being in the access group (allow_root = yes; the audit records the login uid)")
	} else if contains(id.groups, cfg.AccessGroup) {
		d.ok("A.5.15", fmt.Sprintf("%s is a member of the access group %q", name, cfg.AccessGroup))
	} else {
		d.bad("A.5.15", fmt.Sprintf("%s is NOT a member of the access group %q: ptscry will refuse (usermod -aG %s %s%s)", name, cfg.AccessGroup, cfg.AccessGroup, name, map[bool]string{true: "; root is excluded by allow_root = no", false: ""}[uid == 0]))
	}
	if cfg.AdminGroup != "" {
		d.info(fmt.Sprintf("%s is a member of the admin group %q: %v", name, cfg.AdminGroup, contains(id.groups, cfg.AdminGroup)))
	}
	if !cfg.Enabled {
		d.bad("A.5.15", "the configuration disables ptscry for "+name+" (enabled = no)")
	}
	if !interactiveShell(cfg.Shell) && !interactiveShell(lookupShell(uid)) {
		d.bad("A.8.2", name+" has no interactive shell (login shell nologin/false): set shell = /bin/bash for the user or group in the configuration")
	}
	max := fmt.Sprint(cfg.MaxSessions)
	if cfg.MaxSessions == 0 {
		max = "unlimited"
	}
	d.info(fmt.Sprintf("policy for %s: max_sessions %s, %s, record %s, allow_command %v", name, max, cfg.Lifecycle, cfg.Record, cfg.AllowCommand))
}

func summarize(d *doctor) error {
	fmt.Printf("\n%s, %s, %s\n", d.paint("1;32", fmt.Sprintf("%d ok", d.pass)),
		d.paint("1;33", fmt.Sprintf("%d warnings", d.warn)), d.paint("1;31", fmt.Sprintf("%d critical", d.fail)))
	if d.fail > 0 {
		return &exitStatus{1}
	}
	return nil
}

func run(name string, args ...string) (string, error) {
	out, _, err := runErr(name, args...)
	return out, err
}

// runErr also returns what the command wrote on stderr (for the message).
func runErr(name string, args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var eb strings.Builder
	cmd.Stderr = &eb
	out, err := cmd.Output()
	return string(out), strings.TrimSpace(eb.String()), err
}

func lookupGroup(name string) (string, error) {
	out, err := run("getent", "group", name)
	if err != nil || strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("group %q not found", name)
	}
	return out, nil
}

// parseSSHDConfig reads `sshd -T`: "keyword value", one per line, keywords
// lowercase (compared case-insensitively anyway), any run of blanks between.
func parseSSHDConfig(out string) map[string]string {
	cfg := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		k := strings.ToLower(f[0])
		if _, dup := cfg[k]; !dup { // a keyword can appear several times (e.g. port): keep the first
			cfg[k] = strings.Join(f[1:], " ")
		}
	}
	return cfg
}

// checkSSHD asks sshd itself what applies to a user (sshd -T -C), so Match
// blocks, includes and defaults are all taken into account.
func checkSSHD(d *doctor, user string) {
	sshd := "sshd"
	if _, err := exec.LookPath(sshd); err != nil { // sudo's PATH may lack the sbin directories
		for _, p := range []string{"/usr/sbin/sshd", "/sbin/sshd", "/usr/local/sbin/sshd"} {
			if _, err := os.Stat(p); err == nil {
				sshd = p
				break
			}
		}
	}
	out, serr, err := runErr(sshd, "-T", "-C", "user="+user+",host=localhost,addr=127.0.0.1")
	if err != nil {
		d.warnf("A.8.5", "cannot run `sshd -T` (need root and OpenSSH): "+err.Error()+" "+clip(serr, 200))
		return
	}
	cfg := parseSSHDConfig(out)
	if len(cfg) == 0 {
		d.warnf("A.8.5", "`sshd -T` printed no settings: "+clip(strings.TrimSpace(out+" "+serr), 200))
		return
	}
	d.info(fmt.Sprintf("effective sshd policy for %q (%d settings read from %s):", user, len(cfg), sshd))
	if v, ok := cfg["passwordauthentication"]; !ok {
		d.warnf("A.8.5", "sshd -T does not report PasswordAuthentication: cannot tell")
	} else if v == "no" {
		d.ok("A.8.5", "PasswordAuthentication no")
	} else {
		d.bad("A.8.5", "PasswordAuthentication "+v+" (passwords alone are not strong authentication)")
	}
	switch v, ok := cfg["permitrootlogin"]; {
	case !ok:
		d.warnf("A.8.2", "sshd -T does not report PermitRootLogin: cannot tell")
	case v == "no" || v == "prohibit-password" || v == "without-password": // the old name of prohibit-password
		d.ok("A.8.2", "PermitRootLogin "+v)
	case v == "forced-commands-only":
		d.warnf("A.8.2", "PermitRootLogin "+v+" (root keys limited to a command: acceptable only if intended)")
	default:
		d.bad("A.8.2", "PermitRootLogin "+v)
	}
	methods := cfg["authenticationmethods"]
	switch {
	case methods == "" || methods == "any":
		d.warnf("A.8.5", "AuthenticationMethods not set: a single factor is enough")
	case strings.Contains(methods, ","):
		d.ok("A.8.5", "multi-factor AuthenticationMethods "+methods)
	default:
		d.warnf("A.8.5", "AuthenticationMethods "+methods+" is a single method")
	}
	opts := cfg["pubkeyauthoptions"]
	if strings.Contains(opts, "verify-required") {
		d.ok("A.8.5", "PubkeyAuthOptions "+opts+" (FIDO2 PIN/biometric required)")
	} else {
		d.warnf("A.8.5", "PubkeyAuthOptions has no verify-required (a FIDO2 key would only need to be touched)")
	}
	if cfg["trustedusercakeys"] != "" && cfg["trustedusercakeys"] != "none" {
		d.ok("A.5.17", "TrustedUserCAKeys "+cfg["trustedusercakeys"]+" (short-lived certificates)")
	} else {
		d.warnf("A.5.17", "no TrustedUserCAKeys: long-lived keys in authorized_keys must be managed by hand")
	}
	if cfg["forcecommand"] != "" && cfg["forcecommand"] != "none" {
		d.ok("A.8.2", "ForceCommand "+cfg["forcecommand"])
	} else {
		d.warnf("A.8.2", "no ForceCommand: user may bypass ptscry with a normal shell (fine unless ptscry must be mandatory)")
	}
	if cfg["permittunnel"] != "" && cfg["permittunnel"] != "no" {
		d.warnf("A.8.20", "PermitTunnel "+cfg["permittunnel"])
	}
}

// checkFrontend reviews the SSH front-end settings (only when enabled).
func checkFrontend(d *doctor, g config) {
	if !g.Frontend {
		return
	}
	if in, _, err := buildFeInit(g); err != nil {
		d.bad("A.8.20", "front-end: "+err.Error())
	} else {
		d.ok("A.8.20", fmt.Sprintf("front-end settings valid (%d CA key(s), plain keys %v)", len(in.CAKeys), in.KeysEnabled))
	}
	if g.FrontendTOTP {
		d.ok("A.8.5", "front-end second factor: TOTP verified by the monitor (a compromised front-end cannot skip it)")
	} else {
		d.warnf("A.8.5", "frontend_totp = no: whoever compromises the front-end process can act as any permitted user")
	}
	if g.FrontendFIDO2 {
		d.ok("A.8.5", "front-end accepts only FIDO2 keys with user verification (PIN/biometric)")
	} else {
		d.warnf("A.8.5", "frontend_require_fido2 = no: software ed25519 keys are accepted")
	}
	if g.FrontendKeysDir != "none" {
		if ents, err := os.ReadDir(g.FrontendKeysDir); err == nil {
			bad := 0
			for _, e := range ents {
				raw, err := os.ReadFile(g.FrontendKeysDir + "/" + e.Name())
				if err != nil {
					continue
				}
				for i, ln := range strings.Split(string(raw), "\n") {
					ln = strings.TrimSpace(ln)
					if ln == "" || strings.HasPrefix(ln, "#") {
						continue
					}
					if _, err := parseAuthLine(ln); err != nil {
						bad++
						d.warnf("A.5.17", fmt.Sprintf("%s line %d grants nothing: %v", e.Name(), i+1, err))
					}
				}
			}
			if bad == 0 {
				d.ok("A.5.17", "authorized_keys.d: every line is usable")
			}
		}
	}
	if g.FrontendCertMax > 24*time.Hour {
		d.warnf("A.5.17", "frontend_cert_max_age is over 24h: certificates are not short-lived")
	}
	if host, port, err := net.SplitHostPort(g.FrontendListen); err == nil {
		if host == "" || host == "0.0.0.0" || host == "::" {
			d.warnf("A.8.20", "front-end listens on all interfaces ("+g.FrontendListen+"): restrict it with the firewall or frontend_listen")
		}
		if p, _ := strconv.Atoi(port); p < 1024 {
			d.bad("A.8.20", "port "+port+" is privileged: the front-end is unprivileged and cannot bind it (use 2222 or a redirect)")
		}
	}
	if c, err := net.DialTimeout("tcp", localDialAddr(g.FrontendListen), time.Second); err == nil {
		c.Close()
		d.ok("A.8.20", "front-end is listening on "+g.FrontendListen)
	} else {
		d.warnf("A.8.20", "front-end is not listening on "+g.FrontendListen+" (monitor stopped or not reloaded?)")
	}
}

func localDialAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// checkStandalone: `ptscry check` in standalone mode (PTSCRY_STANDALONE=1, as
// ptscry-gui runs it for "senza server"): the sessions of this user only, a
// per-user host instead of the root monitor. The binary and the configuration
// may belong to the user (an installation in ~/.local), but nobody else may be
// able to change them, nor read the sessions and the audit.
func checkStandalone(d *doctor) error {
	uid := os.Getuid()
	d.info("standalone mode: sessions of this user only, no root monitor (installation for one user)")
	if uid == 0 {
		d.warnf("A.8.2", "standalone mode as root: use the server (root monitor) for root sessions")
	}
	me := uint32(uid)
	if exe, err := os.Executable(); err == nil {
		if err := checkTrustedBy(exe, true, 0, me); err != nil {
			d.bad("A.8.9", "binary: "+err.Error())
		} else {
			d.ok("A.8.9", "binary "+exe+" is owned by root or by you, and not writable by others")
		}
	}
	cpath := configPath()
	cfg, u, err := loadEnv()
	if err != nil {
		d.bad("A.8.9", "configuration "+cpath+": "+err.Error())
		return summarize(d)
	}
	if _, err := os.Stat(cpath); err != nil {
		d.info("configuration " + cpath + " not found: the defaults apply")
	} else if err := checkTrustedBy(cpath, true, 0, me); err != nil {
		d.bad("A.8.9", "configuration: "+err.Error())
	} else {
		d.ok("A.8.9", "configuration "+cpath+" is owned by root or by you, and not writable by others")
	}
	dir := resolveSocketDir(&cfg, uid)
	if fi, err := os.Stat(dir); err != nil {
		d.info("session directory " + dir + " not created yet (the first session creates it)")
	} else if st, ok := fi.Sys().(*syscall.Stat_t); !ok || st.Uid != me || fi.Mode().Perm()&0o077 != 0 {
		d.bad("A.5.15", fmt.Sprintf("session directory %s must be yours with mode 0700 (it is %v)", dir, fi.Mode().Perm()))
	} else {
		d.ok("A.5.15", "session directory "+dir+" is yours, mode 0700")
	}
	if c, err := net.DialTimeout("unix", filepath.Join(dir, "host.sock"), 2*time.Second); err == nil {
		c.Close()
		d.ok("A.8.15", "host running ("+filepath.Join(dir, "host.sock")+")")
	} else {
		d.info("host not running: it starts with the first session")
	}
	ap := cfg.AuditLog
	if ap == "" {
		ap = defaultAuditPath(uid, u.HomeDir)
	}
	switch fi, err := os.Stat(ap); {
	case ap == "off":
		d.warnf("A.8.15", "audit_log = off: the sessions are not recorded")
	case err != nil:
		d.info("audit log " + ap + " not created yet")
	case fi.Mode().Perm()&0o007 != 0:
		d.bad("A.8.15", fmt.Sprintf("audit log %s is readable/writable by others (%v)", ap, fi.Mode().Perm()))
	default:
		d.ok("A.8.15", fmt.Sprintf("audit log %s mode %v", ap, fi.Mode().Perm()))
	}
	// the hash chain is the root monitor's: the per-user host writes plain records
	d.info("in standalone mode the audit is the user's own, without the hash chain: for evidence others can rely on, use the server (root monitor)")
	if s := readTrim("/proc/sys/kernel/yama/ptrace_scope"); s == "0" {
		d.warnf("A.8.9", "kernel.yama.ptrace_scope = 0 (set >= 1: protects the audit data held by the host)")
	} else if s != "n/a" {
		d.ok("A.8.9", "kernel.yama.ptrace_scope = "+s)
	}
	return summarize(d)
}
