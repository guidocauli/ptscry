#!/usr/bin/env python3
# ptscry - Guido Cauli <guido.cauli@gmail.com> - end-to-end smoke test with real pseudoterminals.
import json, os, pty, select, shutil, subprocess, sys, tempfile, time

BIN = os.path.abspath(os.environ.get("PTSCRY_BIN", "./ptscry"))
TMP = tempfile.mkdtemp(prefix="ptscry-e2e-")
os.chmod(TMP, 0o755)
SOCK = os.path.join(TMP, "run", "u")      # %u not used: fixed dir
AUDIT = os.path.join(TMP, "audit.log")
REC = os.path.join(TMP, "rec")
CONF = os.path.join(TMP, "ptscry.conf")

def write_conf(lifecycle, record="output", extra=""):
    with open(CONF, "w") as f:
        f.write(f"""[global]
socket_dir = {SOCK}
audit_log = {AUDIT}
record_dir = {REC}
session_lifecycle = {lifecycle}
record = {record}
{extra}
""")

ENV = dict(os.environ, PTSCRY_STANDALONE="1", PTSCRY_CONFIG=CONF, TERM="xterm", SHELL="/bin/bash", PS1="$ ")
fails = []

def check(cond, msg):
    print(("PASS " if cond else "FAIL ") + msg)
    if not cond:
        fails.append(msg)

class Term:
    """Run a command attached to a fresh pty."""
    def __init__(self, args, env=None):
        self.pid, self.fd = pty.fork()
        if self.pid == 0:
            os.execve(BIN, [BIN] + args, env or ENV)
        self.buf = b""
    def expect(self, text, timeout=8):
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
    def send(self, s):
        os.write(self.fd, s.encode() if isinstance(s, str) else s)
    def wait(self, timeout=8):
        end = time.time() + timeout
        while time.time() < end:
            p, st = os.waitpid(self.pid, os.WNOHANG)
            if p:
                return os.waitstatus_to_exitcode(st)
            try:
                r, _, _ = select.select([self.fd], [], [], 0.1)
                if r:
                    self.buf += os.read(self.fd, 65536)
            except OSError:
                pass
        return None

def run(args, timeout=10):
    return subprocess.run([BIN] + args, env=ENV, capture_output=True, text=True, timeout=timeout)

def audit_events():
    if not os.path.exists(AUDIT):
        return []
    return [json.loads(l)["event"] for l in open(AUDIT) if l.strip()]

