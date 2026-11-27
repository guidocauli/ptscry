#!/usr/bin/env python3
# ptscry - Guido Cauli <guido.cauli@gmail.com>
# e2e_tui.py: ptscry-tui (panes on a text terminal) driven through tmux,
# against a standalone host. PTSCRY_BIN=./ptscry PTSCRY_TUI=./ptscry-tui
import os, shutil, subprocess, sys, tempfile, time, json

BIN = os.path.abspath(os.environ.get("PTSCRY_BIN", "./ptscry"))
TUI = os.path.abspath(os.environ.get("PTSCRY_TUI", "./ptscry-tui"))
if not shutil.which("tmux"):
    print("SKIP: tmux not installed"); sys.exit(0)
TMP = tempfile.mkdtemp(prefix="ptscry-tui-")
CONF = os.path.join(TMP, "p.conf")
open(CONF, "w").write(f"[global]\nsocket_dir = {TMP}/sock\naudit_log = {TMP}/audit.log\nsession_lifecycle = exit_on_last_shell\nrecord = off\n")
BINDIR = os.path.join(TMP, "bin"); os.makedirs(BINDIR)
shutil.copy(BIN, os.path.join(BINDIR, "ptscry")); shutil.copy(TUI, os.path.join(BINDIR, "ptscry-tui"))
ENV = dict(os.environ, PTSCRY_STANDALONE="1", PTSCRY_CONFIG=CONF, PATH=BINDIR + ":" + os.environ["PATH"], TERM="xterm-256color", HOME=TMP)
ENV.pop("PTSCRY_SESSION", None); ENV.pop("TMUX", None)
fails = []
def check(c, m):
    print(("PASS " if c else "FAIL ") + m)
    if not c: fails.append(m)
def tm(*a): return subprocess.run(["tmux", "-L", "ptscry-e2e"] + list(a), capture_output=True, text=True, env=ENV)
def screen(): return tm("capture-pane", "-p", "-t", "t").stdout
def keys(*k): tm("send-keys", "-t", "t", *k)
def wait_for(s, t=8):
    end = time.time() + t
    while time.time() < end:
        if s in screen(): return True
        time.sleep(0.2)
    return False
def run(args): return subprocess.run([os.path.join(BINDIR, "ptscry")] + args, env=ENV, capture_output=True, text=True, timeout=20)
def start(cmd):
    tm("kill-server")
    tm("-f", "/dev/null", "new-session", "-d", "-s", "t", "-x", "120", "-y", "35", cmd + f" 2>{TMP}/err; sleep 60")
try:
    start("ptscry tui")
    check(wait_for("main"), "ptscry tui opens a pane on session main")
    keys("C-b", "%"); time.sleep(2); keys("echo PANE-B-$((2*21))", "Enter")
    check(wait_for("PANE-B-42") and "│" in screen(), "prefix % splits side by side")
    keys("C-b", '"'); time.sleep(2); keys("echo PANE-C", "Enter")
    check(wait_for("pane 3/3"), "prefix \" splits again (3 panes in the status bar)")
    keys("C-b", "Left"); time.sleep(0.4); keys("echo LEFT-$((1+1))", "Enter")
    check(wait_for("LEFT-2") and "main" in screen().splitlines()[-1], "prefix arrow moves the keyboard")
    keys("ptscry split -v", "Enter"); time.sleep(4)
    check(wait_for("pane 2/4") or wait_for("/4"), "ptscry split (OSC 7799) opens a pane from inside")
    keys("C-b", "S"); time.sleep(0.4); keys("echo SYNC-$((3*3))", "Enter"); time.sleep(1.5)
    check(screen().count("SYNC-9") >= 4 and "SYNC INPUT" in screen(), "prefix S: synchronized input to every pane, shown in the status bar")
    keys("C-b", "S"); time.sleep(0.3)
    keys("C-b", "z"); time.sleep(0.8)
    check("│" not in screen() and "ZOOM" in screen(), "prefix z zooms the pane")
    keys("C-b", "z"); time.sleep(0.5)
    keys("C-b", "["); time.sleep(0.5)
    check(wait_for("COPY"), "prefix [ enters copy mode")
    keys("q"); time.sleep(0.3)
    keys("C-b", "W"); time.sleep(0.4); keys("C-u", "e2elay", "Enter")
    check(wait_for("saved"), "prefix W saves the layout on the server")
    lay = json.loads(run(["layout", "show", "e2elay"]).stdout)
    check(lay["version"] == 1 and "split" in lay["root"], "the saved layout is a tree of sessions")
    keys("C-b", "d"); time.sleep(4)
    check("[detached]" in open(f"{TMP}/err").read(), "prefix d detaches every pane")
    ls = run(["list"]).stdout
    check(ls.count("\n") >= 5, "the sessions of the panes go on")
    start("ptscry attach -l e2elay")
    check(wait_for("/4"), "attach -l opens the layout again (4 panes)")
    check(wait_for("SYNC-9"), "…with what the sessions printed")
    # the cursor: what the program of the focused pane asks (DECSCUSR) reaches the real terminal
    tm("pipe-pane", "-o", "-t", "t", f"cat > {TMP}/raw")
    keys("printf '\\033[6 q'", "Enter"); time.sleep(1.2)
    keys("printf '\\033[0 q'", "Enter"); time.sleep(1.2)
    tm("pipe-pane", "-t", "t")
    raw = open(f"{TMP}/raw", "rb").read()
    check(b"\x1b[6 q" in raw and raw.rfind(b"\x1b[0 q") > raw.find(b"\x1b[6 q"), "the cursor shape asked in the pane reaches the terminal, and goes back to its default")
    keys("C-b", "s"); time.sleep(1.5)
    check(wait_for("ptscry - sessions"), "prefix s goes to the ptscry client of the pane (session list)")
    keys("Escape"); time.sleep(0.5)
    keys("C-b", "d"); time.sleep(4)
finally:
    tm("kill-server")
    subprocess.run([BIN, "stop"], env=ENV, capture_output=True)
    print("\nlog dir:", TMP)
    print("RESULT:", "ALL PASSED" if not fails else f"{len(fails)} FAILED: {fails}")
    sys.exit(1 if fails else 0)
