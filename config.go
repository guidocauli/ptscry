//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// config.go: /etc/ptscry/ptscry.conf parsing.
//
// Format: INI-like. Sections [global], [group:NAME], [user:NAME]. Values are
// applied in that order, so a user section overrides a group section, which
// overrides global. Unknown keys are errors (a typo must never silently weaken
// the audit settings).

package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/user"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const defaultConfigPath = "/etc/ptscry/ptscry.conf"

const (
	lifeExit       = "exit_on_last_shell"
	lifePersistent = "persistent_per_user"
)

type config struct {
	// per user / group overridable
	Enabled      bool
	Lifecycle    string
	IdleTimeout  time.Duration // detached sessions idle longer than this are reaped (0 = never)
	MaxLifetime  time.Duration // hard cap on a session's age (0 = none)
	MaxSessions  int           // 0 = unlimited
	RateLimit    int           // requests per second per uid (burst = 4x)
	JobRetention time.Duration // how long the result of a detached job (run -d) is kept; 0 = not kept
	ReplayBytes  int
	DetachPrefix byte // key that introduces a command while attached (default Ctrl-B)
	Shell        string
	AllowCommand bool
	Record       string // off | output | all
	// RecordMaxBytes caps one session's recording (0 = unlimited). Reaching it
	// ends the session, as any other recording failure does.
	RecordMaxBytes int64

	// global only
	SocketDir     string // standalone mode only
	AuditLog      string // path, or "off"
	AuditSyslog   bool
	AuditGroup    string // group that may read the audit log (root:group 0640); empty = root only
	RecordDir     string
	MonitorSocket string
	AccessGroup   string // members may use ptscry (empty = everybody)
	AllowRoot     bool   // root may use ptscry locally without being in AccessGroup (allow_root, default yes)
	AdminGroup    string // members may use "admin" operations (root always may)
	// MaxTotalSessions caps the sessions of all users together (0 = unlimited):
	// pseudoterminals are a system-wide resource (kernel.pty.max).
	MaxTotalSessions int
	// PAMService is the PAM service the session launcher runs for every
	// session ("off" = no launcher: the host starts the shell itself, without
	// PAM, limits or audit login uid).
	PAMService string

	// Integrated SSH front-end (see frontend.go). All global-only.
	Frontend         bool          // run the front-end
	FrontendListen   string        // host:port
	FrontendUser     string        // unprivileged account the front-end runs as
	FrontendHostKey  string        // SSH host key (read by the monitor, passed to the front-end)
	FrontendCAKeys   string        // file with trusted user CA public keys ("none" = no certificates)
	FrontendKeysDir  string        // dir with <user> files of authorized keys ("none" = no plain keys)
	FrontendFIDO2    bool          // only security-key (verify-required) keys
	FrontendTOTP     bool          // second factor verified by the monitor
	FrontendTOTPDir  string        // dir with <user> files holding the base32 secret
	FrontendCertMax  time.Duration // longest certificate validity accepted
	FrontendMaxConns int           // concurrent TCP connections
	FrontendSeccomp  bool          // seccomp-bpf filter in the front-end (linux/amd64)
	FrontendRate     int           // new connections per minute per source address (burst 10)
}

func defaultConfig() config {
	return config{
		Enabled:       true,
		Lifecycle:     lifeExit,
		MaxSessions:   16,
		RateLimit:     10,
		JobRetention:  time.Hour,
		ReplayBytes:   256 * 1024,
		DetachPrefix:  defaultPrefix,
		AllowCommand:  true,
		Record:        "off",
		MonitorSocket: defaultMonitorSocket,
		AllowRoot:     true,

		RecordMaxBytes:   512 << 20,
		MaxTotalSessions: 1024,
		PAMService:       "ptscry",

		FrontendListen:   ":2222",
		FrontendUser:     "ptscry-fe",
		FrontendHostKey:  "/etc/ptscry/ssh_host_ed25519_key",
		FrontendCAKeys:   "/etc/ptscry/frontend_ca.pub",
		FrontendKeysDir:  "/etc/ptscry/authorized_keys.d",
		FrontendFIDO2:    true,
		FrontendTOTP:     true,
		FrontendTOTPDir:  "/etc/ptscry/totp",
		FrontendCertMax:  24 * time.Hour,
		FrontendMaxConns: 256,
		FrontendRate:     30,
		FrontendSeccomp:  true,
	}
}

var pamNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

const defaultMonitorSocket = "/run/ptscry/monitor.sock"

var globalOnly = map[string]bool{
	"socket_dir": true, "audit_log": true, "audit_syslog": true, "audit_group": true, "record_dir": true,
	"monitor_socket": true, "access_group": true, "admin_group": true, "allow_root": true,
	"max_total_sessions": true, "pam_service": true,
	"frontend": true, "frontend_listen": true, "frontend_user": true, "frontend_host_key": true,
	"frontend_ca_keys": true, "frontend_authorized_keys_dir": true, "frontend_require_fido2": true,
	"frontend_totp": true, "frontend_totp_dir": true, "frontend_cert_max_age": true,
	"frontend_max_conns": true, "frontend_rate": true, "frontend_seccomp": true,
}

func parseBool(v string) (bool, error) {
	switch strings.ToLower(v) {
	case "yes", "true", "on", "1":
		return true, nil
	case "no", "false", "off", "0":
		return false, nil
	}
	return false, fmt.Errorf("invalid boolean %q", v)
}

func parseDuration(v string) (time.Duration, error) {
	if v == "0" {
		return 0, nil
	}
	if strings.HasSuffix(v, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(v, "d"))
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid duration %q", v)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid duration %q", v)
	}
	return d, nil
}

