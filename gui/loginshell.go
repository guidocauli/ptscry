// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// loginshell.go: the user's login shell, for when the program is started
// without $SHELL (an application launched from Finder or a menu does not get
// the one of a terminal: the host would fall back to bash, not zsh).

package main

import (
	"bufio"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strings"
	"time"
)

func loginShell() string {
	if sh := os.Getenv("SHELL"); strings.HasPrefix(sh, "/") {
		return sh
	}
	u, err := user.Current()
	if err != nil {
		return ""
	}
	switch runtime.GOOS {
	case "darwin":
		out, err := runTimeout(exec.Command("dscl", ".", "-read", "/Users/"+u.Username, "UserShell"), 3*time.Second)
		if err == nil {
			// "UserShell: /bin/zsh"
			if f := strings.Fields(string(out)); len(f) >= 2 && strings.HasPrefix(f[len(f)-1], "/") {
				return f[len(f)-1]
			}
		}
	case "linux":
		if f, err := os.Open("/etc/passwd"); err == nil {
			defer f.Close()
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				p := strings.Split(sc.Text(), ":")
				if len(p) >= 7 && p[0] == u.Username && strings.HasPrefix(p[6], "/") {
					return p[6]
				}
			}
		}
	}
	return ""
}

// withShell adds SHELL to an environment that lacks it.
func withShell(env []string) []string {
	for _, e := range env {
		if strings.HasPrefix(e, "SHELL=/") {
			return env
		}
	}
	if sh := loginShell(); sh != "" {
		return append(env, "SHELL="+sh)
	}
	return env
}
