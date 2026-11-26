//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// Unit tests.

package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRingWrap(t *testing.T) {
	r := newRing(8)
	r.Write([]byte("abc"))
	d, tr := r.Snapshot()
	if string(d) != "abc" || tr {
		t.Fatalf("got %q %v", d, tr)
	}
	r.Write([]byte("defgh")) // exactly full
	d, tr = r.Snapshot()
	if string(d) != "abcdefgh" || !tr {
		t.Fatalf("got %q %v", d, tr)
	}
	r.Write([]byte("ij"))
	d, _ = r.Snapshot()
	if string(d) != "cdefghij" {
		t.Fatalf("got %q", d)
	}
	r.Write([]byte("0123456789ABCDEF")) // larger than the buffer
	d, _ = r.Snapshot()
	if string(d) != "89ABCDEF" {
		t.Fatalf("got %q", d)
	}
}

func TestRingZero(t *testing.T) {
	r := newRing(0)
	r.Write([]byte("x"))
	if d, _ := r.Snapshot(); len(d) != 0 {
		t.Fatal("expected empty")
	}
}

func TestCleanReplay(t *testing.T) {
	got := cleanReplay([]byte("rtial line\nfull line\n"), true)
	if !bytes.Equal(got, []byte("\x1b[0mfull line\n")) {
		t.Fatalf("got %q", got)
	}
	if cleanReplay(nil, true) != nil {
		t.Fatal("expected nil")
	}
}

func TestAltScreen(t *testing.T) {
	if !altScreenState([]byte("x\x1b[?1049hy"), false) {
		t.Fatal("alt screen on not detected")
	}
	if altScreenState([]byte("\x1b[?1049h...\x1b[?1049l"), false) {
		t.Fatal("alt screen off not detected")
	}
	if !altScreenState([]byte("plain"), true) {
		t.Fatal("state must be unchanged")
	}
}

func TestFrameRoundtrip(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	fa, fb := newFramed(a), newFramed(b)
	go func() { _ = fa.write(msgData, []byte("hello")) }()
	typ, p, err := fb.read()
	if err != nil || typ != msgData || string(p) != "hello" {
		t.Fatalf("got %d %q %v", typ, p, err)
	}
}

func TestFrameTooBig(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() { _, _ = a.Write([]byte{msgData, 0xff, 0xff, 0xff, 0xff}) }()
	if _, _, err := newFramed(b).read(); err != errFrameTooBig {
		t.Fatalf("expected errFrameTooBig, got %v", err)
	}
}

func TestResizeCodec(t *testing.T) {
	r, c, ok := decodeResize(encodeResize(40, 120))
	if !ok || r != 40 || c != 120 {
		t.Fatal("resize roundtrip failed")
	}
	if _, _, ok := decodeResize(encodeResize(0, 10)); ok {
		t.Fatal("zero rows must be rejected")
	}
}

func TestDetachPrefixPolicy(t *testing.T) {
	// server policy key
	p := writeCfg(t, "[global]\ndetach_prefix = ctrl-a\n[user:x]\ndetach_prefix = ctrl-]\n")
	cfg, err := loadConfig(p, "x", nil)
	if err != nil || cfg.DetachPrefix != 0x1d {
		t.Fatalf("policy key: %v %v", cfg.DetachPrefix, err)
	}
	if _, err := loadConfig(writeCfg(t, "[global]\ndetach_prefix = ctrl-c\n"), "x", nil); err == nil {
		t.Fatal("reserved key must be refused")
	}
	if c := defaultConfig(); c.DetachPrefix != 0x02 {
		t.Fatal("default must be Ctrl-B")
	}
}

