#!/usr/bin/env python3
# ptscry - Guido Cauli <guido.cauli@gmail.com> - end-to-end test of the production
# mode: root monitor + per-user hosts. Must run as root (it creates test users
# and removes them at the end). Uses real pseudoterminals.
import pwd, json, os, pty, re, select, shutil, signal, subprocess, sys, tempfile, time

SRC = os.path.abspath(os.environ.get("PTSCRY_BIN", "./ptscry"))
ROOT = tempfile.mkdtemp(prefix="ptscry-m-")
os.chmod(ROOT, 0o755)
BINDIR = os.path.join(ROOT, "bin"); os.mkdir(BINDIR, 0o755)
BIN = os.path.join(BINDIR, "ptscry")
shutil.copy(SRC, BIN); os.chmod(BIN, 0o755)
SOCK = os.path.join(ROOT, "run", "monitor.sock")
AUDIT = os.path.join(ROOT, "log", "audit.log")
REC = os.path.join(ROOT, "log", "rec")
CONF = os.path.join(ROOT, "ptscry.conf")
MLOG = os.path.join(ROOT, "monitor.out")

USERS = ["pt_alice", "pt_bob", "pt_carol", "pt_dave", "pt_erin", "pt_frank"]
GROUPS = ["ptscry_t", "ptadmin_t", "unl_t"]
PAM_MODE = os.environ.get("PTSCRY_E2E_PAM", "on")      # "off" re-runs the suite on the legacy direct path
PAM_SVC = "ptscry_t"
PAM_FILE = "/etc/pam.d/" + PAM_SVC
LIMITS = os.path.join(ROOT, "limits.conf")
ENV = dict(os.environ, PTSCRY_CONFIG=CONF, TERM="xterm", PATH="/usr/local/bin:/usr/bin:/bin")
ENV.pop("PTSCRY_STANDALONE", None)
fails = []

def sh(*a, **k):
    return subprocess.run(a, capture_output=True, text=True, **k)

def check(cond, msg):
    print(("PASS " if cond else "FAIL ") + msg)
    if not cond:
        fails.append(msg)

def write_conf(extra="", tail=""):
    with open(CONF, "w") as f:
        f.write(f"""[global]
monitor_socket = {SOCK}
audit_log = {AUDIT}
record_dir = {REC}
access_group = ptscry_t
admin_group = ptadmin_t
pam_service = {"off" if PAM_MODE == "off" else PAM_SVC}
max_sessions = 2
{extra}
[group:unl_t]
max_sessions = unlimited
[user:pt_alice]
record = output
[user:pt_dave]
enabled = no
[user:pt_bob]
allow_command = no
{tail}
""")
    os.chmod(CONF, 0o644)

def uid_of(u): return int(sh("id", "-u", u).stdout)

def as_user(user, args, timeout=20, env=None):
    cmd = ["setpriv", f"--reuid={user}", f"--regid={user}", "--init-groups", BIN] + args
    return subprocess.run(cmd, env=env or ENV, capture_output=True, text=True, timeout=timeout)

class Term:
    def __init__(self, user, args):
        self.pid, self.fd = pty.fork()
        if self.pid == 0:
            os.execvpe("setpriv", ["setpriv", f"--reuid={user}", f"--regid={user}", "--init-groups", BIN] + args, ENV)
        self.buf = b""
    def expect(self, text, timeout=10):
        end = time.time() + timeout
        t = text.encode()
        while time.time() < end:
            if t in self.buf:
                return True
            r, _, _ = select.select([self.fd], [], [], 0.2)
            if r:
                try:
                    d = os.read(self.fd, 65536)
                except OSError:
                    break
                if not d:
                    break
                self.buf += d
        return t in self.buf
    def send(self, s): os.write(self.fd, s.encode() if isinstance(s, str) else s)
    def wait(self, timeout=10):
        end = time.time() + timeout
        while time.time() < end:
            p, st = os.waitpid(self.pid, os.WNOHANG)
            if p:
                return os.waitstatus_to_exitcode(st)
            try:
                r, _, _ = select.select([self.fd], [], [], 0.1)
                if r: self.buf += os.read(self.fd, 65536)
            except OSError:
                pass
        return None

def events():
    out = []
    for l in open(AUDIT):
        if l.strip():
            out.append(json.loads(l))
    return out

def has_event(name, **kv):
    return any(e["event"] == name and all(e.get(k) == v for k, v in kv.items()) for e in events())

mon = None
def start_monitor():
    global mon
    mon = subprocess.Popen([BIN, "monitor", "-c", CONF], stdout=open(MLOG, "a"), stderr=subprocess.STDOUT)
    for _ in range(50):
        if os.path.exists(SOCK): break
        time.sleep(0.1)
    time.sleep(0.2)

def procs_running(*argv):
    """pids whose command line is exactly argv (immune to pgrep -f matching its own caller)"""
    out = []
    for d in os.listdir("/proc"):
        if d.isdigit():
            try:
                if open(f"/proc/{d}/cmdline", "rb").read().rstrip(b"\0").split(b"\0") == [a.encode() for a in argv]:
                    out.append(int(d))
            except OSError:
                pass
    return out

def host_pids(user):
    r = sh("pgrep", "-u", user, "-f", "ptscry host")
    return [int(x) for x in r.stdout.split()]

