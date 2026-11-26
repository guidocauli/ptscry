#!/usr/bin/env python3
# ptscry - Guido Cauli <guido.cauli@gmail.com> - end-to-end test of the SSH
# front-end: certificates, TOTP, tickets, abuse attempts. Must run as root
# (creates test users and removes them at the end). Needs the Go toolchain to
# build the test client (tests/sshclient) unless PTSCRY_SSHCLIENT is set.
import pwd, hashlib, hmac, json, os, shutil, signal, socket, struct, subprocess, sys, tempfile, time

SRC = os.path.abspath(os.environ.get("PTSCRY_BIN", "./ptscry"))
ROOT = tempfile.mkdtemp(prefix="ptscry-f-")
os.chmod(ROOT, 0o755)
BINDIR = os.path.join(ROOT, "bin"); os.mkdir(BINDIR, 0o755)
BIN = os.path.join(BINDIR, "ptscry")
shutil.copy(SRC, BIN); os.chmod(BIN, 0o755)
CLI = os.environ.get("PTSCRY_SSHCLIENT") or os.path.join(BINDIR, "sshclient")
if not os.path.exists(CLI):
    subprocess.run(["go", "build", "-o", CLI, "./tests/sshclient"], check=True)
SOCK = os.path.join(ROOT, "run", "monitor.sock")
AUDIT = os.path.join(ROOT, "log", "audit.log")
CONF = os.path.join(ROOT, "ptscry.conf")
MLOG = os.path.join(ROOT, "monitor.out")
ETC = os.path.join(ROOT, "etc"); KEYS = os.path.join(ETC, "authorized_keys.d"); TOTP = os.path.join(ETC, "totp")
KD = os.path.join(ROOT, "keys")  # client-side material (not trusted by the monitor)
os.makedirs(KEYS); os.makedirs(TOTP); os.makedirs(KD)
for d in (ETC, KEYS, TOTP): os.chmod(d, 0o755)
os.chmod(TOTP, 0o700)
os.chmod(KD, 0o755)

def free_port():
    s = socket.socket(); s.bind(("127.0.0.1", 0)); p = s.getsockname()[1]; s.close(); return p
PORT = free_port()
ADDR = f"127.0.0.1:{PORT}"

USERS = ["pt_alice", "pt_bob", "pt_carol", "pt_dave", "pt_erin", "pt_frank", "pt_grace", "pt_heidi", "pt_ivan", "pt_judy", "pt_karl", "pt_leo", "pt_mike"]
FE = "pt_fe"
fails = []
ENV = dict(os.environ, PTSCRY_CONFIG=CONF, PATH="/usr/local/bin:/usr/bin:/bin")

def sh(*a, **k): return subprocess.run(a, capture_output=True, text=True, **k)
def check(cond, msg):
    print(("PASS " if cond else "FAIL ") + msg)
    if not cond: fails.append(msg)

def hotp(secret, counter):
    h = hmac.new(secret, struct.pack(">Q", counter), hashlib.sha1).digest()
    o = h[-1] & 0xf
    return "%06d" % ((struct.unpack(">I", h[o:o+4])[0] & 0x7fffffff) % 1000000)