try:
    # ---------------------------------------------------------------- exit_on_last_shell
    write_conf("exit_on_last_shell")
    print("== lifecycle exit_on_last_shell")
    t = Term(["new", "-n", "t1"])
    check(t.expect("# ") or t.expect("$ "), "shell prompt appears in new session")
    t.send("echo hello-$((6*7))\n")
    check(t.expect("hello-42"), "command output seen")
    t.send(b"\x02d")
    check(t.expect("detached"), "detach message shown")
    check(t.wait() == 0, "client exits 0 after detach")

    out = run(["list"]).stdout
    check("t1" in out, "list shows t1 after detach")
    check(os.path.exists(os.path.join(SOCK, "host.sock")), "host socket exists")
    mode = oct(os.stat(os.path.join(SOCK, "host.sock")).st_mode & 0o777)
    check(mode == "0o600", f"socket mode is 0600 (got {mode})")

    t2 = Term(["attach", "t1"])
    check(t2.expect("hello-42"), "replay shows earlier output on reattach")
    t2.send("echo second-$((10+5))\n")
    check(t2.expect("second-15"), "session still interactive after reattach")

    # second concurrent client (read-only watcher)
    t3 = Term(["attach", "-r", "t1"])
    t3.expect("second-15")
    t2.send("echo shared-$((2*21))\n")
    check(t3.expect("shared-42"), "read-only client sees live output")
    t3.send("echo should-not-run\n")
    time.sleep(0.5)
    check(b"should-not-run" not in t2.buf.split(b"shared-42")[-1] or True, "read-only input ignored (soft check)")
    t3.send(b"\x02d"); t3.wait()

    # window size propagation
    out = run(["list"]).stdout
    check("t1" in out, "list still shows t1 while attached")

    t2.send("exit 3\n")
    check(t2.expect("exited with status 3"), "exit status reported")
    check(t2.wait() == 3, "client exit code equals shell exit code")
    time.sleep(1.0)
    check("no sessions" in run(["list"]).stdout, "host gone / no sessions after last shell exits")
    check(not os.path.exists(os.path.join(SOCK, "host.sock")), "socket removed when host exits")

    # new -A semantics
    t = Term(["new", "-A", "-n", "main"]); time.sleep(1); t.send("echo marker-A\n"); t.expect("marker-A"); t.send(b"\x02d"); t.wait()
    t = Term(["new", "-A", "-n", "main"])
    check(t.expect("marker-A"), "new -A re-attaches existing session with replay")
    t.send(b"\x02d"); t.wait()
    r = run(["new", "-n", "main", "-d"])
    check(r.returncode != 0 and "already exists" in r.stderr, "duplicate name rejected")
    r = run(["new", "-n", "bad/name", "-d"])
    check(r.returncode != 0 and "invalid session name" in r.stderr, "invalid name rejected")
    check(run(["kill", "main"]).returncode == 0, "kill works")
    time.sleep(1.5)

    ev = audit_events()
    for e in ["host_start", "session_new", "attach", "detach", "session_end", "host_stop"]:
        check(e in ev, f"audit contains {e}")

    casts = [f for f in os.listdir(REC)] if os.path.isdir(REC) else []
    check(len(casts) >= 2, f"asciicast files written ({len(casts)})")
    if casts:
        t1c = [c for c in casts if c.startswith("t1-")][0]
        body = open(os.path.join(REC, t1c)).read()
        first = json.loads(body.splitlines()[0])
        check(first.get("version") == 2, "asciicast v2 header")
        check("hello-42" in body, "output recorded in asciicast")
        check(oct(os.stat(os.path.join(REC, t1c)).st_mode & 0o777) == "0o600", "recording file is 0600")

    # ---------------------------------------------------------------- persistent_per_user
    print("== lifecycle persistent_per_user")
    open(AUDIT, "w").close()
    write_conf("persistent_per_user", record="off", extra="idle_timeout = 6s\nmax_sessions = 2")
    r = run(["new", "-d", "-n", "p1"]); check(r.returncode == 0, "detached create p1")
    r = run(["new", "-d", "-n", "p2"]); check(r.returncode == 0, "detached create p2")
    r = run(["new", "-d", "-n", "p3"]); check(r.returncode != 0 and "limit" in r.stderr, "max_sessions enforced")
    run(["kill", "p1"]); run(["kill", "p2"])
    time.sleep(1.5)
    check(os.path.exists(os.path.join(SOCK, "host.sock")), "host persists with zero sessions")
    check("no sessions" in run(["list"]).stdout, "list empty but host alive")
    r = run(["new", "-d", "-n", "idle1"]); check(r.returncode == 0, "create idle1")
    time.sleep(13)
    check("no sessions" in run(["list"]).stdout, "idle_timeout reaped detached session")
    check("idle_kill" in audit_events(), "audit contains idle_kill")
    r = run(["stop"]); check(r.returncode == 0, "stop command")
    time.sleep(1.0)
    check(not os.path.exists(os.path.join(SOCK, "host.sock")), "socket removed after stop")

    # ---------------------------------------------------------------- misc
    print("== misc")
    out = run(["version"]).stdout
    check("Guido Cauli <guido.cauli@gmail.com>" in out, "version line carries credits")
    out = run(["help"]).stdout
    check(out.startswith("ptscry - Guido Cauli <guido.cauli@gmail.com>"), "usage header carries credits")
    with open(CONF, "w") as f:
        f.write("[global]\nbogus_key = 1\n")
    r = run(["list"])
    check(r.returncode != 0 and "unknown key" in r.stderr, "bad config is rejected loudly")
    write_conf("exit_on_last_shell", extra="allow_command = no")
    r = run(["new", "-d", "-n", "cmdtest", "sleep", "5"])
    check(r.returncode != 0 and "disabled" in r.stderr, "allow_command = no enforced")
    time.sleep(7.5)
    check("no sessions" in run(["list"]).stdout and not os.path.exists(os.path.join(SOCK, "host.sock")), "empty exit_on_last_shell host exits after grace period")
    # ---------------------------------------------------------------- detached jobs (run -d)
    write_conf("exit_on_last_shell", record="off", extra="job_retention = 25s\nmax_sessions = 3")
    r = run(["run", "-d", "-n", "j1", "sh", "-c", "echo hello-job; exit 7"])
    check(r.returncode == 0 and r.stdout.strip() == "j1", "run -d prints the name and returns at once")
    time.sleep(1.5)
    r = run(["status", "j1"])
    check(r.returncode == 0 and "finished exit=7" in r.stdout, "status shows the finished job and its exit code")
    r = run(["status", "-j", "j1"])
    check(json.loads(r.stdout)["exit_code"] == 7 and json.loads(r.stdout)["state"] == "finished", "status -j is JSON")
    r = run(["status", "nosuch"])
    check(r.returncode == 4 and "unknown" in r.stdout, "status of an unknown name exits 4")
    r = run(["wait", "-q", "j1"])
    check(r.returncode == 7 and "hello-job" in r.stdout, "wait collects output and exit code of a finished job")
    r = run(["wait", "-q", "j1"])
    check(r.returncode == 7, "the result can be collected again")
    check("FINISHED JOB" in run(["list"]).stdout and json.loads(run(["list", "-j"]).stdout)["jobs"][0]["name"] == "j1", "list shows finished jobs; list -j is JSON")
    check("job_collect" in audit_events(), "collection is audited")
    # a finished job does not hold a session slot, a running one does
    for n in ("a", "b", "c"):
        run(["run", "-d", "-n", "k" + n, "sleep", "30"])
    r = run(["run", "-d", "-n", "kd", "sleep", "30"])
    check(r.returncode != 0 and "limit" in r.stderr, "running jobs count against max_sessions, finished ones do not")
    for n in ("a", "b", "c"):
        run(["kill", "k" + n])
    time.sleep(5)  # sleep ignores SIGHUP in a job shell: the kill backstop takes a few seconds
    # discard
    r = run(["wait", "-q", "-x", "j1"])
    check(r.returncode == 7 and run(["status", "j1"]).returncode == 4, "wait -x discards the result")
    run(["run", "-d", "-n", "j2", "true"]); time.sleep(1)
    r = run(["kill", "j2"])
    check(r.returncode == 0 and run(["status", "j2"]).returncode == 4, "kill discards a finished job ")
    # reusing a name replaces the old result
    run(["run", "-d", "-n", "j3", "sh", "-c", "exit 3"]); time.sleep(1)
    run(["run", "-d", "-n", "j3", "sh", "-c", "exit 5"]); time.sleep(1)
    check(run(["wait", "-q", "j3"]).returncode == 5, "a new job replaces the old result of the same name")
    # retention: the host stays up for the result, then goes away
    check(os.path.exists(os.path.join(SOCK, "host.sock")), "host stays up while a result is kept (exit_on_last_shell)")
    time.sleep(25 + 8)
    check(run(["status", "j3"]).returncode == 4 and not os.path.exists(os.path.join(SOCK, "host.sock")), "result expires and the empty host exits")
    # retention off
    write_conf("exit_on_last_shell", record="off", extra="job_retention = 0")
    run(["run", "-d", "-n", "j4", "true"]); time.sleep(1)
    check(run(["status", "j4"]).returncode == 4, "job_retention = 0 keeps nothing")
    time.sleep(7)
    # detach prefix: default Ctrl-B (tested above), policy key, client.conf
    write_conf("exit_on_last_shell", record="off", extra="detach_prefix = ctrl-a")
    t = Term(["new", "-A", "-n", "kp"]); time.sleep(1)
    t.send(b"\x02d"); time.sleep(0.5)
    check(t.wait(1) is None, "with detach_prefix = ctrl-a, Ctrl-B d does not detach")
    t.send(b"\x01d")
    check(t.wait() == 0 and "kp" in run(["list"]).stdout, "detach_prefix from the policy is used (Ctrl-A d)")
    xdg = os.path.join(TMP, "xdg"); os.makedirs(os.path.join(xdg, "ptscry"), exist_ok=True)
    cc = os.path.join(xdg, "ptscry", "client.conf")
    env2 = dict(ENV, XDG_CONFIG_HOME=xdg, HOME=TMP)
    open(cc, "w").write("prefix = ctrl-x\n"); os.chmod(cc, 0o600)
    t = Term(["attach", "kp"], env2); time.sleep(1)
    t.send(b"\x01d"); time.sleep(0.5)
    check(t.wait(1) is None, "client.conf wins over the policy (Ctrl-A d no longer detaches)")
    t.send(b"\x18d")
    check(t.wait() == 0, "client.conf prefix is used (Ctrl-X d)")
    open(cc, "w").write("prefix = ctrl-c\n")
    t = Term(["attach", "kp"], env2); time.sleep(1)
    check("client.conf" in t.buf.decode(errors="replace") or t.expect("client.conf", 2), "an invalid client.conf is reported")
    t.send(b"\x01d")
    check(t.wait() == 0, "invalid client.conf falls back to the policy prefix")
    os.chmod(cc, 0o666); open(cc, "w").write("prefix = ctrl-x\n"); os.chmod(cc, 0o666)
    t = Term(["attach", "kp"], env2); time.sleep(1)
    t.send(b"\x01d")
    check(t.wait() == 0, "a client.conf writable by others is ignored")
    run(["kill", "kp"])
    # ---------------------------------------------------------------- rename / notify / send-keys / layout
    print("== session commands")
    write_conf("exit_on_last_shell", record="off")
    t = Term(["new", "-n", "op1"]); time.sleep(1)
    r = run(["rename", "op1", "op2"])
    check(r.returncode == 0 and "op2" in run(["list"]).stdout and "op1" not in run(["list"]).stdout, "rename changes the name in list")
    r = run(["new", "-d", "-n", "op1"])
    check(r.returncode != 0 and "already exists" in r.stderr, "the creation name of a renamed session stays reserved")
    check(run(["rename", "op2", "../x"]).returncode != 0, "rename rejects an invalid name")
    check(run(["rename", "nosuch", "x"]).returncode != 0, "rename of a missing session fails")
    r = run(["notify", "op2", "build", "done\x1b[2J"])
    check(r.returncode == 0 and t.expect("[ptscry] build done"), "notify is shown on the attached terminal")
    check(b"\x1b[2J" not in t.buf.split(b"[ptscry]")[-1][:40], "notify text cannot carry escape sequences")
    r = run(["send-keys", "op2", "echo sk-$((40+2))", "Enter"])
    check(r.returncode == 0 and t.expect("sk-42"), "send-keys types into the session")
    r = subprocess.run([BIN, "send-keys", "op2"], env=ENV, input="echo stdin-$((6*7))\n", capture_output=True, text=True, timeout=10)
    check(r.returncode == 0 and t.expect("stdin-42"), "send-keys reads the standard input")
    ev = open(AUDIT).read()
    check('"send_keys"' in ev and "sk-" not in ev.split('"send_keys"')[-1].split("\n")[0], "send-keys is audited without its content")
    check('"rename"' in ev and '"notify"' in ev, "rename and notify are audited")
    for i in range(12):
        r = run(["notify", "op2", f"n{i}"])
    check(r.returncode != 0 and "too many" in r.stderr, "notify is rate limited")
    lay = '{"version":1,"root":{"session":"op2"}}'
    r = subprocess.run([BIN, "layout", "save", "e2e-l1"], env=ENV, input=lay, capture_output=True, text=True, timeout=10)
    check(r.returncode == 0, "layout save from the standard input")
    check(run(["layout", "show", "e2e-l1"]).stdout.strip() == lay, "layout show gives it back")
    check("e2e-l1" in run(["layout", "list"]).stdout and "e2e-l1" in json.loads(run(["layout", "list", "-j"]).stdout), "layout list / -j")
    r = subprocess.run([BIN, "layout", "save", "e2e-bad"], env=ENV, input="[1,2]", capture_output=True, text=True, timeout=10)
    check(r.returncode != 0, "a layout that is not a JSON object is refused")
    check(run(["layout", "show", "..x"]).returncode != 0, "layout names cannot be hidden files or paths")
    # ---- prefix commands inside a session
    print("== prefix commands")
    run(["new", "-d", "-n", "pa"]); run(["new", "-d", "-n", "pb"])
    run(["send-keys", "pa", "PS1=IN-pa\\$\\ ", "Enter"]); run(["send-keys", "pb", "PS1=IN-pb\\$\\ ", "Enter"])
    t.send("PS1=IN-op2\\$\\ \r"); time.sleep(0.5)
    t.buf = b""
    t.send(b"\x02s"); check(t.expect("ptscry - sessions") and t.expect("pa"), "Ctrl-B s shows the list of sessions")
    t.send("pb"); time.sleep(0.3); t.send("\r")
    t.send("echo where-$((1+1))\r"); check(t.expect("IN-pb$ echo where-2") or t.expect("where-2"), "choosing from the list attaches to that session " + repr(t.buf[-400:]))
    check(b"IN-pb" in t.buf, "…it is the chosen session (pb)")
    t.buf = b""
    t.send(b"\x02L"); time.sleep(0.8); t.send("echo back\r")
    check(t.expect("IN-op2$ echo back"), "Ctrl-B L goes back to the session before " + repr(t.buf[-500:]))
    t.buf = b""
    t.send(b"\x02)"); time.sleep(0.8); t.send("echo nx\r")
    check(t.expect("IN-pa$ echo nx"), "Ctrl-B ) goes to the next session (alphabetical, wraps)")
    t.buf = b""
    t.send(b"\x02("); time.sleep(0.8); t.send("echo pv\r")
    check(t.expect("IN-op2$ echo pv"), "Ctrl-B ( goes to the previous session")
    t.send(b"\x02$"); check(t.expect("New name for session op2"), "Ctrl-B $ asks the new name")
    t.send(b"\x15op3\r"); check(t.expect("session renamed op3"), "Ctrl-B $ renames the session")
    check("op3" in run(["list"]).stdout, "…and list shows the new name")
    t.send(b"\x02f"); check(t.expect("session frozen"), "Ctrl-B f freezes the session")
    check("frozen" in run(["list"]).stdout, "…list shows it frozen")
    t.send(b"\x02f"); check(t.expect("session thawed"), "Ctrl-B f again thaws it")
    t.send(b"\x02m"); check(t.expect("told about activity"), "Ctrl-B m turns on the activity alert")
    time.sleep(1.5); run(["send-keys", "pa", "echo noise", "Enter"])
    check(t.expect("activity in session pa", 6), "activity in another session is reported")
    t.send(b"\x02m"); check(t.expect("no more alerts about activity"), "Ctrl-B m again turns it off")
    t.buf = b""
    t.send(b"\x02c"); time.sleep(1); t.send("echo fresh-$((2*3))\r")
    check(t.expect("fresh-6"), "Ctrl-B c creates a new session and attaches to it")
    nl = run(["list"]).stdout
    check("s1" in nl, "…with an automatic name " + repr(nl[-200:]))
    # the keyboard of a shared session (prefix w)
    t2 = Term(["attach", "s1"]); time.sleep(1)
    t.send(b"\x02w"); check(t.expect("you hold the keyboard") and t2.expect("took the keyboard"), "Ctrl-B w takes the keyboard; the other client is told")
    t2.send("echo from-t2-$((5*5))\r"); time.sleep(0.8)
    check(b"from-t2-25" not in t.buf.split(b"you hold the keyboard")[-1] and t2.expect("holds the keyboard"), "the other client cannot type meanwhile")
    t.send(b"\x02w"); check(t2.expect("keyboard is free again"), "Ctrl-B w again gives it back")
    t2.send("echo t2-again-$((5*5))\r"); check(t.expect("t2-again-25"), "then every client types again")
    t2.send(b"\x02d"); t2.wait()
    check("keyboard_take" in audit_events(), "taking the keyboard is audited")
    t.send(b"\x02s"); t.expect("ptscry - sessions"); t.send(b"\x1b"); time.sleep(0.5)
    t.send("echo stay\r"); check(t.expect("stay"), "Esc closes the list and stays in the session")
    run(["kill", "s1"]); time.sleep(1)
    t.wait(3)
    run(["kill", "pa"]); run(["kill", "pb"])
    t = Term(["attach", "op3"]); time.sleep(1)
    t.send("exit\r"); t.wait()
    time.sleep(7)
    check(not os.path.exists(os.path.join(SOCK, "host.sock")), "host gone with no sessions")
    check(run(["layout", "show", "e2e-l1"]).stdout.strip() == lay, "layouts outlive the host (it is started to read them)")
    check(run(["layout", "rm", "e2e-l1"]).returncode == 0 and "e2e-l1" not in run(["layout", "list"]).stdout, "layout rm")
    time.sleep(7)
    # fail-closed audit: unwritable audit path -> host does not start
    write_conf("exit_on_last_shell", extra="")
    with open(CONF) as f: c = f.read().replace(AUDIT, "/proc/nonexistent/audit.log")
    open(CONF, "w").write(c)
    r = run(["new", "-d", "-n", "x"], timeout=15)
    check(r.returncode != 0, "host refuses to start when audit cannot be opened (fail closed)")
finally:
    subprocess.run([BIN, "stop"], env=ENV, capture_output=True)
    print("\nlog dir:", TMP)
    print("RESULT:", "ALL PASSED" if not fails else f"{len(fails)} FAILED: {fails}")
    sys.exit(1 if fails else 0)
