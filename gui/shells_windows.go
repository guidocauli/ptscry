// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
)

// availableShells: PowerShell 7, Windows PowerShell, cmd, then the bash shells
// that are installed (Git Bash, MSYS2, Cygwin) and WSL.
func availableShells() []shellChoice {
	var out []shellChoice
	q := func(p string) string { return `"` + p + `"` }
	exists := func(p string) bool { st, err := os.Stat(p); return err == nil && !st.IsDir() }
	if p, err := exec.LookPath("pwsh.exe"); err == nil {
		out = append(out, shellChoice{"PowerShell 7", q(p) + " -NoLogo"})
	} else if p := filepath.Join(os.Getenv("ProgramFiles"), "PowerShell", "7", "pwsh.exe"); exists(p) {
		out = append(out, shellChoice{"PowerShell 7", q(p) + " -NoLogo"})
	}
	if p, err := exec.LookPath("powershell.exe"); err == nil {
		out = append(out, shellChoice{"Windows PowerShell", q(p) + " -NoLogo"})
	}
	cmd := os.Getenv("COMSPEC")
	if cmd == "" {
		cmd = filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
	}
	out = append(out, shellChoice{"Prompt dei comandi (cmd)", q(cmd)})
	for _, c := range []struct{ name, path string }{
		{"Git Bash", filepath.Join(os.Getenv("ProgramFiles"), "Git", "bin", "bash.exe")},
		{"Git Bash", filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "Git", "bin", "bash.exe")},
		{"MSYS2 bash", `C:\msys64\usr\bin\bash.exe`},
		{"Cygwin bash", `C:\cygwin64\bin\bash.exe`},
	} {
		if exists(c.path) && !hasShell(out, c.name) {
			out = append(out, shellChoice{c.name, q(c.path) + " --login -i"})
		}
	}
	if p := filepath.Join(os.Getenv("SystemRoot"), "System32", "wsl.exe"); exists(p) {
		out = append(out, shellChoice{"WSL (Linux)", q(p)})
	}
	return out
}

func hasShell(l []shellChoice, name string) bool {
	for _, s := range l {
		if s.Name == name {
			return true
		}
	}
	return false
}

const shellHint = `"C:\Program Files\Git\bin\bash.exe" --login -i`

// autoShellName is the shell used when none is chosen.
func autoShellName() string {
	if isWine() {
		return "cmd"
	}
	if l := availableShells(); len(l) > 0 {
		return l[0].Name
	}
	return "cmd"
}
