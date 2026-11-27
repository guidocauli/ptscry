#!/usr/bin/env python3
# ptscry - Guido Cauli <guido.cauli@gmail.com> - red team: real attacks by
# unprivileged local users against the root monitor and the per-user hosts.
# Must run as root (it creates test users and removes them at the end).
# Each attack is one function; the result is PASS when the attack FAILS.
# Usage: sudo PTSCRY_BIN=./ptscry python3 tests/e2e_redteam.py
import json, os, pty, select, shutil, signal, socket, struct, subprocess, sys, tempfile, time

SRC = os.path.abspath(os.environ.get("PTSCRY_BIN", "./ptscry"))
ROOT = tempfile.mkdtemp(prefix="ptscry-rt-")
os.chmod(ROOT, 0o755)
BINDIR = os.path.join(ROOT, "bin"); os.mkdir(BINDIR, 0o755)
BIN = os.path.join(BINDIR, "ptscry")
shutil.copy(SRC, BIN); os.chmod(BIN, 0o755)
SOCK = os.path.join(ROOT, "run", "monitor.sock")
AUDIT = os.path.join(ROOT, "log", "audit.log")
REC = os.path.join(ROOT, "log", "rec")
KEYS = os.path.join(ROOT, "keys")
CONF = os.path.join(ROOT, "ptscry.conf")
MLOG = os.path.join(ROOT, "monitor.out")
USERS = ["rt_eve", "rt_vic", "rt_adm", "rt_sudo"]
GROUPS = ["rt_acc", "rt_admins"]
ENV = dict(os.environ, PTSCRY_CONFIG=CONF, TERM="xterm", PATH="/usr/local/bin:/usr/bin:/bin")
ENV.pop("PTSCRY_STANDALONE", None)
fails = []

# tries PTRACE_ATTACH on every new pid as fast as it can (spawn race on the hosts)
RACE_C = r"""#include <stdio.h>
#include <stdlib.h>
#include <sys/ptrace.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>
#include <fcntl.h>
#include <string.h>
#include <errno.h>
static char done[4194304];
int main(int c, char **v){
  int base=0; FILE*f=fopen("/proc/sys/kernel/ns_last_pid","r"); fscanf(f,"%d",&base); fclose(f);
  time_t end=time(0)+atoi(v[1]); int won=0;
  while(time(0)<end){
    f=fopen("/proc/sys/kernel/ns_last_pid","r"); int last; fscanf(f,"%d",&last); fclose(f);
    for(int p=base+1;p<=last+3;p++){
      if(done[p]) continue;
      { char sp[64]; snprintf(sp,64,"/proc/%d/stat",p); FILE*sf=fopen(sp,"r"); if(!sf) continue;
        int pp=0; char cm[64]; if(fscanf(sf,"%*d (%63[^)]) %*c %d",cm,&pp)!=2) pp=0; fclose(sf);
        if(pp!=atoi(v[2])){ done[p]=1; continue; } }
      if(ptrace(PTRACE_ATTACH,p,0,0)!=0){ continue; }
      done[p]=1; {
        char b[256],cmd[256]={0}; snprintf(b,256,"/proc/%d/cmdline",p);
        int fd=open(b,O_RDONLY); int n=fd>=0?read(fd,cmd,255):0; if(fd>=0)close(fd);
        for(int i=0;i<n;i++) if(!cmd[i]) cmd[i]=' ';
        int st; waitpid(p,&st,__WALL);
        printf("ATTACHED %d %s\n",p,cmd); fflush(stdout);
        ptrace(PTRACE_DETACH,p,0,0); won++;
      }
    }
    if(last>base+50) base=last-10;
  }
  printf("won %d\n",won); return 0;
}
"""

def sh(*a, **k): return subprocess.run(a, capture_output=True, text=True, **k)
def check(cond, msg):
    print(("PASS " if cond else "FAIL ") + msg)
    if not cond: fails.append(msg)

def write_conf(extra=""):
    with open(CONF, "w") as f:
        f.write(f"""[global]
monitor_socket = {SOCK}
audit_log = {AUDIT}
record_dir = {REC}
frontend_authorized_keys_dir = {KEYS}
access_group = rt_acc
admin_group = rt_admins
pam_service = off
{extra}
[user:rt_vic]
record = output
""")
    os.chmod(CONF, 0o644)