// parseSize reads a byte count with an optional K, M or G suffix (powers of
// 1024); "0" and "unlimited" mean no limit.
func parseSize(v string) (int64, error) {
	if v == "unlimited" {
		return 0, nil
	}
	mult := int64(1)
	if n := len(v); n > 1 {
		switch v[n-1] {
		case 'K', 'k':
			mult, v = 1<<10, v[:n-1]
		case 'M', 'm':
			mult, v = 1<<20, v[:n-1]
		case 'G', 'g':
			mult, v = 1<<30, v[:n-1]
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 || n > (1<<40)/mult {
		return 0, fmt.Errorf("invalid size %q (use bytes or a K, M, G suffix)", v)
	}
	return n * mult, nil
}

func (c *config) set(key, val string, scoped bool) error {
	if scoped && globalOnly[key] {
		return fmt.Errorf("%q is only allowed in [global]", key)
	}
	switch key {
	case "session_lifecycle":
		if val != lifeExit && val != lifePersistent {
			return fmt.Errorf("session_lifecycle must be %s or %s", lifeExit, lifePersistent)
		}
		c.Lifecycle = val
	case "idle_timeout":
		d, err := parseDuration(val)
		if err != nil {
			return err
		}
		c.IdleTimeout = d
	case "enabled":
		b, err := parseBool(val)
		if err != nil {
			return err
		}
		c.Enabled = b
	case "max_lifetime":
		d, err := parseDuration(val)
		if err != nil {
			return err
		}
		c.MaxLifetime = d
	case "max_sessions":
		if val == "unlimited" {
			c.MaxSessions = 0
			break
		}
		n, err := strconv.Atoi(val)
		if err != nil || n < 1 || n > 100000 {
			return fmt.Errorf("max_sessions must be 1..100000 or unlimited")
		}
		c.MaxSessions = n
	case "job_retention":
		d, err := parseDuration(val)
		if err != nil {
			return err
		}
		if d > 7*24*time.Hour {
			return fmt.Errorf("job_retention must be at most 7d")
		}
		c.JobRetention = d
	case "rate_limit":
		n, err := strconv.Atoi(val)
		if err != nil || n < 1 || n > 1000 {
			return fmt.Errorf("rate_limit must be 1..1000 (requests per second)")
		}
		c.RateLimit = n
	case "max_total_sessions":
		if val == "unlimited" {
			c.MaxTotalSessions = 0
			break
		}
		n, err := strconv.Atoi(val)
		if err != nil || n < 1 || n > 1000000 {
			return fmt.Errorf("max_total_sessions must be 1..1000000 or unlimited")
		}
		c.MaxTotalSessions = n
	case "pam_service":
		if val != "off" && !pamNameRe.MatchString(val) {
			return fmt.Errorf("pam_service must be \"off\" or a PAM service name (letters, digits, . _ -)")
		}
		c.PAMService = val
	case "frontend", "frontend_require_fido2", "frontend_totp":
		b, err := parseBool(val)
		if err != nil {
			return err
		}
		switch key {
		case "frontend":
			c.Frontend = b
		case "frontend_require_fido2":
			c.FrontendFIDO2 = b
		default:
			c.FrontendTOTP = b
		}
	case "frontend_listen":
		if _, port, err := net.SplitHostPort(val); err != nil || port == "" {
			return fmt.Errorf("frontend_listen must be host:port (e.g. :2222)")
		}
		c.FrontendListen = val
	case "frontend_user":
		if !pamNameRe.MatchString(val) {
			return fmt.Errorf("frontend_user %q is not a valid account name", val)
		}
		c.FrontendUser = val
	case "frontend_host_key", "frontend_ca_keys", "frontend_authorized_keys_dir", "frontend_totp_dir":
		if val != "none" && !strings.HasPrefix(val, "/") {
			return fmt.Errorf("%s must be an absolute path (or none)", key)
		}
		switch key {
		case "frontend_host_key":
			c.FrontendHostKey = val
		case "frontend_ca_keys":
			c.FrontendCAKeys = val
		case "frontend_authorized_keys_dir":
			c.FrontendKeysDir = val
		default:
			c.FrontendTOTPDir = val
		}
	case "frontend_cert_max_age":
		d, err := parseDuration(val)
		if err != nil || d < time.Minute {
			return fmt.Errorf("frontend_cert_max_age must be a duration of at least 1m")
		}
		c.FrontendCertMax = d
	case "frontend_seccomp":
		b, err := parseBool(val)
		if err != nil {
			return err
		}
		c.FrontendSeccomp = b
	case "frontend_rate":
		n, err := strconv.Atoi(val)
		if err != nil || n < 1 || n > 1000000 {
			return fmt.Errorf("frontend_rate must be 1..1000000 (connections per minute per address)")
		}
		c.FrontendRate = n
	case "frontend_max_conns":
		n, err := strconv.Atoi(val)
		if err != nil || n < 1 || n > 100000 {
			return fmt.Errorf("frontend_max_conns must be 1..100000")
		}
		c.FrontendMaxConns = n
	case "record_max_bytes":
		n, err := parseSize(val)
		if err != nil {
			return err
		}
		c.RecordMaxBytes = n
	case "monitor_socket":
		if !strings.HasPrefix(val, "/") {
			return fmt.Errorf("monitor_socket must be an absolute path")
		}
		c.MonitorSocket = val
	case "access_group":
		c.AccessGroup = val
	case "allow_root":
		b, err := parseBool(val)
		if err != nil {
			return fmt.Errorf("allow_root: %v", err)
		}
		c.AllowRoot = b
	case "admin_group":
		c.AdminGroup = val
	case "replay_bytes":
		n, err := strconv.Atoi(val)
		if err != nil || n < 0 || n > 512*1024 {
			return fmt.Errorf("replay_bytes must be 0..524288")
		}
		c.ReplayBytes = n
	case "detach_prefix":
		b, err := parsePrefix(val)
		if err != nil {
			return fmt.Errorf("detach_prefix: %v", err)
		}
		c.DetachPrefix = b
	case "shell":
		if !strings.HasPrefix(val, "/") {
			return fmt.Errorf("shell must be an absolute path")
		}
		c.Shell = val
	case "allow_command":
		b, err := parseBool(val)
		if err != nil {
			return err
		}
		c.AllowCommand = b
	case "record":
		if val != "off" && val != "output" && val != "all" {
			return fmt.Errorf("record must be off, output or all")
		}
		c.Record = val
	case "socket_dir":
		if !strings.HasPrefix(val, "/") {
			return fmt.Errorf("socket_dir must be an absolute path (%%u = uid)")
		}
		c.SocketDir = val
	case "audit_log":
		c.AuditLog = val
	case "audit_group":
		if val != "" && !pamNameRe.MatchString(val) {
			return fmt.Errorf("audit_group: not a valid group name")
		}
		c.AuditGroup = val
	case "audit_syslog":
		b, err := parseBool(val)
		if err != nil {
			return err
		}
		c.AuditSyslog = b
	case "record_dir":
		if !strings.HasPrefix(val, "/") {
			return fmt.Errorf("record_dir must be an absolute path")
		}
		c.RecordDir = val
	default:
		return fmt.Errorf("unknown key %q", key)
	}
	return nil
}

type kvPair struct {
	key, val string
	line     int
}

type section struct {
	kind string // global | user | group
	name string
	kv   []kvPair
}

// canonKey turns sshd-style names into the canonical key: MaxParallelSessions
// and max_parallel_sessions both mean max_sessions.
func canonKey(k string) string {
	if !strings.Contains(k, "_") && k != strings.ToLower(k) { // CamelCase
		var b strings.Builder
		for i, r := range k {
			if i > 0 && r >= 'A' && r <= 'Z' {
				b.WriteByte('_')
			}
			b.WriteRune(r)
		}
		k = b.String()
	}
	k = strings.ToLower(k)
	if k == "max_parallel_sessions" {
		return "max_sessions"
	}
	return k
}

func unquoteName(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// parseSections reads two equivalent styles that may be mixed:
//
//	[user:guido]                 Match User guido,rex-agent
//	max_sessions = 5             MaxParallelSessions 16
//
// A Match line starts a block that lasts until the next section or Match line;
// "Match All" returns to the global settings.
func parseSections(path string, f *os.File) ([]section, error) {
	var secs []section
	secs = append(secs, section{kind: "global"})
	idx := []int{0}
	sc := bufio.NewScanner(f)
	ln := 0
	for sc.Scan() {
		ln++
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		line = stripInlineComment(line)
		if line[0] == '[' {
			if !strings.HasSuffix(line, "]") {
				return nil, fmt.Errorf("%s:%d: malformed section header", path, ln)
			}
			h := strings.TrimSpace(line[1 : len(line)-1])
			s := section{}
			switch {
			case h == "global":
				s.kind = "global"
			case strings.HasPrefix(h, "user:") && len(h) > 5:
				s.kind, s.name = "user", h[5:]
			case strings.HasPrefix(h, "group:") && len(h) > 6:
				s.kind, s.name = "group", h[6:]
			default:
				return nil, fmt.Errorf("%s:%d: unknown section %q", path, ln, h)
			}
			secs = append(secs, s)
			idx = []int{len(secs) - 1}
			continue
		}
		if f := strings.Fields(line); len(f) >= 2 && strings.EqualFold(f[0], "match") && !strings.Contains(f[0], "=") {
			kind := strings.ToLower(f[1])
			if kind == "all" && len(f) == 2 {
				secs = append(secs, section{kind: "global"})
				idx = []int{len(secs) - 1}
				continue
			}
			if (kind != "user" && kind != "group") || len(f) != 3 {
				return nil, fmt.Errorf("%s:%d: expected: Match User name[,name] | Match Group name[,name] | Match All", path, ln)
			}
			idx = nil
			for _, n := range strings.Split(f[2], ",") {
				n = unquoteName(n)
				if n == "" {
					return nil, fmt.Errorf("%s:%d: empty name in Match", path, ln)
				}
				secs = append(secs, section{kind: kind, name: n})
				idx = append(idx, len(secs)-1)
			}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok { // sshd style: Key value
			var rest string
			k, rest, ok = strings.Cut(line, " ")
			if !ok {
				k, rest, ok = strings.Cut(line, "\t")
			}
			if !ok {
				return nil, fmt.Errorf("%s:%d: expected key = value", path, ln)
			}
			v = rest
		}
		key, val := canonKey(strings.TrimSpace(k)), unquoteName(v)
		for _, i := range idx {
			secs[i].kv = append(secs[i].kv, kvPair{key, val, ln})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return secs, nil
}

// configFile is a parsed and fully validated configuration file.
type configFile struct {
	path string
	secs []section
}

// stripInlineComment removes a trailing " # comment": a # that follows a blank
// and is not inside double quotes. A # glued to a word stays (it is part of a value).
func stripInlineComment(s string) string {
	inQ := false
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '"':
			inQ = !inQ
		case s[i] == '#' && !inQ && i > 0 && (s[i-1] == ' ' || s[i-1] == '\t'):
			return strings.TrimRight(s[:i], " \t")
		}
	}
	return s
}

// parseConfigFile reads path (a missing file means defaults) and validates
// every section, matching or not, so a typo is always reported.
func parseConfigFile(path string) (*configFile, error) {
	cf := &configFile{path: path}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cf, nil
		}
		return nil, err
	}
	defer f.Close()
	secs, err := parseSections(path, f)
	if err != nil {
		return nil, err
	}
	for _, s := range secs {
		scratch := defaultConfig()
		for _, kv := range s.kv {
			if err := scratch.set(kv.key, kv.val, s.kind != "global"); err != nil {
				return nil, fmt.Errorf("%s:%d: %v", path, kv.line, err)
			}
		}
	}
	cf.secs = secs
	return cf, nil
}

// resolve computes the effective configuration for a user: global values,
// then the sections of the user's groups, then the user's own section.
func (cf *configFile) resolve(username string, groups []string) config {
	cfg := defaultConfig()
	inGroups := map[string]bool{}
	for _, g := range groups {
		inGroups[g] = true
	}
	apply := func(match func(section) bool) {
		for _, s := range cf.secs {
			if match(s) {
				for _, kv := range s.kv {
					_ = cfg.set(kv.key, kv.val, s.kind != "global") // already validated
				}
			}
		}
	}
	apply(func(s section) bool { return s.kind == "global" })
	apply(func(s section) bool { return s.kind == "group" && inGroups[s.name] })
	apply(func(s section) bool { return s.kind == "user" && username != "" && s.name == username })
	return cfg
}

// loadConfig reads path and resolves the configuration for username / groups.
func loadConfig(path, username string, groups []string) (config, error) {
	cf, err := parseConfigFile(path)
	if err != nil {
		return config{}, err
	}
	return cf.resolve(username, groups), nil
}

func groupsOf(u *user.User) []string {
	ids, err := u.GroupIds()
	if err != nil {
		return nil
	}
	var names []string
	for _, id := range ids {
		if g, err := user.LookupGroupId(id); err == nil {
			names = append(names, g.Name)
		}
	}
	return names
}

func configPath() string {
	if p := os.Getenv("PTSCRY_CONFIG"); p != "" {
		return p
	}
	return defaultConfigPath
}

func loadEnv() (config, *user.User, error) {
	u, err := user.Current()
	if err != nil {
		return config{}, nil, err
	}
	cfg, err := loadConfig(configPath(), u.Username, groupsOf(u))
	return cfg, u, err
}

// mayAccess tells whether a user with these groups passes access_group. Root
// passes when allow_root is on (the default): it can do anything on the
// machine anyway, and the audit still records who logged in (loginuid). The
// SSH front-end does not use this: root over the network still needs the group.
func (c config) mayAccess(uid int, groups []string) bool {
	if c.AccessGroup == "" || contains(groups, c.AccessGroup) {
		return true
	}
	return uid == 0 && c.AllowRoot
}
