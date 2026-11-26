//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestTOTPVectors(t *testing.T) { // RFC 6238 appendix B (SHA-1), last 6 digits
	secret := []byte("12345678901234567890")
	for _, c := range []struct {
		unix int64
		want string
	}{{59, "287082"}, {1111111109, "081804"}, {1111111111, "050471"}, {1234567890, "005924"}, {2000000000, "279037"}} {
		if got := hotp(secret, c.unix/30); got != c.want {
			t.Errorf("t=%d: got %s want %s", c.unix, got, c.want)
		}
	}
	now := time.Unix(1111111109, 0)
	if _, ok := totpMatch(secret, "081804", now); !ok {
		t.Error("current code rejected")
	}
	if _, ok := totpMatch(secret, hotp(secret, 1111111109/30-1), now); !ok {
		t.Error("previous step should be tolerated")
	}
	if _, ok := totpMatch(secret, hotp(secret, 1111111109/30+2), now); ok {
		t.Error("code two steps ahead must be refused")
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef", "081805"} {
		if _, ok := totpMatch(secret, bad, now); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := decodeTOTPSecret("GEZD GNBV-GY3T QOJQ GEZD GNBV GY3T QOJQ"); err != nil {
		t.Error(err)
	}
	if _, err := decodeTOTPSecret("AAAA"); err == nil {
		t.Error("short secret accepted")
	}
}

func TestTicketsAreBoundToUser(t *testing.T) {
	f := newFeState()
	tk, err := f.issue("alice")
	if err != nil {
		t.Fatal(err)
	}
	if !f.valid(tk, "alice") || f.valid(tk, "bob") || f.valid("", "alice") || f.valid(strings.Repeat("0", 32), "alice") {
		t.Fatal("ticket binding wrong")
	}
	f.revokeUser("alice")
	if f.valid(tk, "alice") {
		t.Fatal("revoked ticket still valid")
	}
	f.setUID(5)
	tk, _ = f.issue("alice")
	f.setUID(6) // a new front-end process: old tickets are gone
	if f.valid(tk, "alice") {
		t.Fatal("ticket survived a front-end restart")
	}
	for i := 0; i < feTicketsUser+10; i++ {
		_, _ = f.issue("carol")
	}
	n := 0
	for _, v := range f.tickets {
		if v.user == "carol" {
			n++
		}
	}
	if n > feTicketsUser {
		t.Fatalf("%d tickets for one user", n)
	}
}

func skSig(t *testing.T, priv ed25519.PrivateKey, data []byte, flags byte) *ssh.Signature {
	t.Helper()
	appD := sha256.Sum256([]byte("ssh:"))
	dataD := sha256.Sum256(data)
	var ctr [4]byte
	binary.BigEndian.PutUint32(ctr[:], 7)
	msg := append(append(append(appD[:], flags), ctr[:]...), dataD[:]...)
	sig := ed25519.Sign(priv, msg)
	return &ssh.Signature{
		Format: ssh.KeyAlgoSKED25519,
		Blob: ssh.Marshal(struct {
			Signature []byte `ssh:"rest"`
		}{sig}),
		Rest: ssh.Marshal(struct {
			Flags   byte
			Counter uint32
		}{flags, 7}),
	}
}

func TestSecurityKeyFlagsEnforced(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	blob := ssh.Marshal(struct {
		Name        string
		KeyBytes    []byte
		Application string
	}{ssh.KeyAlgoSKED25519, pub, "ssh:"})
	key, err := ssh.ParsePublicKey(blob)
	if err != nil {
		t.Fatal(err)
	}
	old := ssh.SKRequiredFlags
	defer func() { ssh.SKRequiredFlags = old }()
	ssh.SKRequiredFlags = skFlagsPresentVer
	data := []byte("signed data")
	for _, c := range []struct {
		flags byte
		ok    bool
	}{{0x05, true}, {0x01, false}, {0x04, false}, {0x00, false}, {0x0d, true}} {
		err := key.Verify(data, skSig(t, priv, data, c.flags))
		if (err == nil) != c.ok {
			t.Errorf("flags %#x: err=%v want ok=%v", c.flags, err, c.ok)
		}
	}
	// A tampered message still fails.
	if key.Verify([]byte("other"), skSig(t, priv, data, 0x05)) == nil {
		t.Error("signature over other data accepted")
	}
}