def as_user(user, args, timeout=20, stdin=None):
    cmd = ["setpriv", f"--reuid={user}", f"--regid={user}", "--init-groups", BIN] + args
    return subprocess.run(cmd, env=ENV, capture_output=True, text=True, timeout=timeout, input=stdin)

def py_as(user, code, timeout=30):
    cmd = ["setpriv", f"--reuid={user}", f"--regid={user}", "--init-groups", "python3", "-I", "-c", code]
    return subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)

RAW = """
import socket,struct,sys,json,time
def req(hello, raw=None, timeout=10):
    s=socket.socket(socket.AF_UNIX); s.settimeout(timeout); s.connect(%r)
    if raw is None:
        p=json.dumps(hello).encode(); raw=struct.pack('>BI',1,len(p))+p
    s.sendall(raw)
    try:
        h=b''
        while len(h)<5:
            c=s.recv(5-len(h))
            if not c: return 'EOF'
            h+=c
        t,n=struct.unpack('>BI',h); d=b''
        while len(d)<n:
            c=s.recv(n-len(d))
            if not c: break
            d+=c
        return '%%d %%s' %% (t, d.decode(errors='replace'))
    except socket.timeout:
        return 'TIMEOUT'
    finally:
        s.close()
""" % SOCK

def raw(user, hello=None, rawbytes=None, timeout=10, expr=None):
    code = RAW + f"\nprint(req({hello!r}, {expr or repr(rawbytes)}, {timeout}))"
    r = py_as(user, code, timeout=timeout + 10)
    return (r.stdout + r.stderr).strip()

def events():
    return [json.loads(l) for l in open(AUDIT) if l.strip()]
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

def host_pid(user):
    r = sh("pgrep", "-u", user, "-f", "ptscry host")
    p = [int(x) for x in r.stdout.split()]
    return p[0] if p else None

def alive(): return mon.poll() is None and "0 " not in raw("rt_vic", {"op": "list"})[:2] and raw("rt_vic", {"op": "list"}).startswith("5 ")