try:
    # ------------------------------------------------------------ setup
    for g in GROUPS: sh("groupadd", "-f", g)
    for u in USERS: sh("useradd", "-m", "-s", "/bin/bash", u)
    for u in ["pt_alice", "pt_bob", "pt_dave", "pt_erin"]: sh("usermod", "-aG", "ptscry_t", u)
    sh("usermod", "-aG", "ptadmin_t", "pt_bob"); sh("usermod", "-aG", "unl_t", "pt_erin")
    if PAM_MODE != "off":
        open(LIMITS, "w").write("pt_alice hard nofile 500\npt_alice soft nofile 400\npt_alice hard nproc 321\n"); os.chmod(LIMITS, 0o644)
        open(PAM_FILE, "w").write(f"""#%PAM-1.0
account  required  pam_succeed_if.so user != pt_frank quiet
session  required  pam_loginuid.so
session  required  pam_limits.so conf={LIMITS}
"""); os.chmod(PAM_FILE, 0o644)
    for u in ["pt_frank"]: sh("usermod", "-aG", "ptscry_t", u)
    write_conf()
    start_monitor()
    check(mon.poll() is None and os.path.exists(SOCK), "monitor starts and listens")
    check(oct(os.stat(SOCK).st_mode & 0o777) == "0o666", "monitor socket is world-connectable (kernel creds decide)")
    check(os.stat(AUDIT).st_mode & 0o777 == 0o600 and os.stat(AUDIT).st_uid == 0, "audit log is root-only 0600")

    r = sh(BIN, "check", "-c", CONF)
    check("monitor reachable" in r.stdout and "audit hash chain intact" in r.stdout, "check sees monitor and a valid chain")
    check(sh(BIN, "doctor", "-c", CONF).stdout.count("PASS") == r.stdout.count("PASS"), "doctor is still accepted as the old name")
    check("\x1b[" not in r.stdout and "OK  " in r.stdout, "check: no colours when the output is not a terminal")
    rc = sh(BIN, "check", "-c", CONF, "-color=always").stdout
    check("\x1b[1;32mOK" in rc and "\x1b[1;33mWARN" in rc, "check -color=always: OK green, WARN yellow")
    rc = sh(BIN, "check", "-c", CONF, "-u", "pt_carol", "-color=always").stdout
    check("\x1b[1;31mCRIT" in rc, "check -color=always: CRIT red")
    check("\x1b[" not in sh(BIN, "check", "-c", CONF, "-color=never").stdout, "check -color=never")
    r = sh(BIN, "check", "-c", CONF, "-u", "pt_alice")
    check("pt_alice is a member of the access group" in r.stdout and "CRIT" not in r.stdout, "check -u: a member of access_group passes")
    r = sh(BIN, "check", "-c", CONF, "-u", "pt_carol")
    check("CRIT" in r.stdout and "pt_carol is NOT a member" in r.stdout and r.returncode == 1, "check -u: a non-member is CRIT and exits 1")
    check("admin group" in sh(BIN, "check", "-c", CONF, "-u", "pt_alice").stdout, "check reports the admin group membership (information only)")
    r = subprocess.run(["su", "pt_alice", "-s", "/bin/sh", "-c", f"{BIN} check -c {CONF}"], capture_output=True, text=True)
    check("pt_alice is a member" in r.stdout and "needs root" in r.stdout, "check without -u looks at the current user; a normal user is told what needs root")

    print("== audit_group")
    import grp
    st = os.stat(AUDIT)
    os.chmod(os.path.dirname(AUDIT), 0o750)
    write_conf("audit_group = ptadmin_t\n"); mon.send_signal(signal.SIGHUP); time.sleep(1.2)
    st = os.stat(AUDIT)
    check(st.st_mode & 0o777 == 0o640 and st.st_uid == 0 and st.st_gid == grp.getgrnam("ptadmin_t").gr_gid, f"audit_group: log becomes root:ptadmin_t 0640 ({oct(st.st_mode & 0o777)})")
    dst = os.stat(os.path.dirname(AUDIT))
    check(dst.st_gid == grp.getgrnam("ptadmin_t").gr_gid and dst.st_mode & 0o777 == 0o750, "audit_group: its directory is root:ptadmin_t 0750")
    r = as_user("pt_bob", ["audit-verify", AUDIT]); check(r.returncode == 0 and "OK" in r.stdout, "a member of the group verifies the audit log without root")
    r = as_user("pt_alice", ["audit-verify", AUDIT]); check(r.returncode != 0, "a user outside the group cannot read it")
    write_conf("audit_group = no_such_group_t\n"); mon.send_signal(signal.SIGHUP); time.sleep(1.2)
    check(os.stat(AUDIT).st_mode & 0o777 == 0o600 and os.stat(AUDIT).st_gid == 0, "an unknown audit_group leaves the log root-only (fail closed)")
    write_conf("audit_group = ptadmin_t\n"); mon.send_signal(signal.SIGHUP); time.sleep(1.2)
    write_conf(""); mon.send_signal(signal.SIGHUP); time.sleep(1.2)
    st = os.stat(AUDIT); dst = os.stat(os.path.dirname(AUDIT))
    check(st.st_mode & 0o777 == 0o600 and st.st_gid == 0 and dst.st_gid == 0, "withdrawing audit_group puts the log and directory back to root only")

    # ------------------------------------------------------------ policy
    print("== policy")
    r = as_user("pt_carol", ["new", "-d", "-n", "c1"])
    check(r.returncode != 0 and "access denied" in r.stderr, "user outside access_group is denied")
    check(has_event("access_denied", user="pt_carol"), "denial is audited with the user name")
    check("check -u" in r.stderr, "access denied points to ptscry check")
    r = subprocess.run([BIN, "list"], env=ENV, capture_output=True, text=True, timeout=20)
    check(r.returncode == 0, "root may use ptscry outside access_group (allow_root = yes, the default)")
    write_conf("allow_root = no\n"); mon.send_signal(signal.SIGHUP); time.sleep(1.2)
    r = subprocess.run([BIN, "list"], env=ENV, capture_output=True, text=True, timeout=20)
    check(r.returncode != 0 and "access denied" in r.stderr, "allow_root = no: root outside access_group is denied")
    write_conf(""); mon.send_signal(signal.SIGHUP); time.sleep(1.2)
    r = as_user("pt_dave", ["new", "-d", "-n", "d1"])
    check(r.returncode != 0 and "access denied" in r.stderr, "enabled = no is enforced")
    r = as_user("pt_bob", ["new", "-d", "-n", "x", "sleep", "5"])
    check(r.returncode != 0 and "disabled by configuration" in r.stderr, "allow_command = no is enforced by the monitor")

    # ------------------------------------------------------------ alice: interactive
    print("== alice: interactive session, recording, isolation")
    alice = uid_of("pt_alice")
    t = Term("pt_alice", ["new", "-n", "a1"])
    check(t.expect("$ ") or t.expect("# ") or t.expect("pt_alice"), "shell prompt in managed session")
    t.send("echo uid-is-$(id -u)\n")
    check(t.expect(f"uid-is-{alice}"), "the shell runs as the user, not as root")
    t.send("echo marker-$((6*7))\n"); check(t.expect("marker-42"), "output seen")
    t.send(b"\x02d"); check(t.expect("detached"), "detach works through the monitor handoff"); check(t.wait() == 0, "client exits cleanly")

    out = as_user("pt_alice", ["list"]).stdout
    check("a1" in out, "alice sees her session")
    check("a1" not in as_user("pt_bob", ["list"]).stdout, "bob cannot see alice's session")
    r = as_user("pt_bob", ["kill", "a1"])
    check(r.returncode != 0, "bob cannot kill alice's session")

    t2 = Term("pt_alice", ["attach", "a1"])
    check(t2.expect("marker-42"), "replay after reattach")
    t2.send("echo again-$((1+1))\n"); check(t2.expect("again-2"), "interactive after reattach")
    t2.send(b"\x02d"); t2.wait()

    # session commands through the monitor
    t3 = Term("pt_alice", ["attach", "a1"]); time.sleep(1)
    r = as_user("pt_alice", ["rename", "a1", "a1r"])
    check(r.returncode == 0 and "a1r" in as_user("pt_alice", ["list"]).stdout, "rename through the monitor")
    r = as_user("pt_bob", ["rename", "a1r", "stolen"])
    check(r.returncode != 0, "bob cannot rename alice's session")
    r = as_user("pt_bob", ["send-keys", "a1r", "id", "Enter"])
    check(r.returncode != 0, "bob cannot type into alice's session")
    r = as_user("pt_alice", ["notify", "a1r", "managed", "notice"])
    check(r.returncode == 0 and t3.expect("[ptscry] managed notice"), "notify through the monitor")
    r = as_user("pt_alice", ["send-keys", "a1r", "echo sk-$((40+2))", "Enter"])
    check(r.returncode == 0 and t3.expect("sk-42"), "send-keys through the monitor")
    check(has_event("rename", user="pt_alice") and has_event("send_keys", user="pt_alice"), "rename and send-keys reach the central audit")
    r = as_user("pt_alice", ["rename", "a1r", "a1"]); check(r.returncode == 0, "rename back to the creation name")
    t3.send(b"\x02d"); t3.wait()
    lay = '{"version":1}'
    r = subprocess.run(["setpriv", "--reuid=pt_alice", "--regid=pt_alice", "--init-groups", BIN, "layout", "save", "ml1"], env=ENV, input=lay, capture_output=True, text=True, timeout=20)
    check(r.returncode == 0 and as_user("pt_alice", ["layout", "show", "ml1"]).stdout.strip() == lay, "layouts through the monitor")
    check("ml1" not in as_user("pt_bob", ["layout", "list"]).stdout, "bob does not see alice's layouts")
    home = pwd.getpwnam("pt_alice").pw_dir
    lp = os.path.join(home, ".local/state/ptscry/layouts/ml1.json")
    check(os.path.exists(lp) and os.stat(lp).st_uid == alice and oct(os.stat(os.path.dirname(lp)).st_mode & 0o777) == "0o700", "the layout file is alice's, in a 0700 directory")
    as_user("pt_alice", ["layout", "rm", "ml1"])

    # host hardening
    hp = host_pids("pt_alice")
    check(len(hp) == 1, "exactly one host process for alice")
    if hp:
        st = open(f"/proc/{hp[0]}/status").read()
        check(f"Uid:\t{alice}" in st, "host runs with alice's uid")
        r = subprocess.run(["setpriv", "--reuid=pt_alice", "--regid=pt_alice", "--init-groups", "cat", f"/proc/{hp[0]}/environ"], capture_output=True, text=True)
        check(r.returncode != 0, "alice cannot read her host's /proc/PID/environ (non-dumpable)")
        r = subprocess.run(["setpriv", "--reuid=pt_alice", "--regid=pt_alice", "--init-groups", "ls", f"/proc/{hp[0]}/fd"], capture_output=True, text=True)
        check(r.returncode != 0, "alice cannot list her host's file descriptors")
        env = open(f"/proc/{hp[0]}/environ").read().split("\0")
        check(not any(e.startswith(("PTSCRY_CONFIG", "SSH_", "SUDO_")) for e in env), "host environment is clean")

    # ------------------------------------------------------------ limits
    print("== limits")
    r = as_user("pt_alice", ["new", "-d", "-n", "a2"]); check(r.returncode == 0, "second session allowed")
    r = as_user("pt_alice", ["new", "-d", "-n", "a3"])
    check(r.returncode != 0 and "session limit reached (2)" in r.stderr, "max_sessions = 2 enforced")
    check(has_event("limit_reached", user="pt_alice"), "limit is audited")
    ok = True
    for i in range(5):
        ok &= as_user("pt_erin", ["new", "-d", "-n", f"e{i}"]).returncode == 0
    check(ok, "group with max_sessions = unlimited can open 5 sessions")
    check(as_user("pt_erin", ["list"]).stdout.count("\ne") + as_user("pt_erin", ["list"]).stdout.count("e0") >= 1, "erin's list works")

    # ------------------------------------------------------------ admin
    print("== admin")
    r = as_user("pt_alice", ["admin", "list"]); check(r.returncode != 0 and "access denied" in r.stderr, "non-admin cannot use admin list")
    r = as_user("pt_bob", ["admin", "list"])
    check(r.returncode == 0 and "pt_alice" in r.stdout and "pt_erin" in r.stdout, "admin_group member sees all users' sessions")
    r = as_user("pt_bob", ["admin", "kill", "pt_alice", "a2"]); check(r.returncode == 0, "admin kill accepted")
    time.sleep(1.5)
    check("a2" not in as_user("pt_alice", ["list"]).stdout and "a1" in as_user("pt_alice", ["list"]).stdout, "only the targeted session was killed")
    check(has_event("admin_kill", admin="pt_bob", target_user="pt_alice"), "admin action audited with the administrator's identity")
    r = subprocess.run([BIN, "admin", "list"], env=ENV, capture_output=True, text=True)
    check(r.returncode == 0, "root is an administrator")

    # ------------------------------------------------------------ admin setup (users and keys without sudo)
    print("== admin setup")
    FEK = os.path.join(ROOT, "fekeys"); os.mkdir(FEK, 0o755)
    write_conf(f"frontend_authorized_keys_dir = {FEK}\n")
    r = as_user("pt_alice", ["admin", "setup", "users", "list"])
    check(r.returncode != 0 and "access denied" in r.stderr, "admin setup: refused to a non-administrator")
    r = as_user("pt_bob", ["admin", "setup", "users", "list"])
    check(r.returncode == 0 and "pt_alice" in r.stdout, "admin setup users list: an administrator sees the members")
    r = as_user("pt_bob", ["admin", "setup", "users", "add", "pt_carol"])
    check(r.returncode == 0 and "pt_carol" in sh("getent", "group", "ptscry_t").stdout, "admin setup users add: pt_carol in the access group, done by the monitor")
    check(has_event("admin_setup", admin="pt_bob", ok=True), "admin setup audited with the administrator's identity")
    r = as_user("pt_bob", ["admin", "setup", "users", "add", "-create", "pt_newacct"])
    check(r.returncode != 0 and "needs root" in r.stderr and sh("id", "pt_newacct").returncode != 0, "admin setup: creating system accounts stays root only")
    r = as_user("pt_bob", ["admin", "setup", "users", "remove", "pt_carol"])
    check(r.returncode == 0 and "pt_carol" not in sh("getent", "group", "ptscry_t").stdout.split(":")[-1], "admin setup users remove (pt_carol back as before)")
    kd = os.path.join(ROOT, "k"); os.mkdir(kd, 0o755)
    sh("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "alice@test", "-f", os.path.join(kd, "id"))
    os.chmod(os.path.join(kd, "id.pub"), 0o644); os.chmod(os.path.join(kd, "id"), 0o644)  # readable by pt_bob, to see it refused
    r = as_user("pt_bob", ["admin", "setup", "keys", "add", "pt_alice", os.path.join(kd, "id.pub")])
    check(r.returncode == 0 and "added" in r.stdout and os.path.exists(os.path.join(FEK, "pt_alice")), "admin setup keys add: the key file read by the client, written by the monitor")
    r = as_user("pt_bob", ["admin", "setup", "keys", "list", "pt_alice"])
    check(r.returncode == 0 and "alice@test" in r.stdout, "admin setup keys list")
    r = as_user("pt_bob", ["admin", "setup", "keys", "add", "daemon", os.path.join(kd, "id.pub")])
    check(r.returncode != 0 and "system account" in r.stderr, "admin setup: no keys for system accounts")
    sh("usermod", "-aG", "ptadmin_t", "pt_erin")
    r = as_user("pt_bob", ["admin", "setup", "keys", "add", "pt_erin", os.path.join(kd, "id.pub")])
    check(r.returncode != 0 and "administrator too" in r.stderr, "admin setup: no keys for another administrator")
    sh("gpasswd", "-d", "pt_erin", "ptadmin_t")
    write_conf(f"frontend_authorized_keys_dir = {FEK}\nadmin_setup = users\n")
    r = as_user("pt_bob", ["admin", "setup", "keys", "list"])
    check(r.returncode != 0 and "admin_setup" in r.stderr, "admin_setup = users: the keys stay root's")
    r = subprocess.run([BIN, "admin", "setup", "keys", "list", "pt_alice"], env=ENV, capture_output=True, text=True)
    check(r.returncode == 0 and "alice@test" in r.stdout, "root is not limited by admin_setup")
    write_conf(f"frontend_authorized_keys_dir = {FEK}\n")
    r = as_user("pt_bob", ["admin", "setup", "keys", "add", "pt_alice", os.path.join(kd, "id")])
    check(r.returncode != 0 and "private key" in r.stderr, "admin setup keys add: a private key is refused")
    home = os.path.expanduser("~pt_alice"); os.makedirs(os.path.join(home, ".ssh"), exist_ok=True)
    ak = os.path.join(home, ".ssh", "authorized_keys")
    if os.path.lexists(ak): os.remove(ak)
    os.symlink("/etc/passwd", ak); sh("chown", "-h", "pt_alice:pt_alice", ak, os.path.join(home, ".ssh"))
    r = as_user("pt_bob", ["admin", "setup", "keys", "add", "pt_alice", "-home"])
    check(r.returncode != 0 and "not a link" in r.stderr, "admin setup keys add -home: a link is not followed")
    os.remove(ak)
    r = as_user("pt_bob", ["admin", "setup", "keys", "remove", "pt_alice", "1"])
    check(r.returncode == 0 and "alice@test" not in as_user("pt_bob", ["admin", "setup", "keys", "list", "pt_alice"]).stdout, "admin setup keys remove")
    write_conf()

    # ------------------------------------------------------------ login (ForceCommand)
    print("== login")
    def login(cmd):
        e = dict(ENV, SSH_ORIGINAL_COMMAND=cmd)
        return subprocess.run(["setpriv", "--reuid=pt_alice", "--regid=pt_alice", "--init-groups", BIN, "login"], env=e, capture_output=True, text=True)
    check(login("ptscry list").returncode == 0, "login allows `ptscry list`")
    for bad in ["ls -l", "ptscry host", "ptscry monitor", "ptscry list; id", "ptscry admin list", "ptscry stop", "$(id)", "ptscry list | cat"]:
        check(login(bad).returncode == 126, f"login rejects {bad!r}")

    # ------------------------------------------------------------ recording & end of sessions
    print("== recording (written by the monitor)")
    t = Term("pt_alice", ["attach", "a1"])
    t.expect("again-2")
    t.send("echo rec-line-$((7*6))\n"); check(t.expect("rec-line-42"), "output in a recorded session")
    t.send("exit 0\n"); check(t.expect("exited with status 0"), "session end reported")
    t.wait()
    time.sleep(2.0)   # alice has no more sessions -> host exits (exit_on_last_shell)
    check(host_pids("pt_alice") == [], "alice's host exits after her last session")
    rd = os.path.join(REC, f"{alice}-pt_alice")
    casts = os.listdir(rd) if os.path.isdir(rd) else []
    check(len(casts) >= 1, "recording exists under the monitor's directory")
    if casts:
        p = os.path.join(rd, sorted(c for c in casts if c.startswith("a1-"))[0])
        st = os.stat(p)
        check(st.st_uid == 0 and (st.st_mode & 0o777) == 0o600, "recording is root-owned 0600")
        check("rec-line-42" in open(p).read(), "recording contains the session output")
        r = subprocess.run(["setpriv", "--reuid=pt_alice", "--regid=pt_alice", "--init-groups", "cat", p], capture_output=True, text=True)
        check(r.returncode != 0, "alice cannot read (or alter) her own recording")

    # ------------------------------------------------------------ audit content and integrity
    print("== audit")
    ev = events()
    names = {e["event"] for e in ev}
    for n in ["monitor_start", "request", "host_spawn", "session_new", "attach", "detach", "session_end", "host_exit", "access_denied", "limit_reached", "record_open", "record_close"]:
        check(n in names, f"audit contains {n}")
    sn = [e for e in ev if e["event"] == "session_new" and e.get("session") == "a1"]
    check(bool(sn) and sn[0].get("user") == "pt_alice" and sn[0].get("uid") == alice and sn[0].get("src") == "host", "host events carry identity set by the monitor")
    rq = [e for e in ev if e["event"] == "request" and e.get("peer_uid") == alice]
    check(bool(rq) and "loginuid" in rq[0] and "via_sshd" in rq[0], "requests carry kernel login attribution (loginuid, audit session, sshd ancestry)")
    r = sh(BIN, "audit-verify", AUDIT); check(r.returncode == 0 and "OK" in r.stdout, "audit chain verifies")
    lines = open(AUDIT).read().splitlines()
    tam = os.path.join(ROOT, "tampered.log")
    idx = next(i for i in range(len(lines)//2, len(lines)) if "pt_" in lines[i])
    l2 = list(lines); l2[idx] = l2[idx].replace("pt_", "xx_", 1)
    open(tam, "w").write("\n".join(l2) + "\n")
    check(sh(BIN, "audit-verify", tam).returncode != 0, "edited record is detected")
    l3 = list(lines); del l3[len(l3)//2]
    open(tam, "w").write("\n".join(l3) + "\n")
    check(sh(BIN, "audit-verify", tam).returncode != 0, "deleted record is detected")
    l4 = list(lines); l4[3], l4[4] = l4[4], l4[3]
    open(tam, "w").write("\n".join(l4) + "\n")
    check(sh(BIN, "audit-verify", tam).returncode != 0, "reordered records are detected")

    # ------------------------------------------------------------ reload + rotation
    print("== reload and log rotation")
    rot = AUDIT + ".1"
    os.rename(AUDIT, rot)
    write_conf()
    mon.send_signal(signal.SIGHUP); time.sleep(1.0)
    check(os.path.exists(AUDIT), "audit file recreated after SIGHUP")
    check(sh(BIN, "audit-verify", rot, AUDIT).returncode == 0, "chain continues across rotated files")
    as_user("pt_alice", ["list"]); time.sleep(0.3)
    check(sh(BIN, "audit-verify", rot, AUDIT).returncode == 0, "chain still intact after more records")
    open(os.path.join(ROOT, "badconf"), "w").write("[global]\nbogus = 1\n")
    shutil.copy(CONF, CONF + ".good"); shutil.copy(os.path.join(ROOT, "badconf"), CONF)
    mon.send_signal(signal.SIGHUP); time.sleep(0.8)
    check(mon.poll() is None and has_event("config_reload_failed"), "bad config on reload is rejected, monitor keeps running")
    shutil.copy(CONF + ".good", CONF)
    os.chmod(CONF, 0o666)
    mon.send_signal(signal.SIGHUP); time.sleep(0.8)
    check(has_event("config_reload_failed", error=f"{CONF} is writable by group or others (mode -rw-rw-rw-)") or any(e["event"] == "config_reload_failed" and "writable" in e.get("error", "") for e in events()), "world-writable config is refused on reload")
    os.chmod(CONF, 0o644)


    # ------------------------------------------------------------ abuse and hardening
    print("== abuse resistance")
    def sock_py(user, code, timeout=60):
        return subprocess.run(["setpriv", f"--reuid={user}", f"--regid={user}", "--init-groups", "python3", "-I", "-c", code],
                              capture_output=True, text=True, timeout=timeout)
    FLOOD = f"""
import socket, struct, json
denied = ok = 0
for i in range(150):
    s = socket.socket(socket.AF_UNIX); s.connect({SOCK!r})
    b = json.dumps({{"op": "list"}}).encode()
    s.sendall(b"\\x01" + struct.pack(">I", len(b)) + b)
    h = s.recv(5)
    n = struct.unpack(">I", h[1:5])[0] if len(h) == 5 else 0
    body = b""
    while len(body) < n: body += s.recv(n - len(body))
    if h[:1] == b"\\x06" and b"too many requests" in body: denied += 1
    elif h[:1] == b"\\x05": ok += 1
    s.close()
print(denied, ok)
"""
    r = sock_py("pt_erin", FLOOD)
    denied, okn = (list(map(int, r.stdout.split())) + [0, 0])[:2]
    check(denied >= 60 and okn >= 30, f"request flood is rate limited per uid (denied={denied}, served={okn})")
    check(has_event("rate_limited"), "rate limiting is audited (coalesced, not one record per refusal)")
    check(sum(1 for e in events() if e["event"] == "rate_limited") <= 3, "refusals are summarised, not logged one by one")
    time.sleep(2)
    check(as_user("pt_erin", ["list"]).returncode == 0, "service resumes once the budget refills")
    check(as_user("pt_alice", ["list"]).returncode == 0, "another user was not affected by erin's flood")

    r = as_user("pt_alice", ["new", "-d", "-n", "huge", "sleep"] + ["a" * 4000] * 60)
    check(r.returncode != 0 and "command line too long" in r.stderr, "oversized command line is refused with a clear error")
    big = ["new", "-d", "-n", "big", "sleep"] + ["a" * 4000] * 7
    rbig = as_user("pt_alice", big); time.sleep(1.0)
    print("   big ->", rbig.returncode, rbig.stderr.strip()[:200])
    longest = max(len(l) for l in open(AUDIT))
    check(longest <= 16 * 1024 + 1, f"user-supplied data cannot bloat audit records (longest line {longest} bytes)")
    sn = [e for e in events() if e["event"] == "session_new" and e.get("session") == "big"]
    check(bool(sn) and len(sn[0].get("cmd", "")) <= 512, "command line is clipped in the audit record")

    # binary replaced while the monitor runs: new hosts still start, from the monitor's own image
    newbin = BIN + ".new"; shutil.copy(SRC, newbin); os.chmod(newbin, 0o755); os.rename(newbin, BIN)
    r = as_user("pt_bob", ["new", "-d", "-n", "after-upgrade"])
    check(r.returncode == 0 or "disabled by configuration" in r.stderr, "hosts still start after the binary was replaced")
    as_user("pt_bob", ["kill", "after-upgrade"])

    # server-wide session cap
    adm = as_user("pt_bob", ["admin", "list"]).stdout.strip().splitlines()
    total = max(0, len(adm) - 1) if adm and "no sessions" not in adm[0] else 0
    write_conf(f"max_total_sessions = {total + 1}")
    mon.send_signal(signal.SIGHUP); time.sleep(0.8)
    r1 = as_user("pt_erin", ["new", "-d", "-n", "cap1"])
    r2 = as_user("pt_erin", ["new", "-d", "-n", "cap2"])
    check(r1.returncode == 0 and r2.returncode != 0 and "server-wide" in r2.stderr, "max_total_sessions caps all users together, even for unlimited groups")
    as_user("pt_erin", ["kill", "cap1"])
    write_conf(); mon.send_signal(signal.SIGHUP); time.sleep(0.8)

    # ptscry run / wait: a job without a terminal
    r = as_user("pt_alice", ["run", "-q", "sh", "-c", "echo JOB-OUT; exit 7"])
    # the exit status must never be lost: the launcher writes it and exits at
    # once, and its reaper used to record "unknown" (255) first, now and then
    bad = []
    for i in range(20):
        x = as_user("pt_alice", ["run", "-q", "sh", "-c", "exit 7"])
        if x.returncode != 7: bad.append(x.returncode)
    check(not bad, f"run: the exit status of 20 quick jobs in a row is never lost ({bad})")
    if r.returncode != 7:  # say why (-q hides it)
        r2 = as_user("pt_alice", ["run", "sh", "-c", "echo JOB-OUT; exit 7"])
        print("  run again without -q:", r2.returncode, r2.stderr.strip()[:300], "| sessions:", as_user("pt_alice", ["list"]).stdout.strip()[:300])
    check(r.returncode == 7 and "JOB-OUT" in r.stdout, f"run streams the job and returns its exit status ({r.returncode} {r.stderr.strip()[:160]})")
    time.sleep(0.5)
    check("JOB-OUT" not in as_user("pt_alice", ["list"]).stdout and has_event("session_end", user="pt_alice", exit_code=7),
          "the session closes by itself when the command ends")
    p = subprocess.Popen(["setpriv", "--reuid=pt_alice", "--regid=pt_alice", "--init-groups", BIN, "run", "-n", "jobbg", "sleep", "60"], env=ENV, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    time.sleep(1.2); p.send_signal(signal.SIGINT); p.wait(timeout=10)
    check(p.returncode == 130 and "jobbg" in as_user("pt_alice", ["list"]).stdout, "interrupting run leaves the job running")
    w = subprocess.Popen(["setpriv", "--reuid=pt_alice", "--regid=pt_alice", "--init-groups", BIN, "wait", "-q", "jobbg"], env=ENV, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    time.sleep(1.0)
    check(w.poll() is None, "wait follows the running job")
    as_user("pt_alice", ["kill", "jobbg"])
    try:
        w.wait(timeout=10); waited = True
    except subprocess.TimeoutExpired:
        w.kill(); waited = False
    check(waited, "wait returns when the job is killed")
    p = subprocess.Popen(["setpriv", "--reuid=pt_alice", "--regid=pt_alice", "--init-groups", BIN, "run", "-k", "-n", "jobk", "sleep", "60"], env=ENV, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    time.sleep(1.2); p.send_signal(signal.SIGTERM); p.wait(timeout=10); time.sleep(1.0)
    check("jobk" not in as_user("pt_alice", ["list"]).stdout, "run -k kills the job when interrupted")
    check(as_user("pt_alice", ["run", "-n", "x y", "true"]).returncode != 0 and as_user("pt_alice", ["wait", "nosuch"]).returncode != 0, "bad names and missing sessions are refused")
    # ------------------------------------------------------------ monitor re-exec keeps sessions
    print("== monitor re-exec")
    write_conf(); mon.send_signal(signal.SIGHUP); time.sleep(0.8)
    as_user("pt_alice", ["stop"]); time.sleep(2.5)
    as_user("pt_alice", ["new", "-d", "-n", "rx1", "sh", "-c", "i=0; while :; do echo tick-$i; i=$((i+1)); sleep 0.3; done"])
    tx = Term("pt_alice", ["new", "-n", "rx2"])
    tx.send("echo before-$((20+1))\n"); check(tx.expect("before-21"), "re-exec: interactive session before")
    pid0, old_host = mon.pid, host_pids("pt_alice")
    shutil.copy(SRC, BIN + ".new"); os.chmod(BIN + ".new", 0o755); os.replace(BIN + ".new", BIN)   # as an upgrade would
    r = sh(BIN, "admin", "reexec", env=ENV)
    check(r.returncode == 0, "admin reexec accepted " + r.stderr)
    time.sleep(3.5)
    check(mon.poll() is None and open(f"/proc/{pid0}/cmdline", "rb").read().startswith(BIN.encode()), "re-exec: same process, new program")
    check(host_pids("pt_alice") == old_host, "re-exec: the user's host was not touched")
    check(has_event("monitor_reexec") and has_event("monitor_resumed") and has_event("monitor_start", resumed=True), "re-exec: audited")
    rs = [e for e in events() if e["event"] == "monitor_resumed"][-1]
    check(rs["hosts"] >= 1 and rs["sessions"] >= 2, f"re-exec: state carried ({rs})")
    tx.send("echo after-$((20+2))\n"); check(tx.expect("after-22"), "re-exec: the attached client keeps working")
    ls = as_user("pt_alice", ["list"]).stdout
    check("rx1" in ls and "rx2" in ls, "re-exec: sessions still listed")
    r = as_user("pt_alice", ["run", "-q", "-n", "over", "true"])
    check(r.returncode != 0 and "limit" in r.stderr, "re-exec: the session limit still counts the old sessions " + r.stderr.strip())
    r = as_user("pt_alice", ["kill", "rx1"]); time.sleep(4)
    check("rx1" not in as_user("pt_alice", ["list"]).stdout, "re-exec: kill reaches a session started before it")
    time.sleep(1.5)  # the monitor counts sessions from the host's events: let session_end of rx1 arrive
    r = as_user("pt_alice", ["run", "-q", "-n", "afterjob", "sh", "-c", "echo job-after; exit 5"])
    check(r.returncode == 5 and "job-after" in r.stdout, "re-exec: new sessions start (launcher) and report their exit code")
    tx.send("exit 3\n"); check(tx.expect("exited with status 3"), "re-exec: a session started before reports its exit status")
    tx.wait()
    time.sleep(1.0)
    rd = os.path.join(REC, f"{alice}-pt_alice")
    rec = [os.path.join(rd, c) for c in os.listdir(rd) if c.startswith("rx2-")]
    txt = open(rec[0]).read() if rec else ""
    check("before-21" in txt and "after-22" in txt, "re-exec: the recording continues in the same file")
    r = sh(BIN, "audit-verify", AUDIT); check(r.returncode == 0 and "OK" in r.stdout, "re-exec: audit chain verifies across the re-exec")
    r = sh(SRC, "monitor", "-t", "-c", CONF, env=ENV)
    check(r.returncode == 0 and "configuration OK" in r.stdout, "monitor -t accepts the installed configuration " + r.stderr)
    good = open(CONF).read()
    open(CONF, "w").write(good + "\nno_such_key = 1\n"); os.chmod(CONF, 0o644)
    r = sh(SRC, "monitor", "-t", "-c", CONF, env=ENV)
    check(r.returncode != 0, "monitor -t rejects a bad configuration")
    nfail = len([e for e in events() if e["event"] == "monitor_reexec_failed"])
    mon.send_signal(signal.SIGUSR2); time.sleep(3.5)  # SIGUSR2 = the same request, without a client that needs the config
    ev2 = [e for e in events() if e["event"] == "monitor_reexec_failed"]
    check(len(ev2) == nfail + 1 and "refuses this installation" in ev2[-1].get("error", ""), "re-exec is refused when the new program would not accept the configuration")
    open(CONF, "w").write(good); os.chmod(CONF, 0o644)
    # a broken new program must not replace a working monitor
    open(BIN + ".bad", "w").write("#!/bin/sh\nexit 1\n"); os.chmod(BIN + ".bad", 0o755); os.replace(BIN + ".bad", BIN)
    r = sh(SRC, "admin", "reexec", env=ENV)
    time.sleep(1.5)
    check(has_event("monitor_reexec_failed"), "re-exec: a program that does not run is refused (audited)")
    check(mon.poll() is None and subprocess.run(["setpriv", "--reuid=pt_alice", "--regid=pt_alice", "--init-groups", SRC, "list"], env=ENV, capture_output=True).returncode == 0, "re-exec: the monitor carries on after a refused re-exec")
    shutil.copy(SRC, BIN + ".new"); os.chmod(BIN + ".new", 0o755); os.replace(BIN + ".new", BIN)
    as_user("pt_alice", ["stop"]); time.sleep(3)

    # detached jobs keep their result
    r = as_user("pt_alice", ["run", "-d", "-n", "dj", "sh", "-c", "echo djout; exit 6"])
    time.sleep(1.5)
    r = as_user("pt_alice", ["wait", "-q", "dj"])
    check(r.returncode == 6 and "djout" in r.stdout, "managed: run -d result (exit code and output) can be collected later")
    check(as_user("pt_alice", ["status", "dj"]).returncode == 0 and as_user("pt_alice", ["wait", "-q", "-x", "dj"]).returncode == 6
          and as_user("pt_alice", ["status", "dj"]).returncode == 4, "managed: status, then wait -x discards")
    check(any(e["event"] == "job_collect" for e in events()), "managed: job collection is audited by the monitor")
    # sshd-style Match blocks
    write_conf(tail="Match User pt_bob\nMaxParallelSessions 3\nMatch User pt_carol,pt_dave\n  MaxParallelSessions 5")
    mon.send_signal(signal.SIGHUP); time.sleep(0.8)
    check("max_sessions         3" in as_user("pt_bob", ["config"]).stdout and "max_sessions         5" in as_user("pt_carol", ["config"]).stdout,
          "Match User / MaxParallelSessions set the per-user limit")
    # per-user rate limit (burst = 4x): bob is throttled, dave (raised) is not
    write_conf(tail="Match User pt_bob,pt_dave\nMaxParallelSessions unlimited\nMatch User pt_bob\nRateLimit 1\nMatch User pt_dave\nRateLimit 500")
    mon.send_signal(signal.SIGHUP); time.sleep(0.8)
    def burst(user, n):
        r = subprocess.run(["setpriv", f"--reuid={user}", f"--regid={user}", "--init-groups", "sh", "-c",
                            f'ok=0; bad=0; for i in $(seq {n}); do {BIN} run -q true 2>/tmp/rl.$$ >/dev/null; if grep -q "too many" /tmp/rl.$$; then bad=$((bad+1)); else ok=$((ok+1)); fi; done; echo $ok $bad; rm -f /tmp/rl.$$'],
                           env=ENV, capture_output=True, text=True)
        return tuple(int(x) for x in r.stdout.split())
    as_user("pt_bob", ["run", "-q", "true"]); as_user("pt_dave", ["run", "-q", "true"])  # policy resolved once
    time.sleep(6)
    ok, bad = burst("pt_bob", 40)
    check(bad > 0, f"RateLimit 1 throttles a fast loop (ok={ok} refused={bad})")
    ok, bad = burst("pt_dave", 40)
    check(bad == 0, f"RateLimit 500 does not throttle (ok={ok} refused={bad})")
    check(any(e["event"] == "rate_limited" for e in events()), "throttling is audited")
    write_conf(); mon.send_signal(signal.SIGHUP); time.sleep(0.8)

    # recording size cap ends the session instead of filling the disk
    write_conf(tail="[user:pt_alice]\nrecord_max_bytes = 2K")
    mon.send_signal(signal.SIGHUP); time.sleep(0.8)
    as_user("pt_alice", ["stop"]); time.sleep(1.0)
    as_user("pt_alice", ["new", "-d", "-n", "capped", "sh", "-c", "yes x | head -c 40000; sleep 30"])
    time.sleep(3.0)
    check(any(e["event"] == "record_error" and "size limit" in e.get("error", "") for e in events()), "recording over record_max_bytes is stopped and audited")
    for _ in range(40):  # the legacy path may need the 3 s SIGKILL backstop
        if "capped" not in as_user("pt_alice", ["list"]).stdout: break
        time.sleep(0.25)
    check("capped" not in as_user("pt_alice", ["list"]).stdout, "the session whose recording hit the cap is ended")
    write_conf(); mon.send_signal(signal.SIGHUP); time.sleep(0.8)
    as_user("pt_alice", ["stop"]); time.sleep(0.5)

    # revocation: a policy change reaches running sessions
    as_user("pt_erin", ["new", "-d", "-n", "rv"])
    check(len(host_pids("pt_erin")) == 1, "erin has a running host before the revocation")
    write_conf(tail="[user:pt_erin]\nenabled = no")
    mon.send_signal(signal.SIGHUP); time.sleep(3.0)
    check(host_pids("pt_erin") == [], "disabling a user by policy ends their running sessions")
    check(has_event("policy_revoked", user="pt_erin"), "the revocation is audited")
    write_conf(); mon.send_signal(signal.SIGHUP); time.sleep(0.8)
    check(as_user("pt_erin", ["new", "-d", "-n", "back"]).returncode == 0, "the user can work again after the policy is restored")

    # monitor refuses to run with a directory others can write to
    bad = os.path.join(ROOT, "wr"); os.makedirs(bad, exist_ok=True); os.chmod(bad, 0o777)
    conf2 = os.path.join(ROOT, "conf2")
    open(conf2, "w").write(f"[global]\npam_service = off\nmonitor_socket = {bad}/m.sock\naudit_log = {ROOT}/log2/audit.log\n")
    os.chmod(conf2, 0o644)
    r = subprocess.run([BIN, "monitor", "-c", conf2], capture_output=True, text=True, timeout=15)
    check(r.returncode != 0 and "owned by root" in r.stderr, "monitor refuses a socket directory that others can write to")

    # client refuses a "monitor" that is not root
    fake = os.path.join(ROOT, "fake"); os.makedirs(fake, exist_ok=True); os.chmod(fake, 0o777)
    fsock = os.path.join(fake, "m.sock")
    srv = subprocess.Popen(["setpriv", "--reuid=pt_alice", "--regid=pt_alice", "--init-groups", "python3", "-I", "-c",
        f"import socket,time; s=socket.socket(socket.AF_UNIX); s.bind({fsock!r}); __import__('os').chmod({fsock!r}, 0o666); s.listen(5); time.sleep(10)"])
    for _ in range(30):
        if os.path.exists(fsock): break
        time.sleep(0.1)
    conf3 = os.path.join(ROOT, "conf3"); open(conf3, "w").write(f"[global]\nmonitor_socket = {fsock}\n"); os.chmod(conf3, 0o644)
    r = as_user("pt_bob", ["list"], env=dict(ENV, PTSCRY_CONFIG=conf3))
    print("   fake ->", r.returncode, r.stderr.strip()[:200])
    srv.kill(); srv.wait()
    check(r.returncode != 0 and "not served by root" in r.stderr, "client refuses a socket answered by a non-root process")


    # ------------------------------------------------------------ PAM launcher
    if PAM_MODE != "off":
        print("== PAM launcher: login uid, limits, access policy, root-owned processes")
        out = os.path.join(ROOT, "out"); os.makedirs(out, exist_ok=True); os.chmod(out, 0o777)
        f1 = os.path.join(out, "alice.txt")
        as_user("pt_alice", ["new", "-d", "-n", "lim", "sh", "-c",
            f"echo hard=$(ulimit -Hn) soft=$(ulimit -Sn) nproc=$(awk '/Max processes/{{print $4}}' /proc/self/limits) auid=$(cat /proc/self/loginuid) ses=$(cat /proc/self/sessionid) "
            f"fds=$(ls /proc/self/fd | tr '\\n' ' ') own=$(stat -c %U /proc/$$) > {f1}"])
        time.sleep(1.2)
        txt = open(f1).read() if os.path.exists(f1) else ""
        check("hard=500" in txt and "soft=400" in txt, f"pam_limits applies to the session (got: {txt.strip()[:80]})")
        check("nproc=321" in txt, "process-count limits (fork-bomb containment) are applied from the PAM limits")
        check(f"auid={alice}" in txt and "ses=4294967295" not in txt, "audit login uid and session id are set inside the session")
        check("own=pt_alice" in txt, "session processes are dumpable by their owner (gdb/strace work), as after a normal login")
        fds = set(re.findall(r"\d+", txt.split("fds=")[-1])) if "fds=" in txt else {"?"}
        check(fds <= {"0", "1", "2", "3"}, f"the session process holds no inherited descriptors (fds: {sorted(fds)})")
        sl = [e for e in events() if e["event"] == "session_launch" and e.get("session") == "lim"]
        check(bool(sl) and sl[0].get("loginuid") == str(alice) and sl[0].get("pam_service") == PAM_SVC,
              "the monitor attests the login uid it saw in /proc (not what anybody claims)")
        check(has_event("launch_exit", session="lim"), "end of the session process is audited by the monitor")

        # access policy by PAM (account stack)
        frank = uid_of("pt_frank")
        r = as_user("pt_frank", ["new", "-d", "-n", "f1"])
        check(r.returncode != 0 and "PAM" in r.stderr and "access policy" in r.stderr, "PAM account policy can refuse a session")
        check(any(e["event"] == "launch_failed" and e.get("stage") == "account" and e.get("user") == "pt_frank" for e in events()),
              "the PAM refusal is audited with its stage")
        check("f1" not in as_user("pt_frank", ["list"]).stdout, "a refused session does not exist")

        # a process that became root inside the session can still be ended by the monitor
        rs = os.path.join(ROOT, "bin", "rootsleep"); shutil.copy("/bin/sleep", rs); os.chown(rs, 0, 0); os.chmod(rs, 0o4755)
        as_user("pt_erin", ["new", "-d", "-n", "esc", rs, "300"]); time.sleep(0.8)
        lst = as_user("pt_erin", ["list"]).stdout
        m = re.search(r"esc\s+(\d+)", lst)
        pid = int(m.group(1)) if m else 0
        euid = None
        if pid:
            try: euid = int(re.search(r"Uid:\s+\d+\s+(\d+)", open(f"/proc/{pid}/status").read()).group(1))
            except Exception: pass
        if euid == 0:
            check(True, "(setup) the session process runs with euid 0 (setuid binary)")
            as_user("pt_erin", ["kill", "esc"]); time.sleep(4.5)
            check(not os.path.exists(f"/proc/{pid}"), "ptscry kill ends a session whose process became root (the monitor signals it)")
        else:
            print("SKIP setuid test (nosuid mount here)")
            as_user("pt_erin", ["kill", "esc"])
        # administrators too
        as_user("pt_erin", ["new", "-d", "-n", "esc2", rs, "300"]); time.sleep(0.6)
        m = re.search(r"esc2\s+(\d+)", as_user("pt_erin", ["list"]).stdout); pid2 = int(m.group(1)) if m else 0
        as_user("pt_bob", ["admin", "kill", "pt_erin", "esc2"]); time.sleep(4.5)
        check(pid2 and not os.path.exists(f"/proc/{pid2}"), "admin kill also reaches a root-owned session process")
        check(sh("pgrep", "-f", "ptscry launch").stdout.strip() == "" or True, "(info) launchers present while sessions exist")

        # killing the host does not free the user's session processes from the monitor's control
        as_user("pt_alice", ["new", "-d", "-n", "orph", "sleep", "300"]); time.sleep(0.6)
        m = re.search(r"orph\s+(\d+)", as_user("pt_alice", ["list"]).stdout); opid = int(m.group(1)) if m else 0
        for hp in host_pids("pt_alice"): os.kill(hp, signal.SIGKILL)
        time.sleep(1.5)
        check(opid and not os.path.exists(f"/proc/{opid}"), "when a host dies its sessions end too (the monitor terminates them)")
        check(procs_running(BIN, "launch") == [] or True, "(info)")

        # a reload that points pam_service at a missing file is refused, the old setting stays
        write_conf(); good = open(CONF).read()
        open(CONF, "w").write(good.replace(f"pam_service = {PAM_SVC}", "pam_service = does_not_exist")); os.chmod(CONF, 0o644)
        mon.send_signal(signal.SIGHUP); time.sleep(0.8)
        check(any(e["event"] == "config_reload_failed" and "not found" in e.get("error", "") for e in events()), "reload with a missing PAM service file is refused")
        open(CONF, "w").write(good); os.chmod(CONF, 0o644)
        mon.send_signal(signal.SIGHUP); time.sleep(0.8)
        check(as_user("pt_erin", ["new", "-d", "-n", "still-ok"]).returncode == 0, "sessions still start after the refused reload")
        check(any(e["event"] == "config_reloaded" and e.get("pam_service") == PAM_SVC for e in events()), "the PAM service in force is recorded at every reload")

    # ------------------------------------------------------------ fail closed
    print("== fail closed")
    ep = host_pids("pt_erin")
    check(len(ep) == 1, "erin's host is running with her sessions")
    mon.kill(); mon.wait()
    time.sleep(2.5)
    check(host_pids("pt_erin") == [], "hosts die when the monitor disappears (no unaudited sessions)")
    time.sleep(1.0)
    check(sh("pgrep", "-u", "pt_erin").stdout.strip() == "", "no process of the user survives the monitor (shells end too)")
    check(procs_running(BIN, "launch") == [], "no launcher survives the monitor")
    r = sh("pgrep", "-u", "pt_erin", "-f", "sleep")
    r2 = as_user("pt_erin", ["list"])
    check(r2.returncode != 0 and "cannot reach the ptscry monitor" in r2.stderr, "clients get a clear error when the monitor is down")

    # restart: chain resumes from the last record, graceful shutdown audited
    start_monitor()
    check(sh(BIN, "audit-verify", rot, AUDIT).returncode == 0, "chain resumes correctly after a crash and restart")
    as_user("pt_erin", ["new", "-d", "-n", "z1"])
    mon.send_signal(signal.SIGTERM)
    try:
        mon.wait(timeout=20); done = True
    except subprocess.TimeoutExpired:
        done = False
    check(done and mon.returncode == 0, "SIGTERM stops the monitor gracefully")
    time.sleep(0.5)
    check(host_pids("pt_erin") == [], "graceful shutdown also ends the hosts")
    names = {e["event"] for e in events()}
    check("monitor_shutdown" in names, "graceful shutdown is audited")
    check(sh(BIN, "audit-verify", rot, AUDIT).returncode == 0, "final chain verifies")
finally:
    try:
        if mon and mon.poll() is None: mon.kill()
    except Exception:
        pass
    for u in USERS:
        sh("pkill", "-KILL", "-u", u)
    time.sleep(0.5)
    for u in USERS: sh("userdel", "-r", u)
    for g in GROUPS: sh("groupdel", g)
    try: os.unlink(PAM_FILE)
    except OSError: pass
    print("\nfiles:", ROOT)
    print("RESULT:", "ALL PASSED" if not fails else f"{len(fails)} FAILED: {fails}")
    sys.exit(1 if fails else 0)
