// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux && amd64

package main

import (
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// Every number in the tables must be the syscall it is named after.
func TestSeccompTables(t *testing.T) {
	b, err := os.ReadFile("/usr/include/x86_64-linux-gnu/asm/unistd_64.h")
	if err != nil {
		t.Skip("kernel header not available")
	}
	re := regexp.MustCompile(`#define __NR_(\w+)\s+(\d+)`)
	hdr := map[string]uint32{}
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		n, _ := strconv.Atoi(m[2])
		hdr[m[1]] = uint32(n)
	}
	for _, tab := range []map[string]uint32{seccompKill, seccompErrno, seccompEnosys} {
		for name, nr := range tab {
			if hdr[name] != nr {
				t.Errorf("%s: table has %d, kernel header %d", name, nr, hdr[name])
			}
		}
	}
	for name, nr := range map[string]uint32{"socket": sysSocket, "openat": sysOpenat, "clone": sysClone} {
		if hdr[name] != nr {
			t.Errorf("%s: %d vs %d", name, nr, hdr[name])
		}
	}
	if len(buildSeccomp()) > 600 {
		t.Errorf("program unexpectedly long: %d", len(buildSeccomp()))
	}
}

// TestSeccompChild runs in a re-executed copy of the test binary, with the
// filter installed on itself; the parent test inspects how it ended.
func TestSeccompChild(t *testing.T) {
	mode := os.Getenv("PTSCRY_SECCOMP_CHILD")
	if mode == "" {
		t.Skip("helper")
	}
	dir := os.Getenv("PTSCRY_SECCOMP_DIR")
	have6 := false // IPv6 may be disabled in this kernel
	if fd, err := syscall.Socket(syscall.AF_INET6, syscall.SOCK_STREAM, 0); err == nil {
		have6 = true
		syscall.Close(fd)
	}
	if on, err := installSeccomp(); !on || err != nil {
		t.Fatalf("install: %v %v", on, err)
	}
	fail := func(m string) { println("FAIL " + m); os.Exit(3) }
	switch mode {
	case "allowed":
		// what the front-end really does
		if _, err := os.ReadFile("/etc/passwd"); err != nil {
			fail("read file: " + err.Error())
		}
		for _, af := range []int{syscall.AF_UNIX, syscall.AF_INET, syscall.AF_INET6} {
			if af == syscall.AF_INET6 && !have6 {
				continue
			}
			fd, err := syscall.Socket(af, syscall.SOCK_STREAM, 0)
			if err != nil {
				fail("socket: " + err.Error())
			}
			syscall.Close(fd)
		}
		done := make(chan int)
		for i := 0; i < 8; i++ { // new threads/goroutines keep working
			go func(i int) { done <- i }(i)
		}
		for i := 0; i < 8; i++ {
			<-done
		}
	case "nowrite":
		if err := os.WriteFile(dir+"/x", []byte("x"), 0o600); err == nil {
			fail("created a file")
		}
		if _, err := os.OpenFile("/etc/passwd", os.O_RDWR, 0); err == nil {
			fail("opened for writing")
		}
		if err := os.Mkdir(dir+"/d", 0o700); err == nil {
			fail("mkdir")
		}
		if err := os.Remove("/etc/hostname"); err == nil {
			fail("unlink")
		}
		if err := os.Chmod("/etc/passwd", 0o777); err == nil {
			fail("chmod")
		}
		if _, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, 0); err == nil {
			fail("raw socket")
		}
		if _, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW, 0); err == nil {
			fail("netlink socket")
		}
	case "exec":
		_ = syscall.Exec("/bin/true", []string{"true"}, nil)
		fail("exec returned")
	case "ptrace":
		syscall.RawSyscall(syscall.SYS_PTRACE, 0, 0, 0)
		fail("ptrace returned")
	case "unshare":
		syscall.RawSyscall(syscall.SYS_UNSHARE, 0x10000000, 0, 0)
		fail("unshare returned")
	}
}

func TestSeccompBehaviour(t *testing.T) {
	if os.Getenv("PTSCRY_SECCOMP_CHILD") != "" {
		t.Skip()
	}
	run := func(mode string) *exec.ExitError {
		cmd := exec.Command(os.Args[0], "-test.run=^TestSeccompChild$", "-test.v")
		cmd.Env = append(os.Environ(), "PTSCRY_SECCOMP_CHILD="+mode, "PTSCRY_SECCOMP_DIR="+t.TempDir())
		out, err := cmd.CombinedOutput()
		if err == nil {
			return nil
		}
		if strings.Contains(string(out), "FAIL ") {
			t.Fatalf("%s: %s", mode, out)
		}
		ee, _ := err.(*exec.ExitError)
		return ee
	}
	for _, ok := range []string{"allowed", "nowrite"} {
		if ee := run(ok); ee != nil {
			t.Errorf("%s: child failed: %v", ok, ee)
		}
	}
	for _, bad := range []string{"exec", "ptrace", "unshare"} {
		ee := run(bad)
		if ee == nil {
			t.Errorf("%s: child survived", bad)
			continue
		}
		if ws, ok := ee.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGSYS {
			t.Errorf("%s: want SIGSYS, got %v", bad, ee)
		}
	}
}