type fakeConn struct {
	user string
	addr net.Addr
}

func (f fakeConn) User() string          { return f.user }
func (f fakeConn) SessionID() []byte     { return []byte("sid") }
func (f fakeConn) ClientVersion() []byte { return []byte("SSH-2.0-test") }
func (f fakeConn) ServerVersion() []byte { return []byte("SSH-2.0-ptscry") }
func (f fakeConn) RemoteAddr() net.Addr  { return f.addr }
func (f fakeConn) LocalAddr() net.Addr   { return f.addr }

func testFrontend(t *testing.T, mod func(*feInit)) (*frontend, ssh.Signer) {
	t.Helper()
	_, hk, _ := ed25519.GenerateKey(rand.Reader)
	blk, _ := ssh.MarshalPrivateKey(hk, "")
	caPub, caPriv, _ := ed25519.GenerateKey(rand.Reader)
	ca, _ := ssh.NewPublicKey(caPub)
	caSigner, _ := ssh.NewSignerFromKey(caPriv)
	in := feInit{HostKey: string(pem.EncodeToMemory(blk)), CAKeys: []string{strings.TrimSpace(string(ssh.MarshalAuthorizedKey(ca)))},
		FIDO2: false, TOTP: false, CertMaxSeconds: 3600, MaxConns: 8, RatePerMin: 30, MonitorSocket: "/nonexistent", DefaultSession: "main"}
	if mod != nil {
		mod(&in)
	}
	fe, err := newFrontend(in)
	if err != nil {
		t.Fatal(err)
	}
	return fe, caSigner
}

func mkCert(t *testing.T, ca ssh.Signer, mod func(*ssh.Certificate)) *ssh.Certificate {
	t.Helper()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	k, _ := ssh.NewPublicKey(pub)
	now := uint64(time.Now().Unix())
	c := &ssh.Certificate{Key: k, KeyId: "id", CertType: ssh.UserCert, ValidPrincipals: []string{"alice"},
		ValidAfter: now - 10, ValidBefore: now + 600,
		Permissions: ssh.Permissions{Extensions: map[string]string{"permit-pty": ""}}}
	if mod != nil {
		mod(c)
	}
	if err := c.SignCert(rand.Reader, ca); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCertificatePolicy(t *testing.T) {
	fe, ca := testFrontend(t, nil)
	conn := fakeConn{"alice", &net.TCPAddr{IP: net.ParseIP("10.1.2.3"), Port: 4000}}
	now := uint64(time.Now().Unix())
	cases := []struct {
		name string
		mod  func(*ssh.Certificate)
		ok   bool
	}{
		{"valid", nil, true},
		{"expired", func(c *ssh.Certificate) { c.ValidAfter, c.ValidBefore = now-1000, now-500 }, false},
		{"not yet valid", func(c *ssh.Certificate) { c.ValidAfter, c.ValidBefore = now+1000, now+1500 }, false},
		{"infinite", func(c *ssh.Certificate) { c.ValidBefore = ssh.CertTimeInfinity }, false},
		{"too long lived", func(c *ssh.Certificate) { c.ValidBefore = now + 7200 }, false},
		{"other principal", func(c *ssh.Certificate) { c.ValidPrincipals = []string{"bob"} }, false},
		{"no principals", func(c *ssh.Certificate) { c.ValidPrincipals = nil }, false},
		{"host cert", func(c *ssh.Certificate) { c.CertType = ssh.HostCert }, false},
		{"force-command", func(c *ssh.Certificate) { c.CriticalOptions = map[string]string{"force-command": "/bin/sh"} }, false},
		{"source ok", func(c *ssh.Certificate) { c.CriticalOptions = map[string]string{"source-address": "10.1.0.0/16"} }, true},
		{"source wrong", func(c *ssh.Certificate) { c.CriticalOptions = map[string]string{"source-address": "192.168.0.0/16"} }, false},
	}
	for _, c := range cases {
		cert := mkCert(t, ca, c.mod)
		perms, err := fe.publicKey(conn, cert)
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v want ok=%v", c.name, err, c.ok)
		}
		if err == nil && (perms.Extensions["method"] != "cert" || perms.Extensions["fp"] == "") {
			t.Errorf("%s: bad extensions %v", c.name, perms.Extensions)
		}
	}
	// A certificate from an unknown CA.
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	other, _ := ssh.NewSignerFromKey(otherPriv)
	if _, err := fe.publicKey(conn, mkCert(t, other, nil)); err == nil {
		t.Error("certificate from an untrusted CA accepted")
	}
	// Without a permit-pty extension a pty is refused later.
	perms, err := fe.publicKey(conn, mkCert(t, ca, func(c *ssh.Certificate) { c.Extensions = nil }))
	if err != nil || perms.Extensions["nopty"] != "1" {
		t.Errorf("nopty not flagged: %v %v", perms, err)
	}
}

