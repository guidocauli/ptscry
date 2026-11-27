//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// tools.go: login (sshd ForceCommand), admin and audit-verify commands.

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
)

// cmdLogin is meant as sshd's ForceCommand: it turns an SSH login into a
// ptscry session and refuses everything else, so a group can be confined to
// ptscry (bastion mode). The client's command line is never given to a shell.
//
//	ssh -t host                    -> attach-or-create the session "main"
//	ssh -t host ptscry attach foo  -> allowed ptscry subcommands only
func cmdLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	name := fs.String("n", "main", "session used for a plain login")
	if err := fs.Parse(args); err != nil {
		return err
	}
	orig := strings.TrimSpace(os.Getenv("SSH_ORIGINAL_COMMAND"))
	if orig == "" {
		return cmdNew([]string{"-A", "-n", *name})
	}
	for _, r := range orig {
		ok := r == ' ' || r == '_' || r == '.' || r == '/' || r == ':' || r == '=' ||
			r == '@' || r == '+' || r == ',' || r == '-' ||
			(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !ok {
			return &exitStatus{126}
		}
	}
	f := strings.Fields(orig)
	if f[0] != progName || len(f) < 2 {
		fmt.Fprintln(os.Stderr, "ptscry: command not permitted")
		return &exitStatus{126}
	}
	switch f[1] {
	case "new", "attach", "list", "kill", "freeze", "thaw", "run", "wait", "status", "version":
		return dispatchLocal(f[1], f[2:])
	}
	fmt.Fprintf(os.Stderr, "ptscry: %q is not permitted over login\n", f[1])
	return &exitStatus{126}
}

func dispatchLocal(cmd string, rest []string) error {
	switch cmd {
	case "new":
		return cmdNew(rest)
	case "attach":
		return cmdAttach(rest)
	case "list":
		return cmdList(rest)
	case "kill":
		return cmdKill(rest)
	case "freeze":
		return cmdFreeze(rest)
	case "thaw":
		return cmdThaw(rest)
	case "run":
		return cmdRun(rest)
	case "wait":
		return cmdWait(rest)
	case "status":
		return cmdStatus(rest)
	case "version":
		fmt.Println(versionLine())
		return nil
	}
	return fmt.Errorf("unknown command %q", cmd)
}

// cmdAdmin: ptscry admin list | kill <user> <session> | stop <user>
func cmdAdmin(args []string) error {
	if standalone() {
		return errors.New("admin commands need the monitor (not available in standalone mode)")
	}
	if len(args) == 0 {
		return errors.New("usage: ptscry admin list | kill <user> <session> | stop <user> | reexec")
	}
	var hl hello
	switch {
	case args[0] == "list" && len(args) == 1:
		hl = hello{Op: "admin-list"}
	case args[0] == "kill" && len(args) == 3:
		hl = hello{Op: "admin-kill", User: args[1], Name: args[2]}
	case args[0] == "stop" && len(args) == 2:
		hl = hello{Op: "admin-stop", User: args[1]}
	case args[0] == "reexec" && len(args) == 1:
		hl = hello{Op: "admin-reexec"}
	case args[0] == "setup" && len(args) >= 3:
		// as "ptscry setup users|keys ...", without sudo for admin_group: the
		// monitor does it; a key file is read here, by the caller
		hl = hello{Op: "admin-setup", Cmd: args[1:]}
		if (args[1] == "keys" || args[1] == "key") && (args[2] == "add" || args[2] == "import") && len(args) == 5 && args[4] != "-home" {
			var raw []byte
			var err error
			if args[4] == "-" {
				raw, err = io.ReadAll(io.LimitReader(os.Stdin, maxOpText+1))
			} else {
				raw, err = os.ReadFile(args[4])
			}
			if err != nil {
				return err
			}
			if len(raw) > maxOpText {
				return fmt.Errorf("too many keys at once (at most %d bytes)", maxOpText)
			}
			hl.Cmd = append(append([]string{}, args[1:4]...), "-")
			hl.Text = string(raw)
		}
	default:
		return errors.New("usage: ptscry admin list | kill <user> <session> | stop <user> | reexec | setup users|keys ...")
	}
	fc, r, err := connect(hl, false)
	if err != nil {
		return err
	}
	fc.c.Close()
	if hl.Op == "admin-setup" {
		fmt.Print(r.Text)
		return nil
	}
	if hl.Op != "admin-list" {
		fmt.Println(r.Message)
		return nil
	}
	if len(r.Sessions) == 0 {
		fmt.Println("no sessions")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "USER\tNAME\tPID\tATTACHED\tCREATED\tCOMMAND")
	for _, s := range r.Sessions {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\t%s\n", s.User, s.Name, s.PID, s.Attached,
			s.Created.Local().Format("2006-01-02 15:04:05"), s.Cmd)
	}
	return tw.Flush()
}

// cmdAuditVerify checks the hash chain of one or more audit files (in order).
func cmdAuditVerify(args []string) error {
	if len(args) == 0 {
		args = []string{defaultAuditLog}
	}
	n, err := verifyChain(args)
	if err != nil {
		fmt.Printf("FAIL after %d valid records: %v\n", n, err)
		return &exitStatus{1}
	}
	fmt.Printf("OK: %d records, sequence and hash chain intact (%s)\n", n, strings.Join(args, ", "))
	return nil
}