try:
    for g in GROUPS: sh("groupadd", "-f", g)
    for u in USERS: sh("useradd", "-m", "-s", "/bin/bash", u)
    for u in USERS: sh("usermod", "-aG", "rt_acc", u)
    sh("usermod", "-aG", "rt_admins", "rt_adm")
    sh("groupadd", "-f", "sudo"); sh("usermod", "-aG", "sudo", "rt_sudo")
    os.makedirs(KEYS, 0o755, exist_ok=True)
    write_conf()
    start_monitor()
    check(mon.poll() is None, "monitor starts")
    r = as_user("rt_vic", ["new", "-d", "-n", "secret", "sh", "-c", "echo VICTIM-DATA; sleep 600"])
    check(r.returncode == 0, "victim starts a recorded session")

    print("== impersonation of the front-end and of the administrators")
    r = raw("rt_eve", {"op": "attach", "name": "secret", "proxy": {"user": "rt_vic", "ticket": "x" * 32}})
    check(r.startswith("6 ") and "denied" in r, f"forged proxy hello (act as rt_vic) is refused: {r[:60]}")
    for op in ["fe-ticket", "fe-totp", "fe-report"]:
        r = raw("rt_eve", {"op": op, "proxy": {"user": "rt_vic"}})
        check(r.startswith("6 "), f"{op} from a normal user is refused")
    for op, extra in [("admin-list", {}), ("admin-kill", {"user": "rt_vic", "name": "secret"}),
                      ("admin-stop", {"user": "rt_vic"}), ("admin-reexec", {}),
                      ("admin-setup", {"cmd": ["users", "add", "rt_eve", "-admin"]})]:
        r = raw("rt_eve", dict({"op": op}, **extra))
        check(r.startswith("6 ") and "admin" in r.lower() or "denied" in r, f"{op} by a non-administrator is refused")
    check(as_user("rt_vic", ["ls"]).stdout.count("secret") == 1, "victim session survived the attempts")

    print("== reaching another user's session")
    for name in ["secret", "../rt_vic/secret", "rt_vic/secret", "rt_vic:secret"]:
        r = raw("rt_eve", {"op": "attach", "name": name, "readonly": True}, timeout=5)
        check(not r.startswith("5 ") and "VICTIM" not in r, f"attach {name!r} as another user does not reach it")
    r = raw("rt_eve", {"op": "send-keys", "name": "secret", "text": "id\n"})
    check(not r.startswith("5 "), "send-keys to another user's session is refused")
    r = raw("rt_eve", {"op": "kill", "name": "secret"})
    check("secret" in as_user("rt_vic", ["ls"]).stdout, "kill of another user's session has no effect")
    recs = [os.path.join(dp, f) for dp, _, fs in os.walk(REC) for f in fs]
    check(recs and all(os.stat(p).st_uid == 0 and os.stat(p).st_mode & 0o077 == 0 for p in recs), "recordings are root-only")
    r = py_as("rt_eve", f"import os; print(os.listdir({REC!r}))")
    check("PermissionError" in r.stderr, "the recording directory cannot be listed by users")

    print("== malformed and hostile input to the monitor")
    names = ["..", "a/b", "../../etc/passwd", "\x1b]52;c;AAAA\x07", "x" * 65, "-rf", "a\nb"]
    for n in names:
        r = raw("rt_eve", {"op": "new", "name": n, "detached": True, "cmd": ["true"]})
        bad = n not in ("..",)  # ".." is a valid (if odd) session name: it must stay harmless
        if bad:
            check(r.startswith("6 "), f"session name {n!r} is refused")
    big = struct.pack(">BI", 1, 0x7fffffff)
    check(raw("rt_eve", rawbytes=big) in ("EOF", "TIMEOUT") or "6 " in raw("rt_eve", rawbytes=big), "frame header claiming 2 GiB: connection dropped")
    r = raw("rt_eve", expr="struct.pack('>BI', 1, 200000) + b'[' * 200000")
    check(r.startswith("6 ") or r == "EOF", "deeply nested JSON is refused")
    r = raw("rt_eve", {"op": "new", "name": "e1", "detached": True, "cmd": ["sh", "-c", "env; sleep 30"],
                        "env": {"LD_PRELOAD": "/tmp/x.so", "PATH": "/tmp", "LC_X": "y", "TERM": "xterm\nEVIL=1", "BASH_ENV": "/tmp/x"}})
    time.sleep(0.5)
    pid = sh("pgrep", "-u", "rt_eve", "-f", "env; sleep 30").stdout.split()
    envs = open(f"/proc/{pid[0]}/environ", "rb").read().split(b"\0") if pid else []
    check(not any(e.startswith((b"LD_PRELOAD", b"BASH_ENV")) for e in envs) and b"PATH=/tmp" not in envs,
          "client environment cannot inject LD_PRELOAD/BASH_ENV/PATH")
    check(alive(), "monitor alive after malformed input")

    print("== denial of service")
    code = RAW + """
ss=[]
for i in range(200):
    s=socket.socket(socket.AF_UNIX); s.connect(%r); ss.append(s)
time.sleep(1); print('held', len(ss)); time.sleep(4)
""" % SOCK
    p = subprocess.Popen(["setpriv", "--reuid=rt_eve", "--regid=rt_eve", "--init-groups", "python3", "-I", "-c", code],
                         stdout=subprocess.PIPE, text=True)
    time.sleep(1.5)
    t0 = time.time(); r = raw("rt_vic", {"op": "list"}); dt = time.time() - t0
    check(r.startswith("5 ") and dt < 2, f"200 idle connections by one user do not block another user ({dt:.1f}s)")
    p.wait(timeout=20)
    # frozen host: the user stops their own host (they may: same uid) and keeps sending
    as_user("rt_eve", ["ls"])
    hp = host_pid("rt_eve")
    check(hp is not None, "attacker has a host")
    r = py_as("rt_eve", f"import os,signal; os.kill({hp}, signal.SIGSTOP); print('stopped')")
    check("stopped" in r.stdout, "a user can SIGSTOP their own host (expected: same uid)")
    code = RAW + """
import threading
def f():
    try: req({'op':'list'}, timeout=3)
    except Exception: pass
for i in range(400):
    threading.Thread(target=f, daemon=True).start(); time.sleep(0.01)
time.sleep(4); print('done')
"""
    py_as("rt_eve", code, timeout=60)
    t0 = time.time(); r = raw("rt_vic", {"op": "list"}); dt = time.time() - t0
    check(r.startswith("5 ") and dt < 2, f"a frozen host does not block other users ({dt:.1f}s)")
    t0 = time.time(); r = as_user("rt_adm", ["admin", "kill", "rt_eve", "x"], timeout=40); dt = time.time() - t0
    check(dt < 20, f"admin kill against a frozen host returns ({dt:.1f}s)")
    t0 = time.time(); r = as_user("rt_adm", ["admin", "list"], timeout=40); dt = time.time() - t0
    check(r.returncode == 0 and dt < 5, f"admin ls works while a host is frozen ({dt:.1f}s)")
    r = raw("rt_eve", {"op": "list"}, timeout=3)
    print("   attacker's own request while frozen:", r)
    t0 = time.time(); r = as_user("rt_adm", ["admin", "stop", "rt_eve"], timeout=40); dt = time.time() - t0
    check(dt < 20, f"admin stop against a frozen host returns ({dt:.1f}s): {r.stdout.strip()} {r.stderr.strip()}")
    time.sleep(1)
    try: os.kill(hp, signal.SIGCONT)
    except OSError: pass
    time.sleep(1)
    check(alive(), "monitor alive after the frozen-host attack")

    print("== taking over one's own host (ptrace)")
    r = py_as("rt_vic", f"""
import ctypes,os,glob,time
libc=ctypes.CDLL(None,use_errno=True)
PTRACE_ATTACH=16
pids=[int(p.split('/')[2]) for p in glob.glob('/proc/[0-9]*/cmdline') if open(p,'rb').read().endswith(b'\\0host\\0') and open(p[:-7]+'status').read().split('Uid:')[1].split()[0]==str({int(sh('id','-u','rt_vic').stdout)})]
res=[]
for p in pids:
    r=libc.ptrace(PTRACE_ATTACH,p,0,0); res.append((p,r,ctypes.get_errno()))
    if r==0: libc.ptrace(17,p,0,0)
print(res)
""")
    check("-1" in r.stdout and ", 0, 0)" not in r.stdout, f"ptrace on one's running host is refused: {r.stdout.strip()}")
    r = py_as("rt_vic", f"import os,glob\nfor p in glob.glob('/proc/[0-9]*/cmdline'):\n  if open(p,'rb').read().endswith(b'\\0host\\0'):\n    try: os.listdir(os.path.dirname(p)+'/fd'); print('READ', p)\n    except OSError: pass")
    check("READ" not in r.stdout, "/proc/HOST/fd of one's host is not readable")

    # the race: attach in the moment between the exec of a new host and its prctl(PR_SET_DUMPABLE, 0)
    racer = os.path.join(ROOT, "race")
    with open(racer + ".c", "w") as f: f.write(RACE_C)
    have_cc = sh("cc", "-O2", "-o", racer, racer + ".c").returncode == 0
    won = 0
    for i in range(15 if have_cc else 0):
        as_user("rt_eve", ["stop"]); time.sleep(0.3)
        p = subprocess.Popen(["setpriv", "--reuid=rt_eve", "--regid=rt_eve", "--init-groups", racer, "3", str(mon.pid)],
                             stdout=subprocess.PIPE, text=True)
        time.sleep(0.5)
        try: as_user("rt_eve", ["ls"], timeout=8)
        except subprocess.TimeoutExpired:
            print("   ls timed out:", sh("ps", "-o", "pid,stat,wchan:20,args", "-u", "rt_eve").stdout)
        out = p.communicate(timeout=20)[0]
        won += sum(1 for l in out.splitlines() if l.startswith("ATTACHED") and " host" in l)
        print("   race", i, out.strip().replace("\n", " | ")[:300])
    print("   (no C compiler: race not tried)" if not have_cc else "", end="")
    check(won == 0, f"no ptrace attach on a host while it starts (race won {won}/15)")

    print("== administrators (admin_group) cannot become root or other admins")
    sh("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", f"{ROOT}/k")
    key = open(f"{ROOT}/k.pub").read()
    for args, why in [(["users", "add", "-create", "newacct"], "create accounts"),
                      (["users", "add", "-shell", "/bin/sh", "rt_eve"], "set shells"),
                      (["users", "add", "root"], "add root"),
                      (["users", "add", "daemon"], "add system accounts"),
                      (["users", "add", "rt_eve", "-admin"], "option after a name"),
                      (["keys", "add", "rt_sudo", "-"], "keys for sudo members"),
                      (["keys", "add", "root", "-"], "keys for root"),
                      (["keys", "add", "..", "-"], "keys file '..'"),
                      (["keys", "list", ".."], "list keys file '..'"),
                      (["keys", "add", "rt_eve", "/etc/shadow"], "read a root file as key file")]:
        r = raw("rt_adm", {"op": "admin-setup", "cmd": args, "text": key})
        check(r.startswith("6 "), f"admin cannot {why}: {r[:90]!r}")
    check("rt_eve" not in sh("getent", "group", "rt_admins").stdout,
          "rt_eve is not an administrator after the attempts")
    sh("usermod", "-aG", "rt_admins", "rt_vic")
    r = raw("rt_adm", {"op": "admin-setup", "cmd": ["keys", "add", "rt_vic", "-"], "text": key})
    check(r.startswith("6 ") and "administrator" in r, "admin cannot add keys for another admin")
    sh("gpasswd", "-d", "rt_vic", "rt_admins")
    with open("/etc/sudoers.d/rt_eve", "w") as f: f.write("rt_eve ALL=(ALL) NOPASSWD: ALL\n")
    os.chmod("/etc/sudoers.d/rt_eve", 0o440)
    r = raw("rt_adm", {"op": "admin-setup", "cmd": ["keys", "add", "rt_eve", "-"], "text": key})
    check(r.startswith("6 ") or not shutil.which("sudo"), f"admin cannot add keys for a user with sudo rights in sudoers: {r[:90]!r}")
    os.unlink("/etc/sudoers.d/rt_eve")
    check(alive(), "monitor alive at the end")
    as_user("rt_eve", ["ls"]); hp = host_pid("rt_eve")
    py_as("rt_eve", f"import os,signal; os.kill({hp}, signal.SIGSTOP)")
    # fill the control socket of the frozen host: every request handed over to it waits there
    flood = RAW + """
import threading
def f():
    try: req({'op':'list'}, timeout=1)
    except Exception: pass
end=time.time()+60
while time.time()<end:
    threading.Thread(target=f, daemon=True).start(); time.sleep(0.09)
time.sleep(2)
"""
    py_as("rt_eve", flood, timeout=90)
    t0 = time.time(); r = raw("rt_vic", {"op": "list"}); dt = time.time() - t0
    check(r.startswith("5 ") and dt < 2, f"after the flood other users are still served ({dt:.1f}s)")
    t0 = time.time(); r = as_user("rt_adm", ["admin", "kill", "rt_eve", "x"], timeout=40); dt = time.time() - t0
    check(dt < 20, f"admin kill against a flooded frozen host returns ({dt:.1f}s)")
    t0 = time.time(); mon.send_signal(signal.SIGTERM)
    try: mon.wait(timeout=40)
    except subprocess.TimeoutExpired: pass
    dt = time.time() - t0
    check(mon.poll() is not None and dt < 25, f"monitor shutdown is not held up by a frozen host ({dt:.1f}s)")
except Exception:
    import traceback; traceback.print_exc(); fails.append('exception')
finally:
    try:
        if mon and mon.poll() is None: mon.kill()
    except Exception: pass
    for u in USERS: sh("pkill", "-KILL", "-u", u)
    time.sleep(0.5)
    for u in USERS: sh("userdel", "-r", u)
    for g in GROUPS: sh("groupdel", g)
    try: os.unlink("/etc/sudoers.d/rt_eve")
    except OSError: pass
    print("\nfiles:", ROOT)
    print("RESULT:", "ALL PASSED" if not fails else f"{len(fails)} FAILED: {fails}")
    sys.exit(1 if fails else 0)