func TestFIDO2RequiredRefusesPlainKeys(t *testing.T) {
	fe, ca := testFrontend(t, func(in *feInit) { in.FIDO2 = true })
	conn := fakeConn{"alice", &net.TCPAddr{IP: net.ParseIP("10.1.2.3")}}
	if _, err := fe.publicKey(conn, mkCert(t, ca, nil)); err == nil {
		t.Error("a certificate on a plain ed25519 key was accepted while FIDO2 is required")
	}
	if len(fe.cfg.PublicKeyAuthAlgorithms) != 1 || fe.cfg.PublicKeyAuthAlgorithms[0] != ssh.KeyAlgoSKED25519 {
		t.Errorf("algorithms: %v", fe.cfg.PublicKeyAuthAlgorithms)
	}
}

func TestSecondFactorIsAPartialSuccess(t *testing.T) {
	fe, ca := testFrontend(t, func(in *feInit) { in.TOTP = true })
	conn := fakeConn{"alice", &net.TCPAddr{IP: net.ParseIP("10.1.2.3")}}
	_, err := fe.publicKey(conn, mkCert(t, ca, nil))
	var ps *ssh.PartialSuccessError
	if !errors.As(err, &ps) || ps.Next.KeyboardInteractiveCallback == nil || ps.Next.PublicKeyCallback != nil {
		t.Fatalf("expected a keyboard-interactive partial success, got %v", err)
	}
}

func TestBadUserNamesRefused(t *testing.T) {
	fe, ca := testFrontend(t, nil)
	for _, u := range []string{"", "-x", "a b", "a/b", "../etc", strings.Repeat("a", 70), "a\x00b"} {
		cert := mkCert(t, ca, func(c *ssh.Certificate) { c.ValidPrincipals = []string{u} })
		if _, err := fe.publicKey(fakeConn{u, &net.TCPAddr{IP: net.ParseIP("1.2.3.4")}}, cert); err == nil {
			t.Errorf("user %q accepted", u)
		}
	}
}