import base64
def code_for(user, delta=0):
    b = open(os.path.join(TOTP, user)).read().strip()
    sec = base64.b32decode(b + "=" * (-len(b) % 8))
    return hotp(sec, int(time.time()) // 30 + delta)

def events():
    return [json.loads(l) for l in open(AUDIT) if l.strip()]
def has_event(name, **kv):
    return any(e["event"] == name and all(e.get(k) == v for k, v in kv.items()) for e in events())
def find_events(name, **kv):
    return [e for e in events() if e["event"] == name and all(e.get(k) == v for k, v in kv.items())]

def write_conf(extra=""):
    with open(CONF, "w") as f:
        f.write(f"""[global]
monitor_socket = {SOCK}
audit_log = {AUDIT}
record_dir = {ROOT}/log/rec
access_group = ptscry_t
pam_service = off
max_sessions = 4
frontend = yes
frontend_listen = {ADDR}
frontend_user = {FE}
frontend_host_key = {KD}/hostkey
frontend_ca_keys = {KD}/ca.pub
frontend_authorized_keys_dir = {KEYS}
frontend_require_fido2 = no
frontend_totp = yes
frontend_totp_dir = {TOTP}
frontend_cert_max_age = 1h
frontend_rate = 100000
{extra}
""")
    os.chmod(CONF, 0o644)

def cli(*args, user=None, key=None, cert=None, totp=None, **flags):
    a = [CLI, "run", "-addr", ADDR, "-user", user or "pt_alice", "-key", key or f"{KD}/{user or 'pt_alice'}"]
    if cert: a += ["-cert", cert]
    if totp: a += ["-totp", totp]
    for k, v in flags.items():
        k = k.replace("_", "-")
        if v is True: a += ["-" + k]
        elif v is not False and v is not None: a += ["-" + k, str(v)]
    a += ["--"] + list(args)
    return subprocess.run(a, capture_output=True, text=True, timeout=40)

def mkkey(name):
    sh(CLI, "gen-key", f"{KD}/{name}")
def mkcert(user, principal=None, secs=600, ident=None, src="", serial=0, ca="ca", key=None):
    out = f"{KD}/{user}-{ident or 'c'}-{secs}.cert"
    r = sh(CLI, "sign", f"{KD}/{ca}", f"{KD}/{key or user}.pub", out, ident or f"id-{user}", principal or user, str(secs), src, str(serial))
    assert r.returncode == 0, r.stderr
    return out

def frame(t, payload):
    return struct.pack(">BI", t, len(payload)) + payload
def raw_request(hello, user=None):
    """send one hello on the monitor socket, optionally as another uid; returns (type, payload)"""
    code = f"""
import socket,struct,sys,json
s=socket.socket(socket.AF_UNIX); s.settimeout(10); s.connect({SOCK!r})
p={json.dumps(hello)!r}.encode()
s.sendall(struct.pack('>BI',1,len(p))+p)
h=s.recv(5)
if len(h)<5: print('EOF'); sys.exit()
t,n=struct.unpack('>BI',h)
d=b''
while len(d)<n: d+=s.recv(n-len(d))
print(t, d.decode())
"""
    cmd = ["python3", "-c", code]
    if user: cmd = ["setpriv", f"--reuid={user}", f"--regid={user}", "--init-groups"] + cmd
    r = subprocess.run(cmd, capture_output=True, text=True, timeout=20)
    return r.stdout.strip() + r.stderr.strip()

def procs_of(user, needle):
    r = sh("pgrep", "-u", user, "-f", needle)
    return [int(x) for x in r.stdout.split()]

mon = None
def start_monitor():
    global mon
    mon = subprocess.Popen([BIN, "monitor", "-c", CONF], stdout=open(MLOG, "a"), stderr=subprocess.STDOUT)
    for _ in range(60):
        if os.path.exists(SOCK): break
        time.sleep(0.1)
    for _ in range(60):  # wait for the front-end to listen
        try:
            socket.create_connection(("127.0.0.1", PORT), 1).close(); break
        except OSError:
            time.sleep(0.2)
    time.sleep(0.3)

def fe_pid():
    return procs_of(FE, "ptscry frontend")

try:
    sh("groupadd", "-f", "ptscry_t")
    for u in USERS: sh("useradd", "-m", "-s", "/bin/bash", u)
    sh("useradd", "-r", "-s", "/usr/sbin/nologin", "-M", FE)
    for u in ("pt_alice", "pt_bob", "pt_dave", "pt_erin", "pt_frank", "pt_grace", "pt_heidi", "pt_ivan", "pt_judy", "pt_karl", "pt_leo", "pt_mike"): sh("usermod", "-aG", "ptscry_t", u)
    # carol is deliberately NOT in the access group.

    # trust material
    sh(CLI, "gen-key", f"{KD}/hostkey"); sh(CLI, "gen-key", f"{KD}/ca"); sh(CLI, "gen-key", f"{KD}/evilca")
    os.chmod(f"{KD}/hostkey", 0o600)
    for u in USERS + ["root"]:
        mkkey(u)
    write_conf()
    # enrol TOTP for everyone except erin (no secret) and root
    for u in ("pt_alice", "pt_bob", "pt_carol", "pt_dave", "pt_frank", "pt_grace", "pt_heidi", "pt_ivan", "pt_mike"):
        r = sh(BIN, "totp-init", u, env=ENV)
        assert r.returncode == 0, r.stderr
    check(oct(os.stat(f"{TOTP}/pt_alice").st_mode & 0o777) == "0o600" and os.stat(f"{TOTP}/pt_alice").st_uid == 0, "totp secrets are root:root 0600")
    check(sh(BIN, "totp-init", "pt_alice", env=ENV).returncode != 0, "totp-init refuses to overwrite without -f")
    check(sh(BIN, "totp-init", "-f", "pt_alice", env=ENV).returncode == 0, "totp-init -f replaces the secret")
    # plain authorized key for bob
    shutil.copy(f"{KD}/pt_bob.pub", f"{KEYS}/pt_bob")
    os.chmod(f"{KEYS}/pt_bob", 0o644)

    start_monitor()
    print("== start-up")
    check(has_event("frontend_start"), "monitor started the front-end")
    pids = fe_pid()
    check(len(pids) == 1, "one front-end process, running as the unprivileged account")
    if pids:
        st = open(f"/proc/{pids[0]}/status").read()
        check("NoNewPrivs:\t1" in st, "front-end has no_new_privs")
        check("Seccomp:\t2" in st, "front-end runs under a seccomp filter")
        check(len(set(l.split("\t")[1] for l in st.splitlines() if l.startswith("Seccomp:"))) == 1, "seccomp status readable")
        check(open(f"/proc/{pids[0]}/cmdline").read().startswith(BIN), "front-end runs the monitor's own binary")
    fe0 = pids[0] if pids else 0
    ev = find_events("frontend_start")[0]
    check(ev["fido2_required"] is False and ev["totp"] is True and ev["listen"] == ADDR, "frontend_start records the policy in force")

    print("== certificate + TOTP login")
    # A TOTP code can be used once and counters must increase: each user logs in
    # with delta -1, 0, +1 in this order, then waits for the next window.
    r = cli("ptscry", "list", user="pt_alice", cert=mkcert("pt_alice"), totp=code_for("pt_alice", -1))
    check("no sessions" in r.stdout and "[exit=0]" in r.stdout, f"cert + TOTP: `ptscry list` works ({r.stdout.strip()[:80]} {r.stderr.strip()[:80]})")
    lg = find_events("fe_login", user="pt_alice")
    check(len(lg) >= 1 and lg[-1]["method"] == "publickey+totp" and lg[-1]["remote"].startswith("127.0.0.1:")
          and lg[-1]["fp"].startswith("SHA256:") and lg[-1]["cert_id"] == "id-pt_alice", "fe_login audited with method, remote, key fingerprint, cert id")
    rq = [e for e in find_events("request", user="pt_alice") if e.get("via") == "frontend"]
    check(rq and rq[-1]["op"] == "list" and rq[-1]["peer_uid"] == int(sh("id", "-u", "pt_alice").stdout), "request audited as via=frontend with the user's uid")
    check("ticket" not in json.dumps([e for e in events() if "ticket_id" not in e]), "no ticket or code ever reaches the audit log")

    print("== interactive session through ssh")
    r = cli(user="pt_alice", cert=mkcert("pt_alice", ident="i2"), totp=code_for("pt_alice", 0), pty=True,
            script='700:"echo OUT=$((6*7)) USER=$(id -un)\\n";;500:"\\u0002d"', tail=4000)
    check("OUT=42 USER=pt_alice" in r.stdout and "detached" in r.stdout and "[exit=0]" in r.stdout,
          f"shell runs as the user, Ctrl-B d detaches ({r.stdout[-200:]!r})")
    r = cli("ptscry", "list", user="pt_alice", cert=mkcert("pt_alice", ident="i3"), totp=code_for("pt_alice", 1))
    check("main" in r.stdout, "the session main persists after detach")
    time.sleep(31)
    r = cli(user="pt_alice", cert=mkcert("pt_alice", ident="i4"), totp=code_for("pt_alice", 1), pty=True,
            script='600:"echo AGAIN\\n";;300:"sleep 300 &\\n"', tail=1500)  # then the client just drops
    check("OUT=42" in r.stdout and "AGAIN" in r.stdout, "reattach replays the earlier output")
    time.sleep(1)
    adm = sh(BIN, "admin", "list", env=ENV).stdout
    check("main" in adm and len(procs_of("pt_alice", "sleep 300")) == 1, "a dropped connection leaves the session and its jobs running")
    check(has_event("detach") and has_event("session_new", user="pt_alice"), "host events are still audited")

    print("== monitor re-exec with a live ssh connection")
    fe_before, starts_before = fe_pid(), len(find_events("frontend_start"))
    a = [CLI, "run", "-addr", ADDR, "-user", "pt_ivan", "-key", f"{KD}/pt_ivan", "-cert", mkcert("pt_ivan"),
         "-totp", code_for("pt_ivan", -1), "-pty", "-script", '1500:"echo PRE=$((6*7))\\n";;7000:"echo POST=$((6*8))\\n";;800:"\\u0002d"',
         "-tail", "20000", "--"]
    p = subprocess.Popen(a, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    time.sleep(4)
    r = sh(BIN, "admin", "reexec", env=ENV)
    out, _ = p.communicate(timeout=60)
    check(r.returncode == 0 and "PRE=42" in out and "POST=48" in out, f"an ssh connection survives a monitor re-exec and keeps working ({out[-160:]!r})")
    check(fe_pid() == fe_before and len(find_events("frontend_start")) == starts_before, "the front-end process is not restarted by a re-exec")
    rs = [e for e in events() if e["event"] == "monitor_resumed"]
    check(bool(rs) and rs[-1]["front_end"] is True, "monitor_resumed reports the front-end it took over")
    r = cli("ptscry", "list", user="pt_ivan", cert=mkcert("pt_ivan", ident="r2"), totp=code_for("pt_ivan", 0))
    check("main" in r.stdout and "[exit=0]" in r.stdout, "new logins work after the re-exec")

    print("== second factor")
    c_dave = mkcert("pt_dave")
    r = cli("ptscry", "list", user="pt_dave", cert=c_dave, totp="000000", expect_auth_fail=True)
    check("AUTH-FAILED" in r.stdout, "wrong TOTP code is refused")
    good = code_for("pt_dave")
    r = cli("ptscry", "list", user="pt_dave", cert=c_dave, totp=good)
    check("[exit=0]" in r.stdout, "the right code works")
    r = cli("ptscry", "list", user="pt_dave", cert=mkcert("pt_dave", ident="r"), totp=good, expect_auth_fail=True)
    check("AUTH-FAILED" in r.stdout, "replay of a used code is refused")
    for i in range(6):
        cli("ptscry", "list", user="pt_dave", cert=mkcert("pt_dave", ident=f"l{i}"), totp="%06d" % (100000 + i), expect_auth_fail=True)
    check(has_event("fe_totp_locked", user="pt_dave"), "5 wrong codes lock the account (audited)")
    r = cli("ptscry", "list", user="pt_dave", cert=mkcert("pt_dave", ident="afterlock"), totp=code_for("pt_dave", 1), expect_auth_fail=True)
    check("AUTH-FAILED" in r.stdout, "even a correct code is refused while locked")
    r = cli("ptscry", "list", user="pt_erin", cert=mkcert("pt_erin"), totp="123456", expect_auth_fail=True)
    check("AUTH-FAILED" in r.stdout and has_event("fe_denied", user="pt_erin", reason="no usable TOTP secret for this account"),
          "a user without an enrolled secret cannot log in")

    print("== plain authorized keys")
    r = cli("ptscry", "list", user="pt_bob", totp=code_for("pt_bob", 0))
    check("[exit=0]" in r.stdout, "authorized_keys entry + TOTP works")
    lg = find_events("fe_login", user="pt_bob")
    check(lg and lg[-1]["key_type"] == "ssh-ed25519" and lg[-1].get("cert_id") is None, "audited as a plain key")
    r = cli("ptscry", "list", user="pt_grace", key=f"{KD}/pt_alice", totp=code_for("pt_grace"), expect_auth_fail=True)
    check("AUTH-FAILED" in r.stdout, "a key that is not in the user's file is refused")
    shutil.copy(f"{KD}/pt_heidi.pub", f"{KEYS}/pt_heidi"); os.chmod(f"{KEYS}/pt_heidi", 0o666)
    r = cli("ptscry", "list", user="pt_heidi", totp=code_for("pt_heidi"), expect_auth_fail=True)
    check("AUTH-FAILED" in r.stdout, "a world-writable authorized_keys file is not trusted")

    print("== portable client (ptscry -H), with a terminal")
    import pty, select
    CFG = os.path.join(ROOT, "cfg"); os.makedirs(os.path.join(CFG, "ptscry"), exist_ok=True)
    cenv = dict(ENV, HOME=ROOT, XDG_CONFIG_HOME=CFG)
    mcert = mkcert("pt_mike", ident="mk")
    class PT:
        def __init__(self, args, env=None):
            self.pid, self.fd = pty.fork()
            if self.pid == 0:
                os.execve(BIN, [BIN] + args, env or cenv)
            self.buf = b""
        def expect(self, text, timeout=10):
            end = time.time() + timeout; t = text.encode()
            while time.time() < end and t not in self.buf:
                r, _, _ = select.select([self.fd], [], [], 0.2)
                if r:
                    try: d = os.read(self.fd, 65536)
                    except OSError: break
                    if not d: break
                    self.buf += d
            return t in self.buf
        def send(self, s): os.write(self.fd, s.encode() if isinstance(s, str) else s)
        def wait(self, timeout=15):
            end = time.time() + timeout
            while time.time() < end:
                self.expect("\x00never", 0.3)
                p, st = os.waitpid(self.pid, os.WNOHANG)
                if p: return os.waitstatus_to_exitcode(st)
            os.kill(self.pid, signal.SIGKILL); os.waitpid(self.pid, 0); return None
        def text(self): return self.buf.decode(errors="replace")
    base = ["-H", f"pt_mike@{ADDR}", "-i", f"{KD}/pt_mike", "-c", mcert]
    t = PT(base + ["list"])
    check(t.expect("authenticity") and "SHA256:" in t.text(), "first connection shows the server key fingerprint")
    t.send("yes\n")
    check(t.expect("Verification code"), "the TOTP code is asked on the terminal")
    t.send(code_for("pt_mike", -1) + "\n")
    check(t.wait() == 0 and "no sessions" in t.text(), f"list works through ptscry -H ({t.text()[-120:]!r})")
    kh = open(os.path.join(CFG, "ptscry", "known_hosts")).read()
    check(ADDR in kh and "SHA256:" in kh, "the key is remembered in known_hosts")
    t = PT(base + ["new", "-A", "-n", "main"])
    check(t.expect("Verification code") and "authenticity" not in t.text(), "second connection: known key, no question")
    t.send(code_for("pt_mike", 0) + "\n"); time.sleep(1.5)
    t.send("echo OUT=$((6*7)) USER=$(id -un)\n")
    check(t.expect("OUT=42 USER=pt_mike"), "interactive shell as the user through the portable client")
    t.send(b"\x02h")
    check(t.expect("keys inside a session") and "Ctrl-B d" in t.text(), "Ctrl-B h shows the key help through the front-end")
    t.send("PS1=IN-main\\$\\ \n"); time.sleep(0.5)
    t.send(b"\x02c"); time.sleep(1.5); t.send("PS1=IN-new\\$\\ \n"); time.sleep(0.5); t.send("echo fe-new-$((3*3))\n")
    check(t.expect("fe-new-9") and b"IN-new" in t.buf, "Ctrl-B c opens a new session through the front-end")
    t.send(b"\x02s"); check(t.expect("ptscry - sessions") and t.expect("main"), "Ctrl-B s lists the sessions through the front-end")
    t.send("main\r"); time.sleep(1); t.buf = b""; t.send("echo fe-back\n")
    check(t.expect("IN-main$ echo fe-back"), "choosing main switches back through the front-end")
    t.send(b"\x02L"); time.sleep(1); t.buf = b""; t.send("echo fe-last\n")
    check(t.expect("IN-new$ echo fe-last"), "Ctrl-B L through the front-end")
    t.send(b"\x02$"); t.expect("New name"); t.send(b"\x15fe-renamed\r")
    check(t.expect("session renamed fe-renamed"), "Ctrl-B $ renames through the front-end")
    t.send(b"\x02L"); time.sleep(1)
    t.send(b"\x02d")
    rc = t.wait(); check(rc == 0 and "detached" in t.text(), f"Ctrl-B d detaches (rc={rc} {t.text()[-160:]!r})")
    open(os.path.join(CFG, "ptscry", "client.conf"), "w").write("prefix = ctrl-a\n"); os.chmod(os.path.join(CFG, "ptscry", "client.conf"), 0o600)
    t = PT(base + ["attach", "main"])
    check(t.expect("Verification code"), "attach asks the code")
    t.send(code_for("pt_mike", 1) + "\n"); time.sleep(1.5)
    t.send(b"\x02d"); time.sleep(0.7)
    t.send("echo STILL-HERE\n")
    check(t.expect("STILL-HERE") and os.waitpid(t.pid, os.WNOHANG) == (0, 0), "client.conf prefix: Ctrl-B d no longer detaches")
    t.send(b"\x01d")
    check(t.wait() == 0 and "detached" in t.text(), "client.conf prefix: Ctrl-A d detaches (announced to the front-end)")
    os.unlink(os.path.join(CFG, "ptscry", "client.conf"))
    # wrong pinned fingerprint and a changed key never reach authentication
    t = PT(base + ["-F", "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "list"])
    check(t.wait() == 255 and "does not match" in t.text() and "Verification" not in t.text(), "-F with another fingerprint: refused before authentication")
    kpath = os.path.join(CFG, "ptscry", "known_hosts")
    open(kpath, "w").write(f"{ADDR} SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n")
    t = PT(base + ["list"])
    check(t.wait() == 255 and "HAS CHANGED" in t.text() and "Verification" not in t.text(), "a changed server key is refused before authentication")
    open(kpath, "w").write(kh)
    t = PT(["-H", f"pt_mike@{ADDR}", "-i", f"{KD}/pt_mike", "-c", mcert, "list; id"])
    check(t.wait() == 2 and "only letters" in t.text(), "arguments the front-end would refuse are rejected locally")
    time.sleep(62)  # three TOTP windows are used up above
    t = PT(base + ["run", "-d", "-n", "pj", "sh", "-c", "exit"])
    t.expect("Verification code"); t.send(code_for("pt_mike", 0) + "\n")
    rc = t.wait()
    check(rc == 0 and "pj" in t.text(), f"run -d through the portable client prints the job name ({rc} {t.text()[-200:]!r})")
    time.sleep(1)
    t = PT(base + ["wait", "-q", "pj"])
    t.expect("Verification code"); t.send(code_for("pt_mike", 1) + "\n")
    rc = t.wait()
    check(rc == 0, f"wait returns the job's exit code (0) ({rc} {t.text()[-200:]!r})")
    t = PT(["-H", f"pt_mike@{ADDR}", "-i", f"{KD}/nonexistent", "list"])
    check(t.wait() == 255, "an unreadable key file is a clear error")

    print("== authorized_keys options")
    def keyline(u, opts): return (opts + " " if opts else "") + open(f"{KD}/{u}.pub").read()
    def put(u, opts):
        open(f"{KEYS}/{u}", "w").write(keyline(u, opts)); os.chmod(f"{KEYS}/{u}", 0o644)
    def reached_totp(u):  # these users have no TOTP secret: getting that far means the key was accepted
        return len(find_events("fe_denied", user=u, reason="no usable TOTP secret for this account"))
    for u, opts in (("pt_ivan", 'from="192.0.2.0/24"'), ("pt_judy", 'command="/bin/true"'),
                    ("pt_karl", 'expiry-time="20200101"'), ("pt_leo", 'from="127.0.0.0/8,!10.0.0.0/8",no-port-forwarding')):
        put(u, opts)
        cli("ptscry", "list", user=u, totp="123456", expect_auth_fail=True)
    check(reached_totp("pt_ivan") == 0, "from= that does not match the client address: key refused")
    check(reached_totp("pt_judy") == 0, "an unsupported option (command=) makes the line grant nothing")
    check(reached_totp("pt_karl") == 0, "expired expiry-time: key refused")
    check(reached_totp("pt_leo") == 1, "from= matching the client (and harmless options): key accepted")
    time.sleep(31)  # key cache
    put("pt_bob", 'no-pty,from="127.0.0.1"')
    r = cli(user="pt_bob", totp=code_for("pt_bob", 1), pty=True)
    check("PTY-REFUSED" in r.stdout, "no-pty in authorized_keys: no terminal is granted")

    print("== certificates that must be refused")
    for label, kw in [
        ("expired", dict(secs=1)),
        ("wrong principal", dict(principal="pt_bob")),
        ("signed by an unknown CA", dict(ca="evilca")),
        ("longer than frontend_cert_max_age", dict(secs=7200)),
        ("no expiry", dict(secs=0)),
        ("source-address elsewhere", dict(src="192.0.2.0/24")),
    ]:
        c = mkcert("pt_grace", ident=label.replace(" ", "_"), **kw)
        if label == "expired": time.sleep(3)
        r = cli("ptscry", "list", user="pt_grace", cert=c, totp=code_for("pt_grace", 0), expect_auth_fail=True)
        check("AUTH-FAILED" in r.stdout, f"certificate refused: {label}")
    r = cli("ptscry", "list", user="pt_grace", cert=mkcert("pt_grace", ident="srcok", src="127.0.0.0/8"), totp=code_for("pt_grace", 0))
    check("[exit=0]" in r.stdout, "source-address matching the client is accepted")

    print("== accounts that must never get in")
    r = cli("ptscry", "list", user="root", cert=mkcert("root"), totp="123456", expect_auth_fail=True)
    check("AUTH-FAILED" in r.stdout and has_event("fe_denied", user="root"), "root cannot log in over the front-end")
    r = cli("ptscry", "list", user="pt_carol", cert=mkcert("pt_carol"), totp=code_for("pt_carol"), expect_auth_fail=True)
    check("AUTH-FAILED" in r.stdout and any(e.get("reason", "").startswith("not a member of access group") for e in find_events("fe_denied", user="pt_carol")),
          "a user outside access_group is refused")
    r = cli("ptscry", "list", user=FE, cert=mkcert(FE, key="root"), key=f"{KD}/root", totp="123456", expect_auth_fail=True)
    check("AUTH-FAILED" in r.stdout, "the front-end account itself cannot be impersonated")
    r = cli("ptscry", "list", user="nosuchuser", cert=mkcert("pt_alice", principal="nosuchuser", ident="ghost"), key=f"{KD}/pt_alice", totp="123456", expect_auth_fail=True)
    check("AUTH-FAILED" in r.stdout, "unknown user refused")

    print("== what an SSH client may not do")
    canary = os.path.join(ROOT, "canary")
    SKF = f"/tmp/ptscry-fe-sk-{os.getpid()}"
    cmds = [f"touch {canary}", "id", "ptscry admin list", "ptscry monitor", f"ptscry list; touch {canary}", f"ptscry list $(touch {canary})", "sh -c id"]
    r = cli(user="pt_frank", cert=mkcert("pt_frank"), totp=code_for("pt_frank"), cmds=";;".join(cmds + ["ptscry run echo RUN-OK-FROM-JOB", "ptscry run false", "ptscry run -d -n fj false", "ptscry wait -q fj", "ptscry status -j fj", "ptscry status -j nosuch", "ptscry list -j", "ptscry wait -q -x fj", "ptscry status fj",
        "ptscry new -d -n fs1", "ptscry rename fs1 fs2", f"ptscry send-keys fs2 touch Space {SKF} Enter", "ptscry notify fs2 hello there",
        'ptscry layout save fl1<<<{"v":1,"s":"fs2"}', "ptscry layout show fl1", "ptscry layout list", "ptscry layout rm fl1", "ptscry layout get fl1",
        "ptscry rename fs2 ../x", "ptscry layout save fl2<<<[1]"]))
    chunks = {c.split('"', 1)[0]: c for c in r.stdout.split('CMD "')[1:]}
    check("RUN-OK-FROM-JOB" in chunks.get("ptscry run echo RUN-OK-FROM-JOB", "") and "[exit=0]" in chunks.get("ptscry run echo RUN-OK-FROM-JOB", ""),
          "`ptscry run` over ssh without a terminal streams the job and returns its exit status " + repr(chunks.get("ptscry run echo RUN-OK-FROM-JOB")))
    check("[exit=1]" in chunks.get("ptscry run false", "") and "not permitted" not in chunks.get("ptscry run false", ""),
          "the job's failing exit status reaches the ssh client")
    ch = lambda c: chunks.get(c, "")
    check("fj" in ch("ptscry run -d -n fj false") and "[exit=0]" in ch("ptscry run -d -n fj false"), "run -d over ssh submits a job and returns at once")
    check("[exit=1]" in ch("ptscry wait -q fj"), "wait over ssh collects the finished job's exit status")
    check('"state":"finished"' in ch("ptscry status -j fj") and '"exit_code":1' in ch("ptscry status -j fj"), "status over ssh reports the finished job")
    check('"state":"unknown"' in ch("ptscry status -j nosuch") and "[exit=4]" in ch("ptscry status -j nosuch"), "status of an unknown job: JSON, exit 4")
    check('"jobs":[{' in ch("ptscry list -j"), "list -j over ssh shows the finished job")
    check("[exit=4]" in ch("ptscry status fj"), "wait -x discarded the result")
    check("renamed fs1 to fs2" in ch("ptscry rename fs1 fs2"), "rename over ssh " + repr(ch("ptscry rename fs1 fs2")))
    check("[exit=0]" in ch(f"ptscry send-keys fs2 touch Space {SKF} Enter"), "send-keys over ssh")
    check("shown to 0" in ch("ptscry notify fs2 hello there"), "notify over ssh (no client attached)")
    check("saved" in ch("ptscry layout save fl1") and '{"v":1,"s":"fs2"}' in ch("ptscry layout show fl1") and "fl1" in ch("ptscry layout list"), "layout save (standard input) / show / list over ssh")
    check("removed" in ch("ptscry layout rm fl1") and "no such layout" in ch("ptscry layout get fl1"), "layout rm over ssh")
    check("[exit=1]" in ch("ptscry rename fs2 ../x") and "[exit=1]" in ch("ptscry layout save fl2"), "bad names and layouts are refused over ssh")
    time.sleep(1)
    check(os.path.exists(SKF) and os.stat(SKF).st_uid == pwd.getpwnam("pt_frank").pw_uid, "the keys sent over ssh ran in the user's session")
    sh(BIN, "admin", "kill", "pt_frank", "fs2", env=ENV)
    for cmd in cmds:
        line = [c for c in r.stdout.split('CMD "')[1:] if c.startswith(cmd + '"')]
        check(bool(line) and "not permitted" in line[0] and "[exit=1]" in line[0], f"command refused: {cmd[:40]!r}")
    check(not os.path.exists(canary), "no command was ever run")
    r = cli(user="pt_heidi", key=f"{KD}/pt_heidi", cert=mkcert("pt_heidi"), totp=code_for("pt_heidi"), dial="127.0.0.1:22", x11=True, cmds="SHELL")
    check("DIAL-REFUSED" in r.stdout, "port forwarding (direct-tcpip) is refused")
    check("X11-ACCEPTED: false" in r.stdout, "X11 forwarding is refused")
    check("terminal is required" in r.stdout, "a shell without a terminal is refused")

    print("== the trust boundary (raw requests to the monitor)")
    out = raw_request({"op": "admin-list", "proxy": {"user": "pt_alice"}}, user=FE)
    check("error" in out.lower() or out.startswith("6") or "not available" in out, f"front-end cannot use admin operations ({out[:80]})")
    out = raw_request({"op": "list", "proxy": {"user": "pt_alice"}}, user=FE)
    check(out.startswith("6") and "access denied" in out, "front-end without a ticket cannot act for a user")
    check(has_event("fe_denied", reason="no valid ticket"), "…and it is audited")
    out = raw_request({"op": "list", "proxy": {"user": "pt_alice", "ticket": "0" * 32}}, user=FE)
    check(out.startswith("6") and "access denied" in out, "a forged ticket is refused")
    out = raw_request({"op": "fe-gate", "proxy": {"user": "pt_alice"}}, user=FE)
    check(out.startswith("6"), "fe-gate does not hand out a ticket while a second factor is required")
    out = raw_request({"op": "fe-totp", "proxy": {"user": "pt_alice", "code": "000000"}}, user=FE)
    check(out.startswith("6"), "a wrong code through the monitor interface gets no ticket")
    out = raw_request({"op": "fe-keys", "proxy": {"user": "pt_bob"}}, user="pt_alice")
    check(out.startswith("6") and has_event("fe_denied", reason="not the front-end"), "an ordinary user cannot speak the front-end protocol")
    out = raw_request({"op": "list", "proxy": {"user": "pt_bob", "ticket": "0" * 32}}, user="pt_alice")
    check(out.startswith("6"), "an ordinary user cannot claim to be another via proxy")
    out = raw_request({"op": "list", "proxy": {"user": "root", "ticket": "0" * 32}}, user=FE)
    check(out.startswith("6"), "proxy for root is refused")

    print("== front-end failure and restart")
    before = len(find_events("frontend_start"))
    os.kill(fe0, signal.SIGKILL)
    time.sleep(3.5)
    check(len(find_events("frontend_start")) == before + 1 and has_event("frontend_exit"), "monitor restarts a dead front-end")
    check(len(procs_of("pt_alice", "ptscry host")) == 1, "user sessions are not affected by a front-end restart")
    # config change: front-end restarts with new settings; invalid config is refused
    write_conf("frontend_max_conns = 99")
    os.kill(mon.pid, signal.SIGHUP); time.sleep(3.5)
    check(len(find_events("frontend_start")) == before + 2, "reload with changed front-end settings restarts it")
    write_conf("frontend_user = pt_alice")
    os.kill(mon.pid, signal.SIGHUP); time.sleep(1.5)
    check(any("interactive shell" in e.get("error", "") for e in find_events("config_reload_failed")), "a front-end account with a login shell is refused")
    write_conf("frontend_host_key = /nonexistent")
    os.kill(mon.pid, signal.SIGHUP); time.sleep(1.5)
    check(has_event("config_reload_failed") and len(find_events("frontend_start")) == before + 2, "reload with a broken front-end config is refused")
    write_conf()
    os.kill(mon.pid, signal.SIGHUP); time.sleep(3.5)

    print("== flood from one address")
    write_conf("frontend_rate = 30")
    os.kill(mon.pid, signal.SIGHUP); time.sleep(3.5)
    refused = 0
    for _ in range(100):  # the restart after the reload waits out the back-off
        if fe_pid():
            break
        time.sleep(0.2)
    time.sleep(1.0)
    for i in range(30):
        r = subprocess.run([CLI, "run", "-addr", ADDR, "-user", "pt_alice", "-key", f"{KD}/pt_alice", "-expect-auth-fail", "--", "x"],
                           capture_output=True, text=True, timeout=30)
        if "EOF" in r.stdout or "reset" in r.stdout or "refused" in r.stdout: refused += 1
    check(refused >= 5, f"connection rate limit per address kicks in ({refused}/30 dropped)")

    print("== monitor failure closes the front-end too")
    os.kill(mon.pid, signal.SIGKILL); mon.wait()
    time.sleep(1.5)
    check(fe_pid() == [], "front-end exits when the monitor disappears")
    # restart cleanly for the chain check
    for p in procs_of("pt_alice", "ptscry host"): os.kill(p, signal.SIGKILL)
    for p in procs_of("pt_alice", "sleep 300"): os.kill(p, signal.SIGKILL)
    start_monitor()
    time.sleep(1)
    mon.send_signal(signal.SIGTERM); mon.wait(timeout=30)
    r = sh(BIN, "audit-verify", AUDIT, env=ENV)
    check(r.returncode == 0, "audit chain verifies at the end: " + r.stdout.strip()[:80])
    mlog = open(MLOG).read()
    check("DATA RACE" not in mlog and "panic" not in mlog, "no race or panic in the monitor/front-end output")
finally:
    try:
        if mon and mon.poll() is None: mon.kill()
    except Exception:
        pass
    for u in USERS: sh("pkill", "-9", "-u", u)
    sh("pkill", "-9", "-u", FE)
    for u in USERS: sh("userdel", "-r", u)
    sh("userdel", FE)
    sh("groupdel", "ptscry_t")
    print("\nfiles:", ROOT)

print("\nRESULT:", "ALL PASSED" if not fails else f"{len(fails)} FAILED")
sys.exit(1 if fails else 0)
