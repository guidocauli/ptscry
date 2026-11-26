//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// frontend.go: the integrated SSH front-end (runs unprivileged, started and
// supervised by the monitor).
//
// It terminates SSH, checks keys and certificates, asks for the second factor
// and relays the terminal to the user's host through the monitor. It holds no
// secret except the SSH host key, reads no file and can neither read audit
// data nor reach a session without a ticket from the monitor.

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	feAuthTimeout       = 30 * time.Second
	feMaxHandshaking    = 64
	feMaxPerIP          = 16
	feMaxHandshakePerIP = 4  // connections still authenticating, per address
	feMaxChannels       = 32 // a GUI tab can have 16 panes on one connection, plus its commands
	feKeyCacheTTL       = 30 * time.Second
	feReportEvery       = 60 * time.Second
	feKeepaliveEvery    = 30 * time.Second
	skFlagsPresentVer   = 0x05 // FIDO user present + user verified
)

var termRe = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,64}$`)

type ipState struct {
	conns  int
	hshake int // connections still authenticating
	tokens float64
	last   time.Time
}

type keyCacheEntry struct {
	keys []authKey
	at   time.Time
}

type frontend struct {
	in  feInit
	cfg *ssh.ServerConfig
	cas []ssh.PublicKey

	sem chan struct{}
	hs  chan struct{}

	mu    sync.Mutex
	ips   map[string]*ipState
	fails map[string]int
	ftot  int
	cache map[string]keyCacheEntry
}

func cmdFrontend(args []string) error {
	if os.Getenv("PTSCRY_FE_FD") != "3" {
		return errors.New("the front-end is started by the monitor (ptscry monitor with frontend = yes)")
	}
	if os.Getuid() == 0 {
		return errors.New("the front-end must not run as root")
	}
	if err := setNotDumpable(); err != nil {
		return err
	}
	const prSetNoNewPrivs = 38
	if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); e != 0 {
		return fmt.Errorf("no_new_privs: %v", e)
	}
	ctl := os.NewFile(3, "monitor")
	br := bufio.NewReaderSize(ctl, 1<<20)
	_ = ctl.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := br.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("reading init: %w", err)
	}
	_ = ctl.SetReadDeadline(time.Time{})
	var in feInit
	if err := json.Unmarshal(line, &in); err != nil {
		return err
	}
	// The monitor holds the other end: if it goes away, so do we.
	go func() {
		_, _ = io.Copy(io.Discard, br)
		os.Exit(1)
	}()
	fe, err := newFrontend(in)
	if err != nil {
		return err
	}
	return fe.listen()
}

func newFrontend(in feInit) (*frontend, error) {
	signer, err := ssh.ParsePrivateKey([]byte(in.HostKey))
	if err != nil {
		return nil, fmt.Errorf("host key: %w", err)
	}
	fe := &frontend{
		in: in, sem: make(chan struct{}, in.MaxConns), hs: make(chan struct{}, feMaxHandshaking),
		ips: map[string]*ipState{}, fails: map[string]int{}, cache: map[string]keyCacheEntry{},
	}
	for _, ln := range in.CAKeys {
		k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(ln))
		if err != nil {
			return nil, err
		}
		fe.cas = append(fe.cas, k)
	}
	// Every security-key signature must carry user-presence and user-verification.
	ssh.SKRequiredFlags = skFlagsPresentVer

	algos := []string{ssh.KeyAlgoSKED25519}
	if !in.FIDO2 {
		algos = append(algos, ssh.KeyAlgoED25519)
	}
	fe.cfg = &ssh.ServerConfig{
		Config: ssh.Config{
			KeyExchanges: []string{ssh.KeyExchangeCurve25519, "curve25519-sha256@libssh.org"},
			Ciphers:      []string{"chacha20-poly1305@openssh.com", ssh.CipherAES256GCM, ssh.CipherAES128GCM},
			MACs:         []string{"hmac-sha2-256-etm@openssh.com", "hmac-sha2-512-etm@openssh.com"},
		},
		PublicKeyAuthAlgorithms: algos,
		MaxAuthTries:            3,
		ServerVersion:           "SSH-2.0-ptscry",
		PublicKeyCallback:       fe.publicKey,
		AuthLogCallback:         fe.authLog,
	}
	fe.cfg.AddHostKey(signer)
	return fe, nil
}

// ------------------------------------------------------------------ monitor client

// dial opens a connection to the monitor, which must be served by root.
func (fe *frontend) dial() (*framed, error) {
	c, err := net.DialTimeout("unix", fe.in.MonitorSocket, 3*time.Second)
	if err != nil {
		return nil, err
	}
	uc, ok := c.(*net.UnixConn)
	if !ok {
		c.Close()
		return nil, errors.New("not a unix socket")
	}
	if cred, err := peerCred(uc); err != nil || cred.Uid != 0 {
		c.Close()
		return nil, errors.New("monitor socket is not served by root")
	}
	return newFramed(c), nil
}

func (fe *frontend) ask(hl hello) (okReply, error) {
	fc, err := fe.dial()
	if err != nil {
		return okReply{}, err
	}
	defer fc.c.Close()
	_ = fc.c.SetDeadline(time.Now().Add(15 * time.Second))
	return request(fc, hl)
}

// ------------------------------------------------------------------ listener

func (fe *frontend) listen() error {
	ln, err := net.Listen("tcp", fe.in.Listen)
	if err != nil {
		return err
	}
	if fe.in.Seccomp {
		// Everything the front-end needs from files is already in memory.
		on, err := installSeccomp()
		if err != nil {
			return fmt.Errorf("sandbox: %w", err) // fail closed: a front-end that was asked to be confined does not run unconfined
		}
		if on {
			fmt.Fprintln(os.Stderr, "ptscry frontend: seccomp filter active")
		} else {
			fmt.Fprintln(os.Stderr, "ptscry frontend: WARNING: no seccomp filter on this platform")
		}
	}
	go fe.reportLoop()
	for {
		nc, err := ln.Accept()
		if err != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		go fe.accept(nc)
	}
}

func remoteIP(a net.Addr) string {
	if ta, ok := a.(*net.TCPAddr); ok {
		return ta.IP.String()
	}
	h, _, _ := net.SplitHostPort(a.String())
	return h
}

// admit applies the connection limits; the caller must call done() when the
// connection ends.
func (fe *frontend) admit(ip string) (done, authenticated func(), ok bool) {
	now := time.Now()
	fe.mu.Lock()
	defer fe.mu.Unlock()
	if len(fe.ips) > 10000 {
		for k, v := range fe.ips {
			if v.conns == 0 && now.Sub(v.last) > 5*time.Minute {
				delete(fe.ips, k)
			}
		}
	}
	st := fe.ips[ip]
	if st == nil && len(fe.ips) >= 200000 { // address-rotation attack: do not grow without bound
		return nil, nil, false
	}
	if st == nil {
		st = &ipState{tokens: 10, last: now}
		fe.ips[ip] = st
	}
	rate := float64(fe.in.RatePerMin) / 60 // sustained rate; burst 10
	if rate <= 0 {
		rate = 0.5
	}
	st.tokens += now.Sub(st.last).Seconds() * rate
	if st.tokens > 10 {
		st.tokens = 10
	}
	st.last = now
	if st.conns >= feMaxPerIP || st.hshake >= feMaxHandshakePerIP || st.tokens < 1 {
		return nil, nil, false
	}
	st.tokens--
	st.conns++
	st.hshake++
	hdone, gone := false, false
	authenticated = func() {
		fe.mu.Lock()
		if !hdone {
			hdone = true
			st.hshake--
		}
		fe.mu.Unlock()
	}
	done = func() {
		authenticated()
		fe.mu.Lock()
		if !gone {
			gone = true
			st.conns--
		}
		fe.mu.Unlock()
	}
	return done, authenticated, true
}

func (fe *frontend) accept(nc net.Conn) {
	defer nc.Close()
	ip := remoteIP(nc.RemoteAddr())
	select {
	case fe.sem <- struct{}{}:
		defer func() { <-fe.sem }()
	default:
		return
	}
	done, authenticated, ok := fe.admit(ip)
	if !ok {
		return
	}
	defer done()

	select {
	case fe.hs <- struct{}{}:
	default:
		return
	}
	released := false
	release := func() {
		if !released {
			released = true
			<-fe.hs
		}
	}
	defer release()

	_ = nc.SetDeadline(time.Now().Add(feAuthTimeout))
	sc, chans, reqs, err := ssh.NewServerConn(nc, fe.cfg)
	release()
	authenticated()
	if err != nil {
		return
	}
	_ = nc.SetDeadline(time.Time{})
	defer sc.Close()
	if tc, ok := nc.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(30 * time.Second)
	}

	ext := sc.Permissions.Extensions
	if ext == nil {
		return
	}
	user := sc.User()
	ticket := ext["ticket"]
	info := &proxyInfo{User: user, Remote: nc.RemoteAddr().String(), Fingerprint: ext["fp"], KeyType: ext["key_type"],
		CertID: ext["cert_id"], Method: ext["method"]}
	fmt.Sscanf(ext["serial"], "%d", &info.Serial)
	if ticket == "" { // no second factor configured: the monitor decides on policy now
		r, err := fe.ask(hello{Op: "fe-gate", Proxy: info})
		if err != nil || r.Ticket == "" {
			return
		}
		ticket = r.Ticket
	}
	info.Ticket = ticket

	// Dead peers must not keep a session attached forever.
	stop := make(chan struct{})
	defer close(stop)
	go keepalive(sc, stop)
	go func() {
		for r := range reqs {
			if r.WantReply {
				_ = r.Reply(false, nil)
			}
		}
	}()

	var nch sync.WaitGroup
	var chMu sync.Mutex
	open := 0
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "only session channels are supported")
			continue
		}
		chMu.Lock()
		if open >= feMaxChannels {
			chMu.Unlock()
			_ = newCh.Reject(ssh.ResourceShortage, "too many sessions on this connection")
			continue
		}
		open++
		chMu.Unlock()
		nch.Add(1)
		go func() {
			defer nch.Done()
			defer func() { chMu.Lock(); open--; chMu.Unlock() }()
			fe.session(newCh, info, ext["nopty"] == "1")
		}()
	}
	nch.Wait()
}

func keepalive(sc *ssh.ServerConn, stop chan struct{}) {
	t := time.NewTicker(feKeepaliveEvery)
	defer t.Stop()
	miss := 0
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			res := make(chan error, 1)
			go func() { _, _, err := sc.SendRequest("keepalive@openssh.com", true, nil); res <- err }()
			select {
			case err := <-res:
				if err != nil {
					sc.Close()
					return
				}
				miss = 0
			case <-time.After(20 * time.Second):
				if miss++; miss >= 3 {
					sc.Close()
					return
				}
			case <-stop:
				return
			}
		}
	}
}

// ------------------------------------------------------------------ authentication

func (fe *frontend) authLog(conn ssh.ConnMetadata, method string, err error) {
	if err == nil || errors.Is(err, ssh.ErrNoAuth) {
		return
	}
	var ps *ssh.PartialSuccessError
	if errors.As(err, &ps) {
		return
	}
	ip := remoteIP(conn.RemoteAddr())
	fe.mu.Lock()
	if len(fe.fails) < 4096 || fe.fails[ip] > 0 {
		fe.fails[ip]++
	}
	fe.ftot++
	fe.mu.Unlock()
}

// reportLoop tells the monitor, once a minute, how many authentications failed
// and from where: one audit record instead of one per probe.
func (fe *frontend) reportLoop() {
	for range time.Tick(feReportEvery) {
		fe.mu.Lock()
		tot, fails := fe.ftot, fe.fails
		fe.ftot, fe.fails = 0, map[string]int{}
		fe.mu.Unlock()
		if tot == 0 {
			continue
		}
		type kv struct {
			ip string
			n  int
		}
		var l []kv
		for ip, n := range fails {
			l = append(l, kv{ip, n})
		}
		sort.Slice(l, func(i, j int) bool { return l[i].n > l[j].n })
		var src []string
		for i, e := range l {
			if i >= 5 {
				break
			}
			src = append(src, fmt.Sprintf("%s=%d", e.ip, e.n))
		}
		_, _ = fe.ask(hello{Op: "fe-report", Proxy: &proxyInfo{User: "frontend", Count: tot, Sources: src}})
	}
}

func (fe *frontend) authorizedKeys(user string) []authKey {
	fe.mu.Lock()
	if e, ok := fe.cache[user]; ok && time.Since(e.at) < feKeyCacheTTL {
		fe.mu.Unlock()
		return e.keys
	}
	fe.mu.Unlock()
	r, err := fe.ask(hello{Op: "fe-keys", Proxy: &proxyInfo{User: user}})
	var keys []authKey
	if err == nil {
		for _, ln := range r.Keys {
			// A line with an option we cannot honour grants nothing (`ptscry check` lists them).
			ak, perr := parseAuthLine(ln)
			if perr != nil {
				fmt.Fprintf(os.Stderr, "ptscry frontend: ignoring a key of %s: %v\n", user, perr)
				continue
			}
			keys = append(keys, ak)
		}
	}
	fe.mu.Lock()
	if len(fe.cache) > 4096 {
		fe.cache = map[string]keyCacheEntry{}
	}
	if err == nil {
		fe.cache[user] = keyCacheEntry{keys, time.Now()}
	}
	fe.mu.Unlock()
	return keys
}

var errDenied = errors.New("permission denied")

func (fe *frontend) isCA(k ssh.PublicKey) bool {
	m := k.Marshal()
	for _, ca := range fe.cas {
		if bytes.Equal(ca.Marshal(), m) {
			return true
		}
	}
	return false
}

// publicKey runs for every key a client offers, before it proves possession
// of it: it must have no side effects beyond a cached read.
func (fe *frontend) publicKey(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	user := conn.User()
	if !feUserRe.MatchString(user) {
		return nil, errDenied
	}
	ext := map[string]string{}
	under := key
	if cert, ok := key.(*ssh.Certificate); ok {
		if err := fe.checkCert(conn, cert); err != nil {
			return nil, errDenied
		}
		under = cert.Key
		ext["method"], ext["cert_id"] = "cert", cert.KeyId
		ext["serial"] = fmt.Sprint(cert.Serial)
		if _, ok := cert.Extensions["permit-pty"]; !ok {
			ext["nopty"] = "1"
		}
	} else {
		found := false
		for _, k := range fe.authorizedKeys(user) {
			if !bytes.Equal(k.key.Marshal(), key.Marshal()) {
				continue
			}
			if k.from != "" && !fromAllowed(k.from, remoteIP(conn.RemoteAddr())) {
				continue
			}
			if !k.expiry.IsZero() && !time.Now().Before(k.expiry) {
				continue
			}
			if k.noPTY {
				ext["nopty"] = "1"
			}
			found = true
			break
		}
		if !found {
			return nil, errDenied
		}
		ext["method"] = "key"
	}
	if t := under.Type(); t != ssh.KeyAlgoSKED25519 && (fe.in.FIDO2 || t != ssh.KeyAlgoED25519) {
		return nil, errDenied
	}
	ext["fp"], ext["key_type"] = ssh.FingerprintSHA256(under), under.Type()
	perms := &ssh.Permissions{Extensions: ext}
	if !fe.in.TOTP {
		return perms, nil
	}
	// Second factor, verified by the monitor. This closure is only used after
	// the client has proved possession of exactly this key.
	next := ssh.ServerAuthCallbacks{
		KeyboardInteractiveCallback: func(c ssh.ConnMetadata, ch ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
			answers, err := ch("", "", []string{"Verification code: "}, []bool{false})
			if err != nil || len(answers) != 1 {
				return nil, errDenied
			}
			info := &proxyInfo{User: c.User(), Remote: c.RemoteAddr().String(), Fingerprint: ext["fp"],
				KeyType: ext["key_type"], CertID: ext["cert_id"], Method: ext["method"], Code: strings.TrimSpace(answers[0])}
			fmt.Sscanf(ext["serial"], "%d", &info.Serial)
			r, err := fe.ask(hello{Op: "fe-totp", Proxy: info})
			if err != nil || r.Ticket == "" {
				return nil, errDenied
			}
			e2 := map[string]string{}
			for k, v := range ext {
				e2[k] = v
			}
			e2["ticket"] = r.Ticket
			return &ssh.Permissions{Extensions: e2}, nil
		},
	}
	return perms, &ssh.PartialSuccessError{Next: next}
}

func (fe *frontend) checkCert(conn ssh.ConnMetadata, cert *ssh.Certificate) error {
	if len(fe.cas) == 0 {
		return errDenied
	}
	if cert.CertType != ssh.UserCert || cert.SignatureKey == nil || !fe.isCA(cert.SignatureKey) {
		return errDenied // CheckCert verifies the signature, not who signed
	}
	for opt := range cert.CriticalOptions {
		if opt != "source-address" { // force-command and the like are not implemented: refuse
			return errDenied
		}
	}
	now := uint64(time.Now().Unix())
	if cert.ValidBefore == ssh.CertTimeInfinity || cert.ValidBefore <= now || cert.ValidAfter > now+60 {
		return errDenied
	}
	if cert.ValidBefore-cert.ValidAfter > uint64(fe.in.CertMaxSeconds) {
		return errDenied
	}
	okp := false
	for _, p := range cert.ValidPrincipals {
		if p == conn.User() {
			okp = true
		}
	}
	if !okp {
		return errDenied
	}
	if sa := cert.CriticalOptions["source-address"]; sa != "" && !sourceAllowed(sa, remoteIP(conn.RemoteAddr())) {
		return errDenied
	}
	cc := &ssh.CertChecker{IsUserAuthority: fe.isCA, SupportedCriticalOptions: []string{"source-address"}}
	if err := cc.CheckCert(conn.User(), cert); err != nil {
		return errDenied
	}
	return nil
}

func sourceAllowed(list, ip string) bool {
	addr := net.ParseIP(ip)
	if addr == nil {
		return false
	}
	for _, e := range strings.Split(list, ",") {
		e = strings.TrimSpace(e)
		if !strings.Contains(e, "/") {
			if p := net.ParseIP(e); p != nil && p.Equal(addr) {
				return true
			}
			continue
		}
		if _, n, err := net.ParseCIDR(e); err == nil && n.Contains(addr) {
			return true
		}
	}
	return false
}
