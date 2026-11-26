// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// availableShells: the login shell first, then those of /etc/shells that exist.
func availableShells() []shellChoice {
	var out []shellChoice
	seen := map[string]bool{}
	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" || !strings.HasPrefix(path, "/") {
			return
		}
		real, err := filepath.EvalSymlinks(path)
		if err != nil || !fileOK(real) {
			return
		}
		name := filepath.Base(path)
		switch name {
		case "rbash", "tmux", "screen", "nologin", "false", "git-shell", "scponly":
			return // in /etc/shells, but not a shell to work in
		}
		if seen[name] { // /bin/bash and /usr/bin/bash are the same shell
			return
		}
		seen[name] = true
		out = append(out, shellChoice{Name: name, Cmd: path})
	}
	add(loginShell())
	if f, err := os.Open("/etc/shells"); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			l := sc.Text()
			if i := strings.IndexByte(l, '#'); i >= 0 {
				l = l[:i]
			}
			add(l)
		}
	}
	for _, p := range []string{"/bin/bash", "/bin/zsh", "/usr/bin/fish", "/bin/sh"} {
		add(p)
	}
	return out
}

const shellHint = "/usr/bin/fish -l"

// autoShellName is the shell used when none is chosen.
func autoShellName() string {
	if sh := loginShell(); sh != "" {
		return sh
	}
	return "/bin/sh"
}
