// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// filepick.go: the system's "open file" dialog, through what each system has:
// the portal / zenity / kdialog on Linux, osascript on macOS, PowerShell on Windows.

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

var errPickCancelled = errors.New("annullato")

// pickFile opens the system dialog and returns the chosen path. It blocks
// until the person chooses or cancels: call it from a goroutine.
func pickFile(title, startDir string) (string, error) {
	if startDir == "" {
		startDir, _ = os.UserHomeDir()
	}
	switch runtime.GOOS {
	case "windows":
		return pickWindows(title, startDir)
	case "darwin":
		return pickMac(title, startDir)
	default:
		return pickLinux(title, startDir)
	}
}

func runPicker(name string, args ...string) (string, bool, error) {
	if _, err := exec.LookPath(name); err != nil {
		return "", false, nil // not installed: try the next
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	p := strings.TrimRight(out.String(), "\r\n")
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && p == "" && ee.ExitCode() == 1 {
			return "", true, errPickCancelled // zenity/kdialog: 1 = cancelled
		}
		return "", true, errors.New(strings.TrimSpace(name + ": " + errb.String()))
	}
	if p == "" {
		return "", true, errPickCancelled
	}
	return p, true, nil
}

func pickLinux(title, dir string) (string, error) {
	if !strings.HasSuffix(dir, "/") {
		dir += "/"
	}
	// the desktop's own choice first: GNOME/GTK, then KDE, then the others
	tries := [][]string{
		{"zenity", "--file-selection", "--title=" + title, "--filename=" + dir},
		{"kdialog", "--title", title, "--getopenfilename", dir},
		{"yad", "--file", "--title=" + title, "--filename=" + dir},
		{"qarma", "--file-selection", "--title=" + title, "--filename=" + dir},
	}
	for _, t := range tries {
		p, used, err := runPicker(t[0], t[1:]...)
		if used {
			return p, err
		}
	}
	return "", errors.New("nessun selettore di file di sistema (installa zenity o kdialog) oppure scrivi il percorso a mano")
}

func pickMac(title, dir string) (string, error) {
	script := `POSIX path of (choose file with prompt "` + strings.NewReplacer(`\`, ``, `"`, ``).Replace(title) + `"` +
		` default location (POSIX file "` + strings.NewReplacer(`\`, ``, `"`, ``).Replace(dir) + `"))`
	p, used, err := runPicker("osascript", "-e", script)
	if !used {
		return "", errors.New("osascript non trovato")
	}
	return p, err
}

func pickWindows(title, dir string) (string, error) {
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	ps := "Add-Type -AssemblyName System.Windows.Forms; " +
		"$d = New-Object System.Windows.Forms.OpenFileDialog; " +
		"$d.Title = " + q(title) + "; $d.InitialDirectory = " + q(dir) + "; $d.CheckFileExists = $true; " +
		"$d.Filter = 'Chiavi e file (*.*)|*.*'; " +
		"if ($d.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) { [Console]::Out.Write($d.FileName) }"
	p, used, err := runPicker("powershell.exe", "-NoProfile", "-STA", "-WindowStyle", "Hidden", "-Command", ps)
	if !used {
		return "", errors.New("PowerShell non trovato")
	}
	return p, err
}
