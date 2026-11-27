//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// Persistent audited PTY sessions: detach, reattach, audit and recording.

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"
)

type exitStatus struct{ code int }

func (e *exitStatus) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func usage(w io.Writer) {
	fmt.Fprintf(w, `%s - %s
  %s (version %s)

Usage:
  ptscry new [-d] [-A] [-n name] [command [args...]]
                     create a session and attach to it
                       -d  create without attaching (prints the name)
                       -A  attach if the named session already exists
                       (like: tmux new-session -A -s name)
  ptscry run [-n name] [-k] [-q] command [args...]
                     run a command as a session without a terminal; follows its
                     output, exits with its exit status; the job survives a lost
                     connection (-k: kill it instead)
  ptscry run -d [-n name] command [args...]
                     submit a job and return at once, printing its name; its result
                     (exit status, output tail) is kept for job_retention
  ptscry wait [-q] [-x] name
                     follow a session or collect a finished job (read-only, no
                     terminal), exit with its status (-x: discard the result)
  ptscry status [-j] name   running / finished (exit code) / unknown (exit 4)
  ptscry attach [-r] [-c] [name]
                     attach to a session (name optional if only one exists)
                       -r  read-only, -c create if missing
  ptscry list [-j]   list sessions and finished jobs (-j: JSON)
  ptscry kill <name> hang up a session
  ptscry freeze <name> | thaw <name>   stop / resume every process of a session
  ptscry stop        stop the host (and all sessions) of this user
  ptscry rename OLD NEW              give a session another name
  ptscry notify NAME [text...]       show a message on the terminals attached to NAME
  ptscry send-keys [-l] NAME [keys...]
                     type into a session: text and key names (Enter, Tab, Escape,
                     C-c, M-x, Up, F1...; -l: all literal); no keys: standard input
  ptscry layout list [-j] | show NAME | save NAME [FILE|-] | rm NAME
                     layouts (panes and their sessions) kept on the server for
                     the GUI and the terminal client
  ptscry tui [-l LAYOUT] [session...]
                     sessions in panes on this terminal (ptscry-tui, tmux style)
  ptscry attach -l LAYOUT            the same, opening a saved layout
  ptscry split [-h|-v] | layout apply NAME
                     inside ptscry-tui or ptscry-gui: a new pane / a saved layout
  ptscry config      show the effective configuration

Administration (root, or members of admin_group):
  ptscry monitor [-c file]     run the privileged monitor (systemd service)
  ptscry admin list | kill <user> <session> | stop <user>
  ptscry admin reexec          (root) replace the monitor program in place, sessions stay up
  ptscry audit-verify [file...]   verify the audit hash chain
  ptscry check [-u user]       check the deployment and a user (default: you; "doctor" also works)
  ptscry login [-n name]       sshd ForceCommand: confine a login to ptscry
  ptscry totp-init [-f] USER   enrol the TOTP secret of a user (SSH front-end)
  ptscry setup [users|keys]    text wizard: who may use ptscry, front-end public keys
  ptscry setup frontend        turn the SSH front-end on (account, host key, config, reload)
  ptscry setup users list | add [-admin] [-create] USER... | remove [-admin] USER...
  ptscry setup keys list [USER] | add USER FILE|-|-home | remove USER N|SHA256:...
  ptscry admin setup users ... | keys ...
                               the same as "setup users/keys", without sudo for members of admin_group:
                               the monitor does it (creating system accounts, -create, stays root only)

Development:
  ptscry frontend              SSH front-end (started by the monitor, not by hand)
  ptscry host                  standalone per-user host (PTSCRY_STANDALONE=1);
                               its audit is NOT tamper-resistant
  ptscry -H [user@]host[:port] [-i key] [-c cert] [command...]
                               the same commands on another server, through its SSH
                               front-end (also macOS and Windows: see the manual)
  ptscry version

Inside a session: Ctrl-B d detaches, Ctrl-B Ctrl-B sends a literal Ctrl-B
(the prefix is configurable: client.conf or detach_prefix, see the manual).

Configuration: %s (override with PTSCRY_CONFIG)
`, progName, progCredits, progTagline, progVersion, defaultConfigPath)
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		usage(os.Stderr)
		os.Exit(2)
	}
	if strings.HasPrefix(args[0], "-H") { // remote client: ptscry -H user@host[:port] ...
		os.Exit(remoteMain(args))
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "new", "n":
		err = cmdNew(rest)
	case "run":
		err = cmdRun(rest)
	case "wait":
		err = cmdWait(rest)
	case "attach", "a":
		err = cmdAttach(rest)
	case "list", "ls":
		err = cmdList(rest)
	case "status":
		err = cmdStatus(rest)
	case "kill":
		err = cmdKill(rest)
	case "freeze":
		err = cmdFreeze(rest)
	case "thaw":
		err = cmdThaw(rest)
	case "stop":
		err = cmdStop(rest)
	case "rename", "notify", "send-keys", "layout":
		err = cmdOp(cmd, rest)
	case "split":
		err = cmdSplit(rest)
	case "tui":
		err = cmdTUI(rest)
	case "host":
		err = cmdHost(rest)
	case "monitor":
		err = cmdMonitor(rest)
	case "launch":
		err = cmdLaunch(rest)
	case "totp-init":
		err = cmdTotpInit(rest)
	case "setup":
		err = cmdSetup(rest)
	case "frontend":
		err = cmdFrontend(rest)
	case "login":
		err = cmdLogin(rest)
	case "admin":
		err = cmdAdmin(rest)
	case "check", "doctor": // doctor: the old name
		err = cmdCheck(rest)
	case "audit-verify":
		err = cmdAuditVerify(rest)
	case "config":
		err = cmdConfig(rest)
	case "version", "-v", "--version":
		fmt.Println(versionLine())
		fmt.Println(licenseLine)
	case "help", "-h", "--help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "%s: unknown command %q\n\n", progName, cmd)
		usage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		var es *exitStatus
		if errors.As(err, &es) {
			os.Exit(es.code)
		}
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		if err.Error() == "access denied" {
			// the monitor does not say why (it is policy); point to where it is said
			fmt.Fprintf(os.Stderr, "%s: the reason is in \"sudo %s check -u %s\"; usually the user is not in access_group\n"+
				"  (default \"ptscry\"): usermod -aG ptscry %s  (FreeIPA: ipa group-add-member; SSSD cache: sss_cache -E)\n",
				progName, progName, currentUserName(), currentUserName())
		}
		os.Exit(1)
	}
}

// currentUserName is the login name, for hints.
func currentUserName() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "USER"
}
