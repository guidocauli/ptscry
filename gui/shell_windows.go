// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// shell_windows.go: the "terminale locale" profile on Windows: PowerShell 7,
// Windows PowerShell or cmd.exe (or the command the profile names) on a
// ConPTY pseudoconsole (Windows 10 1809 or later). Nothing persists: closing
// the tab ends the shell.

//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"

	"ptscry/internal/rclient"
)

// shellCommand is the command line of the shell to start.
func shellCommand(p profile) (string, error) {
	if t := strings.TrimSpace(p.Target); t != "" {
		if strings.ContainsAny(t, "\r\n\x00") {
			return "", errors.New("comando non valido")
		}
		return t, nil
	}
	for _, n := range []string{"pwsh.exe", "powershell.exe"} {
		if path, err := exec.LookPath(n); err == nil {
			return `"` + path + `" -NoLogo`, nil
		}
	}
	if c := os.Getenv("COMSPEC"); c != "" {
		return `"` + c + `"`, nil
	}
	return "cmd.exe", nil
}

func openShell(p profile, rows, cols int) (*gterm, error) {
	if isWine() {
		// Wine's ConPTY gives the shell a console that shows nothing: use plain pipes
		return openPipeShell(p)
	}
	line, err := shellCommand(p)
	if err != nil {
		return nil, err
	}
	var inR, inW, outR, outW windows.Handle
	if err := windows.CreatePipe(&inR, &inW, nil, 0); err != nil {
		return nil, err
	}
	if err := windows.CreatePipe(&outR, &outW, nil, 0); err != nil {
		windows.CloseHandle(inR)
		windows.CloseHandle(inW)
		return nil, err
	}
	size := windows.Coord{X: int16(max(cols, 1)), Y: int16(max(rows, 1))}
	var hpc windows.Handle
	if err := windows.CreatePseudoConsole(size, inR, outW, 0, &hpc); err != nil {
		for _, h := range []windows.Handle{inR, inW, outR, outW} {
			windows.CloseHandle(h)
		}
		fmt.Fprintf(os.Stderr, "ConPTY non disponibile (%v): shell su pipe\n", err)
		return openPipeShell(p) // Windows before 10 1809
	}
	// the pseudoconsole has its own copies of these two ends
	windows.CloseHandle(inR)
	windows.CloseHandle(outW)
	fail := func(err error) (*gterm, error) {
		windows.ClosePseudoConsole(hpc)
		windows.CloseHandle(inW)
		windows.CloseHandle(outR)
		return nil, err
	}

	al, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return fail(err)
	}
	defer al.Delete()
	// the attribute value is the HPCON itself, not its address
	if err := al.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, *(*unsafe.Pointer)(unsafe.Pointer(&hpc)), unsafe.Sizeof(hpc)); err != nil {
		return fail(err)
	}
	si := windows.StartupInfoEx{ProcThreadAttributeList: al.List()}
	si.StartupInfo.Cb = uint32(unsafe.Sizeof(si))
	line16, err := windows.UTF16PtrFromString(line)
	if err != nil {
		return fail(err)
	}
	var dir16 *uint16
	if h, err := os.UserHomeDir(); err == nil {
		dir16, _ = windows.UTF16PtrFromString(h)
	}
	var pi windows.ProcessInformation
	err = windows.CreateProcess(nil, line16, nil, nil, false,
		windows.EXTENDED_STARTUPINFO_PRESENT|windows.CREATE_UNICODE_ENVIRONMENT, nil, dir16, &si.StartupInfo, &pi)
	if err != nil {
		return fail(fmt.Errorf("%s: %v", line, err))
	}
	windows.CloseHandle(pi.Thread)

	in := os.NewFile(uintptr(inW), "conin")
	out := os.NewFile(uintptr(outR), "conout")
	closeCon := sync.OnceFunc(func() { windows.ClosePseudoConsole(hpc) })
	var once sync.Once
	closeAll := func() {
		once.Do(func() {
			_ = windows.TerminateProcess(pi.Process, 1)
			// closing the console can wait for the output to be read: do it aside
			go func() {
				closeCon()
				_ = in.Close()
				_ = out.Close()
			}()
		})
	}
	var res rclient.Result
	got := false
	return &gterm{
		In:  in,
		Out: out,
		Resize: func(r, c int) error {
			return windows.ResizePseudoConsole(hpc, windows.Coord{X: int16(max(c, 1)), Y: int16(max(r, 1))})
		},
		Wait: func() rclient.Result {
			if !got {
				_, _ = windows.WaitForSingleObject(pi.Process, windows.INFINITE)
				var code uint32
				_ = windows.GetExitCodeProcess(pi.Process, &code)
				windows.CloseHandle(pi.Process)
				res, got = rclient.Result{Code: int(code)}, true
				// the shell is gone: let the console flush and end the output stream
				go closeCon()
			}
			return res
		},
		Close: closeAll,
	}, nil
}