func TestParseCommand(t *testing.T) {
	st := &sessState{rows: 24, cols: 80, env: map[string]string{}}
	hl, pty, err := parseCommand("", "main", st)
	if err != nil || hl.Op != "new" || hl.Name != "main" || !hl.Create || !pty {
		t.Fatalf("default: %+v %v %v", hl, pty, err)
	}
	for cmd, want := range map[string]string{
		"ptscry list": "list", "ptscry attach -r foo": "attach", "ptscry new -A -n x": "new",
		"ptscry kill foo": "kill", "ptscry stop": "stop",
	} {
		h, _, err := parseCommand(cmd, "main", st)
		if err != nil || h.Op != want {
			t.Errorf("%q: %+v %v", cmd, h, err)
		}
	}
	if h, pty, _ := parseCommand("ptscry new -d -n bg sleep 5", "main", st); pty || !h.Detached || len(h.Cmd) != 2 {
		t.Errorf("detached new: %+v pty=%v", h, pty)
	}
	for _, bad := range []string{"ls", "ptscry", "ptscry admin list", "ptscry monitor", "ptscry list; id", "ptscry list $(id)",
		"ptscry new `id`", "bash -c id", "ptscry kill", "ptscry attach a b", "ptscry\nlist", "ptscry list | sh"} {
		if _, _, err := parseCommand(bad, "main", st); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestConnectionAdmission(t *testing.T) {
	fe, _ := testFrontend(t, nil)
	var dones []func()
	ok := 0
	for i := 0; i < 40; i++ {
		if d, _, o := fe.admit("203.0.113.9"); o {
			ok++
			dones = append(dones, d)
		}
	}
	if ok > 12 { // burst of 10 (plus the trickle during the loop)
		t.Errorf("%d connections admitted from one address", ok)
	}
	if _, _, o := fe.admit("203.0.113.10"); !o {
		t.Error("another address must not be affected")
	}
	if ok > feMaxHandshakePerIP {
		t.Errorf("%d unauthenticated connections from one address (max %d)", ok, feMaxHandshakePerIP)
	}
	for _, d := range dones {
		d()
	}
	// Once the handshakes are over (done), the address can connect again.
	if _, _, o := fe.admit("203.0.113.9"); !o && fe.ips["203.0.113.9"].tokens >= 1 {
		t.Error("slots not released")
	}
}

func TestSourceAllowed(t *testing.T) {
	for _, c := range []struct {
		list, ip string
		ok       bool
	}{{"10.0.0.0/8", "10.9.9.9", true}, {"10.0.0.0/8,192.0.2.1", "192.0.2.1", true}, {"10.0.0.0/8", "11.0.0.1", false},
		{"bogus", "10.0.0.1", false}, {"10.0.0.0/8", "nonsense", false}, {"2001:db8::/32", "2001:db8::1", true}} {
		if got := sourceAllowed(c.list, c.ip); got != c.ok {
			t.Errorf("%q %q: %v", c.list, c.ip, got)
		}
	}
}

func TestFrontendConfig(t *testing.T) {
	cf, err := parseConfigFileString(`[global]
frontend = yes
frontend_listen = 127.0.0.1:2222
frontend_require_fido2 = no
frontend_totp = yes
frontend_cert_max_age = 8h
frontend_max_conns = 50
frontend_ca_keys = none
`)
	if err != nil {
		t.Fatal(err)
	}
	g := cf.resolve("", nil)
	if !g.Frontend || g.FrontendListen != "127.0.0.1:2222" || g.FrontendFIDO2 || !g.FrontendTOTP ||
		g.FrontendCertMax != 8*time.Hour || g.FrontendMaxConns != 50 || g.FrontendCAKeys != "none" {
		t.Errorf("%+v", g)
	}
	d := defaultConfig()
	if d.Frontend || !d.FrontendFIDO2 || !d.FrontendTOTP {
		t.Error("defaults must be: off, FIDO2 required, TOTP required")
	}
	for _, bad := range []string{"frontend_listen = nonsense", "frontend_user = a b", "frontend_host_key = rel/path",
		"frontend_cert_max_age = 1s", "frontend_max_conns = 0"} {
		if _, err := parseConfigFileString("[global]\n" + bad + "\n"); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := parseConfigFileString("[user:bob]\nfrontend = yes\n"); err == nil {
		t.Error("frontend must be global-only")
	}
}

func TestProxyHelloLimits(t *testing.T) {
	ok := hello{Op: "fe-keys", Proxy: &proxyInfo{User: "alice"}}
	if err := validHello(&ok); err != nil {
		t.Fatal(err)
	}
	for _, h := range []hello{
		{Op: "fe-keys"},
		{Op: "admin-list", Proxy: &proxyInfo{User: "alice"}},
		{Op: "list", Proxy: &proxyInfo{User: "alice", Code: strings.Repeat("1", 40)}},
		{Op: "list", Proxy: &proxyInfo{User: "alice", Remote: strings.Repeat("x", 500)}},
		{Op: "fe-report", Proxy: &proxyInfo{User: "frontend", Sources: make([]string, 40)}},
	} {
		h := h
		if err := validHello(&h); err == nil {
			t.Errorf("accepted: %+v", h)
		}
	}
}

func parseConfigFileString(s string) (*configFile, error) {
	f, err := os.CreateTemp("", "ptscry-conf")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	f.WriteString(s)
	f.Close()
	return parseConfigFile(f.Name())
}

func TestAuthorizedKeyOptions(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	k, _ := ssh.NewPublicKey(pub)
	base := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
	ok := map[string]func(authKey) bool{
		base:                                     func(a authKey) bool { return a.from == "" && !a.noPTY },
		`from="10.0.0.0/8,!10.9.0.0/16" ` + base: func(a authKey) bool { return a.from == "10.0.0.0/8,!10.9.0.0/16" },
		`no-pty,no-port-forwarding,no-agent-forwarding ` + base: func(a authKey) bool { return a.noPTY },
		`restrict ` + base:                    func(a authKey) bool { return a.noPTY },
		`restrict,pty ` + base:                func(a authKey) bool { return !a.noPTY },
		`expiry-time="20300101" ` + base:      func(a authKey) bool { return a.expiry.Year() == 2030 },
		`expiry-time="203001011230Z" ` + base: func(a authKey) bool { return a.expiry.Hour() == 12 },
		`verify-required ` + base:             func(a authKey) bool { return true },
	}
	for line, check := range ok {
		a, err := parseAuthLine(line)
		if err != nil || !check(a) {
			t.Errorf("%q: %+v %v", line, a, err)
		}
	}
	for _, line := range []string{
		`command="/bin/true" ` + base, `environment="A=b" ` + base, `cert-authority ` + base,
		`principals="x" ` + base, `no-touch-required ` + base, `port-forwarding ` + base,
		`from="*.example.org" ` + base, `from="" ` + base, `expiry-time="soon" ` + base, `bogus-option ` + base,
	} {
		if _, err := parseAuthLine(line); err == nil {
			t.Errorf("%q accepted", line)
		}
	}
	for _, c := range []struct {
		list, ip string
		ok       bool
	}{
		{"10.0.0.0/8", "10.1.1.1", true}, {"10.0.0.0/8,!10.9.0.0/16", "10.9.1.1", false},
		{"10.0.0.0/8,!10.9.0.0/16", "10.8.1.1", true}, {"192.168.1.*", "192.168.1.77", true},
		{"192.168.1.*", "192.168.2.77", false}, {"192.168.1.?", "192.168.1.77", false},
		{"!10.0.0.1", "10.0.0.2", false}, {"fd00::/8", "fd00::1", true}, {"10.0.0.0/8", "garbage", false},
	} {
		if got := fromAllowed(c.list, c.ip); got != c.ok {
			t.Errorf("from %q ip %q: %v", c.list, c.ip, got)
		}
	}
}

func TestMatchSyntax(t *testing.T) {
	cf, err := parseConfigFileString(`[global]
max_sessions = 2

Match User 'guido'
    MaxParallelSessions 5
Match User "rex-agent",svc
    MaxParallelSessions 16
    IdleTimeout 1h
Match Group ops
    max_parallel_sessions = unlimited
[user:old]
max_sessions = 3
Match All
allow_command no
`)
	if err != nil {
		t.Fatal(err)
	}
	if got := cf.resolve("guido", nil).MaxSessions; got != 5 {
		t.Errorf("guido: %d", got)
	}
	for _, u := range []string{"rex-agent", "svc"} {
		if c := cf.resolve(u, nil); c.MaxSessions != 16 || c.IdleTimeout != time.Hour {
			t.Errorf("%s: %+v", u, c)
		}
	}
	if got := cf.resolve("x", []string{"ops"}).MaxSessions; got != 0 {
		t.Errorf("group ops should be unlimited, got %d", got)
	}
	if got := cf.resolve("old", nil).MaxSessions; got != 3 {
		t.Errorf("old-style section: %d", got)
	}
	if got := cf.resolve("nobody", nil); got.MaxSessions != 2 || got.AllowCommand {
		t.Errorf("defaults: %+v", got)
	}
	// user section beats group section, as before
	cf, _ = parseConfigFileString("Match Group ops\nMaxParallelSessions 4\nMatch User bob\nMaxParallelSessions 9\n")
	if got := cf.resolve("bob", []string{"ops"}).MaxSessions; got != 9 {
		t.Errorf("precedence: %d", got)
	}
}

func TestJobCommands(t *testing.T) {
	st := &sessState{rows: 24, cols: 80, env: map[string]string{}}
	h, pty, err := parseCommand("ptscry run -d -n j1 sleep 5", "main", st)
	if err != nil || !h.Job || !h.Detached || h.Op != "new" || pty || len(h.Cmd) != 2 {
		t.Fatalf("run -d: %+v %v", h, err)
	}
	if _, _, err := parseCommand("ptscry run -d -k sleep 5", "main", st); err == nil {
		t.Error("run -d -k accepted")
	}
	h, _, err = parseCommand("ptscry wait -q -x j1", "main", st)
	if err != nil || !h.Remove || !h.ReadOnly || h.Op != "attach" {
		t.Fatalf("wait -x: %+v %v", h, err)
	}
	st2 := &sessState{env: map[string]string{}}
	if _, _, err := parseCommand("ptscry status -j j1", "main", st2); err != nil || st2.statusName != "j1" || !st2.listJSON {
		t.Fatalf("status: %+v %v", st2, err)
	}
	if _, _, err := parseCommand("ptscry status", "main", st2); err == nil {
		t.Error("status without name accepted")
	}
}

func TestStatusAndResults(t *testing.T) {
	r := okReply{
		Sessions: []sessionInfo{{Name: "r1", PID: 42}},
		Jobs:     []jobInfo{{Name: "f1", ExitCode: 3, Reason: "exited"}},
	}
	if txt, c := statusOf(r, "r1", false); c != 0 || !strings.Contains(txt, "running") {
		t.Errorf("running: %q %d", txt, c)
	}
	if txt, c := statusOf(r, "f1", false); c != 0 || !strings.Contains(txt, "exit=3") {
		t.Errorf("finished: %q %d", txt, c)
	}
	if _, c := statusOf(r, "zz", true); c != statusUnknown {
		t.Errorf("unknown: %d", c)
	}
	if !strings.Contains(listJSON(okReply{}), `"jobs":[]`) {
		t.Error("empty list JSON must have arrays")
	}

	h := &host{cfg: config{JobRetention: time.Hour}, sessions: map[string]*session{}}
	for i := 0; i < maxJobResults+10; i++ {
		s := &session{h: h, name: fmt.Sprintf("j%d", i), job: true, created: time.Now()}
		h.keepResult(s, 0, "exited")
		time.Sleep(time.Microsecond)
	}
	if len(h.results) != maxJobResults || h.getResult("j0") != nil || h.getResult(fmt.Sprintf("j%d", maxJobResults+9)) == nil {
		t.Fatalf("result cap not enforced oldest-first: %d", len(h.results))
	}
	h.keepResult(&session{h: h, name: "nojob", created: time.Now()}, 0, "x")
	if h.getResult("nojob") != nil {
		t.Error("a non-job session left a result")
	}
	h.mu.Lock()
	for _, r := range h.results {
		r.info.Expires = time.Now().Add(-time.Second)
	}
	h.mu.Unlock()
	if len(h.listJobs()) != 0 {
		t.Error("expired results listed")
	}
}