func writeCfg(t *testing.T, body string) string {
	p := filepath.Join(t.TempDir(), "ptscry.conf")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConfigPrecedence(t *testing.T) {
	p := writeCfg(t, `
# comment
[global]
session_lifecycle = exit_on_last_shell
max_sessions = 4
idle_timeout = 1h
audit_log = /var/log/ptscry/audit.log

[group:admins]
session_lifecycle = persistent_per_user
max_sessions = 32

[user:guido]
idle_timeout = 2d
record = output
`)
	cfg, err := loadConfig(p, "guido", []string{"admins"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Lifecycle != lifePersistent || cfg.MaxSessions != 32 ||
		cfg.IdleTimeout != 48*time.Hour || cfg.Record != "output" ||
		cfg.AuditLog != "/var/log/ptscry/audit.log" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	other, err := loadConfig(p, "nobody", nil)
	if err != nil {
		t.Fatal(err)
	}
	if other.Lifecycle != lifeExit || other.MaxSessions != 4 || other.IdleTimeout != time.Hour {
		t.Fatalf("unexpected config: %+v", other)
	}
}

func TestConfigErrors(t *testing.T) {
	cases := map[string]string{
		"unknown key":         "[global]\nbogus = 1\n",
		"bad lifecycle":       "[global]\nsession_lifecycle = forever\n",
		"global-only in user": "[user:x]\naudit_log = off\n",
		"bad section":         "[host:x]\n",
		"no value":            "[global]\nmax_sessions\n",
		"bad match":           "Match Host x\nmax_sessions 3\n",
		"match global-only":   "Match User x\naudit_log off\n",
		"bad duration":        "[global]\nidle_timeout = soon\n",
	}
	for name, body := range cases {
		if _, err := loadConfig(writeCfg(t, body), "x", nil); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	// typo in a section that does not match must still be reported
	if _, err := loadConfig(writeCfg(t, "[user:other]\nmax_sesions = 3\n"), "x", nil); err == nil {
		t.Error("typo in non-matching section must be reported")
	}
}

func TestConfigMissingFileIsDefaults(t *testing.T) {
	cfg, err := loadConfig(filepath.Join(t.TempDir(), "nope.conf"), "x", nil)
	if err != nil || cfg.Lifecycle != lifeExit {
		t.Fatalf("got %+v %v", cfg, err)
	}
}

func TestRecorderUTF8Split(t *testing.T) {
	dir := t.TempDir()
	r, err := newRecorder(dir, "t", "output", "xterm", 24, 80, 0)
	if err != nil {
		t.Fatal(err)
	}
	euro := []byte("€") // 3 bytes
	r.output(euro[:2])
	r.output(euro[2:])
	r.close()
	data, _ := os.ReadFile(r.Path)
	if !strings.Contains(string(data), `"€"`) || strings.Contains(string(data), "�") {
		t.Fatalf("split rune corrupted: %s", data)
	}
}

func TestSessionNameValidation(t *testing.T) {
	for _, ok := range []string{"main", "a.b-c_1", "s1"} {
		if !nameRe.MatchString(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "a b", "../x", "x/y", strings.Repeat("a", 65)} {
		if nameRe.MatchString(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestChainAuditResumeAndVerify(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.log")
	a, err := openChainAudit(p, false)
	if err != nil {
		t.Fatal(err)
	}
	a.log("one", fields{"x": 1})
	a.log("two", nil)
	a.f.Close()
	// a new process resumes the chain from the last record
	b, err := openChainAudit(p, false)
	if err != nil {
		t.Fatal(err)
	}
	b.log("three", fields{"seq": 999, "prev": "forged", "event": "forged"}) // core keys cannot be overridden
	if err := b.reopen(); err == nil {
		b.log("four", nil)
	}
	b.f.Close()
	n, err := verifyChain([]string{p})
	if err != nil || n != 5 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	data, _ := os.ReadFile(p)
	if strings.Contains(string(data), "forged") && strings.Contains(string(data), `"event":"forged"`) {
		t.Fatal("fields overrode core keys")
	}
}

func TestChainDetectsTampering(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.log")
	a, _ := openChainAudit(p, false)
	for i := 0; i < 5; i++ {
		a.log("e", fields{"i": i})
	}
	a.f.Close()
	data, _ := os.ReadFile(p)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	edit := filepath.Join(t.TempDir(), "edit.log")
	for name, mod := range map[string][]string{
		"edited":  {lines[0], strings.Replace(lines[1], `"i":1`, `"i":9`, 1), lines[2], lines[3], lines[4]},
		"deleted": {lines[0], lines[1], lines[3], lines[4]},
		"swapped": {lines[0], lines[2], lines[1], lines[3], lines[4]},
	} {
		os.WriteFile(edit, []byte(strings.Join(mod, "\n")+"\n"), 0o600)
		if _, err := verifyChain([]string{edit}); err == nil {
			t.Errorf("%s: tampering not detected", name)
		}
	}
	// dropping the tail is invisible to a single file, which is why syslog forwarding matters
	os.WriteFile(edit, []byte(strings.Join(lines[:3], "\n")+"\n"), 0o600)
	if _, err := verifyChain([]string{edit}); err != nil {
		t.Errorf("a truncated tail must still verify as a prefix: %v", err)
	}
}

func TestConfigUnlimitedEnabledAndScopes(t *testing.T) {
	p := writeCfg(t, `
[global]
max_sessions = 3
access_group = ptscry
[group:power]
max_sessions = unlimited
[user:mallory]
enabled = no
`)
	cf, err := parseConfigFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if c := cf.resolve("x", nil); c.MaxSessions != 3 || !c.Enabled || c.AccessGroup != "ptscry" {
		t.Fatalf("%+v", c)
	}
	if c := cf.resolve("x", []string{"power"}); c.MaxSessions != 0 {
		t.Fatalf("unlimited expected, got %d", c.MaxSessions)
	}
	if c := cf.resolve("mallory", nil); c.Enabled {
		t.Fatal("mallory must be disabled")
	}
	if _, err := loadConfig(writeCfg(t, "[group:g]\naccess_group = x\n"), "u", nil); err == nil {
		t.Fatal("access_group must be global-only")
	}
	if _, err := loadConfig(writeCfg(t, "[global]\nmax_sessions = 0\n"), "u", nil); err == nil {
		t.Fatal("0 is not a valid max_sessions (use unlimited)")
	}
}

func TestLoginCommandFilter(t *testing.T) {
	t.Setenv("SSH_ORIGINAL_COMMAND", "ptscry list; id")
	if err := cmdLogin(nil); err == nil {
		t.Fatal("metacharacters must be rejected")
	}
	t.Setenv("SSH_ORIGINAL_COMMAND", "ls")
	if err := cmdLogin(nil); err == nil {
		t.Fatal("foreign commands must be rejected")
	}
}

// ---- hardening regression tests

func TestAuditSurvivesWriteFailure(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.log")
	a, err := openChainAudit(p, false)
	if err != nil {
		t.Fatal(err)
	}
	a.log("one", nil)
	good := a.f
	// A read-only handle: every write fails, as with a full or broken disk.
	ro, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.f = ro
	a.mu.Unlock()
	a.log("two", fields{"k": "v"})
	a.log("three", nil)
	if a.healthy() {
		t.Fatal("audit must report failure while writes fail")
	}
	if a.probe() {
		t.Fatal("probe must not succeed while the disk still fails")
	}
	// Disk is back: the queued records are written, in order, with no gap.
	a.mu.Lock()
	a.f = good
	a.mu.Unlock()
	if !a.probe() {
		t.Fatal("probe must recover once writing works")
	}
	n, err := verifyChain([]string{p})
	if err != nil {
		t.Fatalf("chain broken after recovery: %v", err)
	}
	if n < 4 { // one, two, three, probe
		t.Fatalf("expected the queued records to be written, got %d", n)
	}
	b, _ := os.ReadFile(p)
	if i, j := bytes.Index(b, []byte(`"event":"two"`)), bytes.Index(b, []byte(`"event":"three"`)); i < 0 || j < i {
		t.Fatal("queued records lost or reordered")
	}
}

func TestAuditQueueIsBounded(t *testing.T) {
	a, err := openChainAudit(filepath.Join(t.TempDir(), "a.log"), false)
	if err != nil {
		t.Fatal(err)
	}
	ro, _ := os.Open(os.DevNull) // writes to a read-only descriptor fail
	good := a.f
	a.mu.Lock()
	a.f = ro
	a.mu.Unlock()
	for i := 0; i < maxPending+500; i++ {
		a.log("e", nil)
	}
	a.mu.Lock()
	pending, dropped := len(a.pending), a.dropped
	a.f = good
	a.mu.Unlock()
	if pending > maxPending || dropped < 500 {
		t.Fatalf("pending=%d dropped=%d", pending, dropped)
	}
	if !a.probe() {
		t.Fatal("no recovery")
	}
	if _, err := verifyChain([]string{a.path}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(a.path)
	if !bytes.Contains(b, []byte("audit_events_lost")) {
		t.Fatal("overflow must itself be recorded")
	}
}

func TestAuditRecordSizeCap(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.log")
	a, _ := openChainAudit(p, false)
	huge := strings.Repeat("x", 100000)
	a.log("big", fields{"cmd": huge, "other": huge, "uid": 1000, "user": "u"})
	a.close()
	b, _ := os.ReadFile(p)
	for _, l := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		if len(l) > maxRecordBytes {
			t.Fatalf("record of %d bytes exceeds the cap", len(l))
		}
	}
	// and the log can still be reopened (last-record parsing works)
	if _, err := openChainAudit(p, false); err != nil {
		t.Fatalf("reopen after a capped record: %v", err)
	}
}

func TestCleanHostFields(t *testing.T) {
	in := map[string]any{
		"session": "s1", "pid": float64(7), "cmd": strings.Repeat("a", 5000),
		"seq": float64(999), "prev": "forged", "uid": float64(0), "user": "root",
		"nested": map[string]any{"a": 1}, "list": []any{1}, "readonly": true,
	}
	out := cleanHostFields(in)
	for _, k := range []string{"seq", "prev", "uid", "user", "nested", "list"} {
		if _, ok := out[k]; ok {
			t.Errorf("field %q must not pass through", k)
		}
	}
	if len(out["cmd"].(string)) != 512 || out["session"] != "s1" || out["readonly"] != true {
		t.Errorf("unexpected result %v", out)
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"0": 0, "unlimited": 0, "10": 10, "2K": 2048, "3m": 3 << 20, "1G": 1 << 30} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"-1", "abc", "1X", "", "99999999999G"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("parseSize(%q) should fail", bad)
		}
	}
}

func TestTotalSessionsConfig(t *testing.T) {
	c := defaultConfig()
	if c.MaxTotalSessions != 1024 {
		t.Fatalf("default max_total_sessions = %d", c.MaxTotalSessions)
	}
	if err := c.set("max_total_sessions", "unlimited", false); err != nil || c.MaxTotalSessions != 0 {
		t.Fatal("unlimited")
	}
	if err := c.set("max_total_sessions", "0", false); err == nil {
		t.Fatal("0 must be rejected (use unlimited)")
	}
	if err := c.set("max_total_sessions", "5", true); err == nil {
		t.Fatal("max_total_sessions must be global-only")
	}
	if err := c.set("record_max_bytes", "1M", true); err != nil || c.RecordMaxBytes != 1<<20 {
		t.Fatal("record_max_bytes")
	}
}

func TestRateLimiter(t *testing.T) {
	m := &monitor{}
	ok := 0
	var notes int
	for i := 0; i < 200; i++ {
		a, n := m.allow(1000)
		if a {
			ok++
		}
		notes += n
	}
	if ok < rateBurst || ok > rateBurst+5 {
		t.Fatalf("served %d of 200, burst is %d", ok, rateBurst)
	}
	if notes != 1 {
		t.Fatalf("expected exactly one coalesced note, got %d", notes)
	}
	if a, _ := m.allow(1001); !a {
		t.Fatal("limits must be per uid")
	}
	for i := 0; i < 500; i++ {
		if a, _ := m.allow(0); !a {
			t.Fatal("root is exempt")
		}
	}
}

func TestCheckTrusted(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root to create root-owned files")
	}
	dir := t.TempDir() // owned by root, 0700
	_ = os.Chmod(dir, 0o755)
	f := filepath.Join(dir, "f")
	_ = os.WriteFile(f, nil, 0o644)
	if err := checkTrusted(f, true); err != nil {
		t.Fatalf("a root-owned file in root-owned directories must pass: %v", err)
	}
	_ = os.Chmod(f, 0o666)
	if checkTrusted(f, true) == nil {
		t.Fatal("world-writable file must be refused")
	}
	_ = os.Chmod(f, 0o644)
	_ = os.Chmod(dir, 0o777)
	if checkTrusted(f, true) == nil {
		t.Fatal("a file in a world-writable directory must be refused")
	}
	_ = os.Chmod(dir, 0o755)
	if err := ensureRootDir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(filepath.Join(dir, "sub"), 0o777)
	if ensureRootDir(filepath.Join(dir, "sub"), 0o700) == nil {
		t.Fatal("ensureRootDir must refuse a world-writable directory")
	}
}

func TestRecorderSizeCap(t *testing.T) {
	r, err := newRecorder(t.TempDir(), "t", "output", "xterm", 24, 80, 600)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	for i := 0; i < 100 && r.err() == nil; i++ {
		r.output(bytes.Repeat([]byte("x"), 100))
	}
	if r.err() == nil || !strings.Contains(r.err().Error(), "size limit") {
		t.Fatalf("cap not enforced: %v", r.err())
	}
}

func TestHostFloodDetection(t *testing.T) {
	mh := &mhost{}
	flooded := false
	// Simulate sustained flooding across several seconds by moving the clock window.
	for sec := int64(0); sec < 8; sec++ {
		mh.evMu.Lock()
		mh.evSec = time.Now().Unix() - 100 + sec // each iteration looks like a new second
		mh.evMu.Unlock()
		for i := 0; i < hostEventRate+50; i++ {
			mh.evMu.Lock()
			mh.evCount++
			mh.evMu.Unlock()
		}
		if mh.floodingEvents() {
			flooded = true
		}
	}
	if !flooded {
		t.Fatal("sustained flooding must be detected")
	}
	calm := &mhost{}
	for i := 0; i < 100; i++ {
		if calm.floodingEvents() {
			t.Fatal("normal traffic must not be flagged")
		}
	}
}

// ---- session launcher

func testHost() *mhost {
	return &mhost{uid: 1000, gid: 1000, gids: []uint32{1000, 27}, user: "mario",
		home: "/home/mario", shell: "/bin/bash",
		cfg: config{AllowCommand: true, MaxSessions: 4, PAMService: "ptscry"}}
}

func TestBuildLaunchSpec(t *testing.T) {
	mh := testHost()
	sp, err := buildLaunchSpec(mh, &launchRequest{Session: "main", Rows: 30, Cols: 100,
		Env: map[string]string{"TERM": "screen", "LANG": "it_IT.UTF-8", "LD_PRELOAD": "/tmp/evil.so",
			"PATH": "/tmp/evil", "SSH_AUTH_SOCK": "/tmp/x", "LC_ALL": "C"}})
	if err != nil {
		t.Fatal(err)
	}
	if sp.UID != 1000 || sp.User != "mario" || sp.Home != "/home/mario" || sp.Argv[0] != "/bin/bash" || sp.Service != "ptscry" {
		t.Fatalf("identity must come from the monitor: %+v", sp)
	}
	joined := strings.Join(sp.Env, "\n")
	for _, bad := range []string{"LD_PRELOAD", "/tmp/evil", "SSH_AUTH_SOCK"} {
		if strings.Contains(joined, bad) {
			t.Errorf("%s must not reach the session environment", bad)
		}
	}
	for _, want := range []string{"TERM=screen", "LANG=it_IT.UTF-8", "LC_ALL=C", "USER=mario", "HOME=/home/mario",
		"PTSCRY=1", "PTSCRY_SESSION=main", "PATH=/usr/local/bin:/usr/bin:/bin"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %s in %v", want, sp.Env)
		}
	}
}

func TestBuildLaunchSpecRejects(t *testing.T) {
	mh := testHost()
	long := strings.Repeat("a", 5000)
	cases := map[string]launchRequest{
		"bad name":   {Session: "../x"},
		"empty name": {Session: ""},
		"long arg":   {Session: "s", Argv: []string{"sleep", long}},
		"nul":        {Session: "s", Argv: []string{"sl\x00eep"}},
		"many args":  {Session: "s", Argv: make([]string, 65)},
		"total size": {Session: "s", Argv: []string{strings.Repeat("a", 4000), strings.Repeat("a", 4000), strings.Repeat("a", 4000), strings.Repeat("a", 4000), strings.Repeat("a", 4000), strings.Repeat("a", 4000), strings.Repeat("a", 4000), strings.Repeat("a", 4000), strings.Repeat("a", 4000)}},
		"many env": {Session: "s", Env: func() map[string]string {
			m := map[string]string{}
			for i := 0; i < 70; i++ {
				m["LC_"+strings.Repeat("A", i+1)] = "x"
			}
			return m
		}()},
	}
	for name, rq := range cases {
		rq := rq
		if _, err := buildLaunchSpec(mh, &rq); err == nil {
			t.Errorf("%s: should be refused", name)
		}
	}
	mh.cfg.AllowCommand = false
	if _, err := buildLaunchSpec(mh, &launchRequest{Session: "s", Argv: []string{"id"}}); err == nil {
		t.Error("allow_command = no must be enforced by the monitor, not only by the host")
	}
	if _, err := buildLaunchSpec(mh, &launchRequest{Session: "s"}); err != nil {
		t.Errorf("a plain shell is always allowed: %v", err)
	}
}

func TestMergeEnvProtectsCriticalVariables(t *testing.T) {
	base := []string{"HOME=/home/m", "USER=m", "TERM=xterm", "PTSCRY=1", "PTSCRY_SESSION=s", "LANG=C", "SHELL=/bin/bash"}
	pam := []string{"HOME=/root", "USER=root", "TERM=evil", "PTSCRY=0", "PTSCRY_SESSION=x", "SHELL=/bin/evil",
		"LANG=it_IT", "XDG_RUNTIME_DIR=/run/user/1000", "BROKEN", "=x"}
	got := strings.Join(mergeEnv(base, pam), "\n")
	for _, want := range []string{"HOME=/home/m", "USER=m", "TERM=xterm", "PTSCRY=1", "PTSCRY_SESSION=s", "SHELL=/bin/bash", "LANG=it_IT", "XDG_RUNTIME_DIR=/run/user/1000"} {
		if !strings.Contains(got, want+"\n") && !strings.HasSuffix(got, want) {
			t.Errorf("expected %q in %q", want, got)
		}
	}
	for _, bad := range []string{"HOME=/root", "USER=root", "TERM=evil", "PTSCRY=0", "BROKEN"} {
		if strings.Contains(got, bad) {
			t.Errorf("%q must not get through", bad)
		}
	}
}

func TestPAMServiceConfig(t *testing.T) {
	c := defaultConfig()
	if c.PAMService != "ptscry" {
		t.Fatalf("default pam_service = %q", c.PAMService)
	}
	for _, ok := range []string{"off", "ptscry", "my.svc-2"} {
		if err := c.set("pam_service", ok, false); err != nil {
			t.Errorf("%q should be accepted: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "../etc/passwd", "a b", "x/y", strings.Repeat("a", 65)} {
		if err := c.set("pam_service", bad, false); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
	if err := c.set("pam_service", "x", true); err == nil {
		t.Error("pam_service is global-only")
	}
}

func TestCheckSpecRefusesMismatch(t *testing.T) {
	if err := checkSpec(&launchSpec{Argv: nil, User: "root"}); err == nil {
		t.Error("empty command must be refused")
	}
	if err := checkSpec(&launchSpec{Argv: []string{"sh"}, User: "root", UID: 0}); err != nil {
		t.Errorf("root/0 should match: %v", err)
	}
	if err := checkSpec(&launchSpec{Argv: []string{"sh"}, User: "root", UID: 1234}); err == nil {
		t.Error("a user name that does not match the uid must be refused")
	}
}

func TestRateLimitPerUser(t *testing.T) {
	m := &monitor{}
	m.setRate(2000, 100) // burst 400
	m.setRate(2001, 1)   // burst 4
	count := func(uid int) int {
		n := 0
		for i := 0; i < 1000; i++ {
			if a, _ := m.allow(uid); a {
				n++
			}
		}
		return n
	}
	if n := count(2000); n < 400 || n > 410 {
		t.Fatalf("raised limit served %d, want ~400", n)
	}
	if n := count(2001); n < 4 || n > 8 {
		t.Fatalf("lowered limit served %d, want ~4", n)
	}
	if n := count(2002); n < rateBurst || n > rateBurst+5 {
		t.Fatalf("default served %d", n)
	}
	m.setRate(0, 5) // root exempt, never tracked
	m.setRate(2003, 0)
	if m.rl[0] != nil || m.rl[2003] != nil {
		t.Fatal("setRate must ignore uid 0 and invalid rates")
	}
}

func TestRateLimitConfig(t *testing.T) {
	cf, err := parseConfigFileString("Match User rex-agent\n    RateLimit 200\nMatch Group g\n    rate_limit 3\n")
	if err != nil {
		t.Fatal(err)
	}
	if r := cf.resolve("rex-agent", nil).RateLimit; r != 200 {
		t.Fatalf("rex-agent: %d", r)
	}
	if r := cf.resolve("x", []string{"g"}).RateLimit; r != 3 {
		t.Fatalf("group: %d", r)
	}
	if r := cf.resolve("y", nil).RateLimit; r != 10 {
		t.Fatalf("default: %d", r)
	}
	for _, bad := range []string{"RateLimit 0", "RateLimit 1001", "RateLimit x", "rate_limit = -1"} {
		if _, err := parseConfigFileString("[global]\n" + bad + "\n"); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestFrontendSeccompConfig(t *testing.T) {
	cf, err := parseConfigFileString("[global]\n")
	if err != nil || !cf.resolve("", nil).FrontendSeccomp {
		t.Fatal("seccomp must default to on")
	}
	cf, err = parseConfigFileString("[global]\nfrontend_seccomp = no\n")
	if err != nil || cf.resolve("", nil).FrontendSeccomp {
		t.Fatal("frontend_seccomp = no not honoured")
	}
	if _, err := parseConfigFileString("[user:x]\nfrontend_seccomp = no\n"); err == nil {
		t.Fatal("frontend_seccomp must be global-only")
	}
}

func TestAuditGroup(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root to chown")
	}
	g, err := user.LookupGroup("daemon")
	if err != nil {
		t.Skip("no group daemon")
	}
	gid, _ := strconv.Atoi(g.Gid)
	p := filepath.Join(t.TempDir(), "d", "audit.log")
	if err := os.Mkdir(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	a, err := openChainAudit(p, false, "daemon")
	if err != nil {
		t.Fatal(err)
	}
	st := func(path string) (os.FileMode, int) {
		fi, _ := os.Stat(path)
		return fi.Mode().Perm(), int(fi.Sys().(*syscall.Stat_t).Gid)
	}
	if m, gg := st(p); m != 0o640 || gg != gid {
		t.Fatalf("with a group: %v %d", m, gg)
	}
	if m, gg := st(filepath.Dir(p)); m != 0o750 || gg != gid {
		t.Fatalf("directory: %v %d", m, gg)
	}
	a.setGroup("no_such_group_xyz")
	if err := a.reopen(); err != nil {
		t.Fatal(err)
	}
	if m, gg := st(p); m != 0o600 || gg != 0 { // back to root-only
		t.Fatalf("unknown group must leave 0600: %v %d", m, gg)
	}
	a.setGroup("daemon")
	_ = a.reopen()
	a.setGroup("")
	_ = a.reopen()
	if m, gg := st(p); m != 0o600 || gg != 0 {
		t.Fatalf("withdrawn: %v %d", m, gg)
	}
	a.close()
}

func TestAuditGroupConfig(t *testing.T) {
	cfg, err := loadConfig(writeCfg(t, "[global]\naudit_group = ptscry-admins\n"), "x", nil)
	if err != nil || cfg.AuditGroup != "ptscry-admins" {
		t.Fatalf("%v %v", cfg.AuditGroup, err)
	}
	if _, err := loadConfig(writeCfg(t, "[global]\naudit_group = a b\n"), "x", nil); err == nil {
		t.Fatal("bad name must be refused")
	}
	if _, err := loadConfig(writeCfg(t, "[user:x]\naudit_group = adm\n"), "x", nil); err == nil {
		t.Fatal("audit_group is global-only")
	}
}

func TestParseSSHDConfig(t *testing.T) {
	cfg := parseSSHDConfig("port 22\nPort 2222\npasswordauthentication   no\npermitrootlogin\tprohibit-password\n\nauthenticationmethods publickey,password publickey\nsubsystem sftp /usr/lib/sftp-server\n")
	if cfg["port"] != "22" || cfg["passwordauthentication"] != "no" || cfg["permitrootlogin"] != "prohibit-password" ||
		cfg["authenticationmethods"] != "publickey,password publickey" || cfg["subsystem"] != "sftp /usr/lib/sftp-server" {
		t.Fatalf("%v", cfg)
	}
	if len(parseSSHDConfig("")) != 0 {
		t.Fatal("empty")
	}
}

func TestInlineComments(t *testing.T) {
	p := writeCfg(t, "[global]\nfrontend_user = ptscry-fe              # account dedicato, shell nologin\naccess_group = ptscry # gruppo\n[user:x] # utente\nshell = /bin/bash\t# la shell\n")
	cfg, err := loadConfig(p, "x", nil)
	if err != nil || cfg.FrontendUser != "ptscry-fe" || cfg.AccessGroup != "ptscry" || cfg.Shell != "/bin/bash" {
		t.Fatalf("%q %q %q %v", cfg.FrontendUser, cfg.AccessGroup, cfg.Shell, err)
	}
	for in, want := range map[string]string{"a = b#c": "a = b#c", "a = b # c": "a = b", `a = "x # y" # z`: `a = "x # y"`, "a = b\t#c": "a = b"} {
		if got := stripInlineComment(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestProcCwd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc")
	}
	want, _ := os.Getwd()
	if got := procCwd(os.Getpid()); got != want {
		t.Fatalf("procCwd = %q, want %q", got, want)
	}
	if procCwd(0) != "" || procCwd(-5) != "" {
		t.Fatal("invalid pid must give nothing")
	}
}

func procState(t *testing.T, pid int) byte {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	return s[strings.LastIndexByte(s, ')')+2]
}

func TestSessionFreezeThaw(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc")
	}
	cmd := exec.Command("sh", "-c", "sleep 30 & wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() }()
	s := &session{name: "t", pid: cmd.Process.Pid}
	var kids []int
	for i := 0; i < 50 && len(kids) < 2; i++ {
		time.Sleep(50 * time.Millisecond)
		kids = s.sessionPids()
	}
	if len(kids) < 2 {
		t.Fatalf("session processes = %v, want the shell and its sleep", kids)
	}
	if err := s.freeze(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	for _, p := range kids {
		if st := procState(t, p); st != 'T' {
			t.Errorf("pid %d state %c after freeze, want T", p, st)
		}
	}
	if !s.info().Frozen {
		t.Error("info does not say frozen")
	}
	s.thaw()
	time.Sleep(100 * time.Millisecond)
	for _, p := range kids {
		if st := procState(t, p); st == 'T' {
			t.Errorf("pid %d still stopped after thaw", p)
		}
	}
	if s.info().Frozen {
		t.Error("still frozen after thaw")
	}
}

func TestParseFreezeCommand(t *testing.T) {
	st := &sessState{rows: 24, cols: 80, env: map[string]string{}}
	for _, op := range []string{"freeze", "thaw"} {
		h, _, err := parseCommand("ptscry "+op+" foo", "main", st)
		if err != nil || h.Op != op || h.Name != "foo" {
			t.Errorf("%s: %+v %v", op, h, err)
		}
		if _, _, err := parseCommand("ptscry "+op, "main", st); err == nil {
			t.Errorf("%s without a name accepted", op)
		}
	}
}

func TestInStateNoPrefix(t *testing.T) {
	st := inState{off: true}
	out, det := st.process([]byte{0x02, 0x01, 0x05, 'd'})
	if det || string(out) != "\x02\x01\x05d" {
		t.Fatalf("out=%q detach=%v", out, det)
	}
	st = inState{}
	if out, det = st.process([]byte{0x02, 'd'}); !det || len(out) != 0 {
		t.Fatalf("normal prefix broken: %q %v", out, det)
	}
}

func TestAllowRoot(t *testing.T) {
	c := defaultConfig()
	c.AccessGroup = "ptscry"
	if !c.AllowRoot {
		t.Fatal("allow_root must default to yes")
	}
	if !c.mayAccess(0, nil) {
		t.Fatal("root refused with allow_root = yes")
	}
	if c.mayAccess(1000, []string{"users"}) || !c.mayAccess(1000, []string{"ptscry"}) {
		t.Fatal("access_group not enforced for normal users")
	}
	c.AllowRoot = false
	if c.mayAccess(0, nil) || !c.mayAccess(0, []string{"ptscry"}) {
		t.Fatal("allow_root = no must require the group from root too")
	}
	c.AccessGroup = ""
	if !c.mayAccess(1000, nil) {
		t.Fatal("empty access_group = everybody")
	}
	p := filepath.Join(t.TempDir(), "c.conf")
	if err := os.WriteFile(p, []byte("[global]\naccess_group = ptscry\nallow_root = no\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cf, err := parseConfigFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if g := cf.resolve("root", nil); g.AllowRoot {
		t.Fatal("allow_root = no not read")
	}
}

func TestSetupKeys(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root (root-owned key directory)")
	}
	dir := filepath.Join(t.TempDir(), "keys")
	g := defaultConfig()
	g.FrontendKeysDir = dir
	var out strings.Builder
	k := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl test@x\n"
	n, err := keysAdd(&out, g, "root", strings.NewReader(k+k))
	if err != nil || n != 1 {
		t.Fatalf("add: %d %v", n, err)
	}
	if n, _ := keysAdd(&out, g, "root", strings.NewReader(k)); n != 0 {
		t.Fatal("a key already present was added again")
	}
	if _, err := keysAdd(&out, g, "root", strings.NewReader(`command="ls" `+k)); err == nil {
		t.Fatal("an option the front-end cannot honour was accepted")
	}
	if _, err := keysAdd(&out, g, "root", strings.NewReader("-----BEGIN OPENSSH PRIVATE KEY-----\n")); err == nil {
		t.Fatal("a private key was accepted")
	}
	lines, err := readKeyFile(g, "root")
	if err != nil || len(lines) != 1 {
		t.Fatalf("read back: %v %v", lines, err)
	}
	if err := keysRemove(&out, g, "root", "1"); err != nil {
		t.Fatal(err)
	}
	if lines, _ := readKeyFile(g, "root"); len(lines) != 0 {
		t.Fatal("key not removed")
	}
}

func TestSetGlobalKey(t *testing.T) {
	in := "# comment\n[global]\naccess_group = ptscry\n#frontend = yes\n#frontend_listen = :2222\n\n[user:x]\nfrontend = no\n"
	out := setGlobalKey(in, "frontend", "yes")
	if !strings.Contains(out, "\nfrontend = yes\n#frontend_listen") || !strings.Contains(out, "[user:x]\nfrontend = no") {
		t.Fatalf("commented line not replaced, or another section touched:\n%s", out)
	}
	if got := setGlobalKey("[global]\nfrontend = no\n", "frontend", "yes"); got != "[global]\nfrontend = yes\n" {
		t.Fatalf("got %q", got)
	}
	if got := setGlobalKey("[global]\naccess_group = a\n", "frontend", "yes"); got != "[global]\nfrontend = yes\naccess_group = a\n" {
		t.Fatalf("got %q", got)
	}
	if got := setGlobalKey("access_group = a\n", "frontend", "yes"); !strings.HasPrefix(got, "[global]\nfrontend = yes\n") {
		t.Fatalf("got %q", got)
	}
}

func TestPrefixHelp(t *testing.T) {
	var shown string
	st := inState{help: func(s string) { shown = s }}
	out, det := st.process([]byte{'a', 0x02, 'h', 'b'})
	if det || string(out) != "ab" || !strings.Contains(shown, "Ctrl-B d") || !strings.Contains(shown, "\r\n") {
		t.Fatalf("out=%q detach=%v help=%q", out, det, shown)
	}
	shown = ""
	st.process([]byte{0x02, '?'})
	if shown == "" {
		t.Fatal("prefix ? shows no help")
	}
	st = inState{prefix: 0x01, help: func(s string) { shown = s }}
	st.process([]byte{0x01, 'h'})
	if !strings.Contains(shown, "Ctrl-A d") {
		t.Fatalf("help does not use the configured prefix: %q", shown)
	}
}
