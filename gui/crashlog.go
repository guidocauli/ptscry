// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// crashlog.go: a GUI program has no console (on Windows it is built without
// one), so what it prints and how it dies would be lost. Both go to gui.log in
// the configuration directory: a crash leaves its stack trace there.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"runtime/pprof"
	"sync/atomic"
	"time"
)

const maxLogSize = 512 << 10

// startLog redirects stderr and fatal-error output to gui.log and returns its path.
func startLog() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	dir = filepath.Join(dir, "ptscry")
	if os.MkdirAll(dir, 0o700) != nil {
		return ""
	}
	path := filepath.Join(dir, "gui.log")
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if st, err := os.Stat(path); err == nil && st.Size() > maxLogSize {
		flags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return ""
	}
	fmt.Fprintf(f, "\n--- %s ptscry-gui %s ---\n", time.Now().Format("2006-01-02 15:04:05"), guiVersion)
	_ = debug.SetCrashOutput(f, debug.CrashOptions{}) // unrecovered panics and fatal errors, with the stack
	os.Stderr = f
	return path
}

// guard runs f and writes a panic to the log instead of losing it; the
// program stops (a half-working window is worse), but the trace is kept.
func guard(name string, f func()) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "panic in %s: %v\n%s\n", name, r, debug.Stack())
			os.Exit(3)
		}
	}()
	f()
}

// lastFrame is when the window last drew; the watchdog writes every
// goroutine's stack to the log when the window stops drawing (a hang shows up
// on Windows as "Non risponde", with nothing else to go by). The cursor blink
// asks for a frame twice a second, so a long gap means something is stuck,
// unless the window is minimised: the dump is then harmless.
var lastFrame atomic.Int64

func frameDone() { lastFrame.Store(time.Now().UnixNano()) }

func watchdog() {
	if os.Getenv("PTSCRY_GUI_DUMP") != "" { // on demand, for debugging
		go func() {
			for i := 0; i < 3; i++ {
				time.Sleep(10 * time.Second)
				fmt.Fprintln(os.Stderr, "goroutines (PTSCRY_GUI_DUMP):")
				_ = pprof.Lookup("goroutine").WriteTo(os.Stderr, 2)
			}
		}()
	}
	dumped := false
	for range time.Tick(5 * time.Second) {
		lf := lastFrame.Load()
		if lf == 0 {
			continue
		}
		stalled := time.Since(time.Unix(0, lf)) > 15*time.Second
		if stalled && !dumped {
			fmt.Fprintf(os.Stderr, "%s: no frame for 15 s, goroutines:\n", time.Now().Format("15:04:05"))
			_ = pprof.Lookup("goroutine").WriteTo(os.Stderr, 2)
			dumped = true
		} else if !stalled {
			dumped = false
		}
	}
}
