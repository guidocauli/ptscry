//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// list_fmt.go: text and JSON renderings of `list` and `status`, shared by the
// local client and the SSH front-end.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"
)

// formatList renders the running sessions and, if any, the finished jobs.
func formatList(r okReply) string {
	if len(r.Sessions) == 0 && len(r.Jobs) == 0 {
		return "no sessions\n"
	}
	var b bytes.Buffer
	if len(r.Sessions) > 0 {
		tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tPID\tATTACHED\tSIZE\tCREATED\tLAST ACTIVITY\tCOMMAND")
		for _, s := range r.Sessions {
			if s.Frozen {
				s.Name += " [frozen]"
			}
			fmt.Fprintf(tw, "%s\t%d\t%d\t%dx%d\t%s\t%s\t%s\n", s.Name, s.PID, s.Attached, s.Cols, s.Rows,
				s.Created.Local().Format("2006-01-02 15:04:05"), s.Last.Local().Format("15:04:05"), s.Cmd)
		}
		_ = tw.Flush()
	}
	if len(r.Jobs) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "FINISHED JOB\tEXIT\tENDED\tKEPT UNTIL\tCOMMAND")
		for _, j := range r.Jobs {
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", j.Name, j.ExitCode,
				j.Ended.Local().Format("2006-01-02 15:04:05"), j.Expires.Local().Format("15:04:05"), j.Cmd)
		}
		_ = tw.Flush()
	}
	return b.String()
}

// listJSON is the machine-readable form of `list -j`.
func listJSON(r okReply) string {
	out := struct {
		Sessions []sessionInfo `json:"sessions"`
		Jobs     []jobInfo     `json:"jobs"`
	}{r.Sessions, r.Jobs}
	if out.Sessions == nil {
		out.Sessions = []sessionInfo{}
	}
	if out.Jobs == nil {
		out.Jobs = []jobInfo{}
	}
	b, _ := json.Marshal(out)
	return string(b) + "\n"
}

const statusUnknown = 4 // exit status of `ptscry status` for a name that does not exist

// statusOf describes one session or finished job. The exit status is 0 when it
// exists (running or finished) and statusUnknown when it does not: the job's
// own exit code is in the text, so scripts never confuse the two.
func statusOf(r okReply, name string, asJSON bool) (string, int) {
	type st struct {
		Name     string     `json:"name"`
		State    string     `json:"state"` // running | finished | unknown
		PID      int        `json:"pid,omitempty"`
		ExitCode *int       `json:"exit_code,omitempty"`
		Reason   string     `json:"reason,omitempty"`
		Created  *time.Time `json:"created,omitempty"`
		Ended    *time.Time `json:"ended,omitempty"`
	}
	x := st{Name: name, State: "unknown"}
	code := statusUnknown
	for _, s := range r.Sessions {
		if s.Name == name {
			c := s.Created
			x.State, x.PID, x.Created, code = "running", s.PID, &c, 0
		}
	}
	for _, j := range r.Jobs {
		if j.Name == name && x.State == "unknown" {
			c, e, ec := j.Created, j.Ended, j.ExitCode
			x.State, x.ExitCode, x.Reason, x.Created, x.Ended, code = "finished", &ec, j.Reason, &c, &e, 0
		}
	}
	if asJSON {
		b, _ := json.Marshal(x)
		return string(b) + "\n", code
	}
	switch x.State {
	case "running":
		return fmt.Sprintf("%s running pid=%d\n", name, x.PID), code
	case "finished":
		return fmt.Sprintf("%s finished exit=%d reason=%s\n", name, *x.ExitCode, x.Reason), code
	}
	return fmt.Sprintf("%s unknown\n", name), code
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("j", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errors.New("usage: ptscry list [-j]")
	}
	fc, r, err := connect(hello{Op: "list"}, false)
	if err != nil {
		if errors.Is(err, errNoHost) {
			r = okReply{}
		} else {
			return err
		}
	} else {
		fc.c.Close()
	}
	if *asJSON {
		fmt.Print(listJSON(r))
	} else {
		fmt.Print(formatList(r))
	}
	return nil
}

// cmdStatus: ptscry status [-j] name. Exit 0 if the session/job exists, 4 if not.
func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("j", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		return errors.New("usage: ptscry status [-j] name")
	}
	fc, r, err := connect(hello{Op: "list"}, false)
	if err != nil {
		if errors.Is(err, errNoHost) {
			r = okReply{}
		} else {
			return err
		}
	} else {
		fc.c.Close()
	}
	text, code := statusOf(r, fs.Arg(0), *asJSON)
	fmt.Print(text)
	if code != 0 {
		return &exitStatus{code}
	}
	return nil
}
