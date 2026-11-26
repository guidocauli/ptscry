// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// fe_seccomp_amd64_install.go: the front-end's seccomp-bpf filter.
//
// Installed once the listener is bound and the configuration is read, for all
// threads (TSYNC). It is a deny-list on purpose: an allow-list of everything the
// Go runtime and the TLS-free SSH stack may call would break with every Go
// release. What it takes away is what a compromised front-end would need:
//   - exec of any program (no shell, no tools), ptrace, process_vm_*, bpf, mounts,
//     namespaces, kernel modules, keyrings, clocks...: the process is killed;
//   - creating, changing or removing files, and opening one for writing: EPERM;
//   - sockets other than unix, IPv4, IPv6 (no raw, packet, netlink-route tricks, vsock...).
// It does not stop the front-end from connecting to arbitrary network addresses:
// use a firewall rule on its uid for that (see the manual).

//go:build linux && amd64

package main

import (
	"fmt"
	"sort"
	"syscall"
	"unsafe"
)

const (
	bpfLD   = 0x00
	bpfJMP  = 0x05
	bpfRET  = 0x06
	bpfW    = 0x00
	bpfABS  = 0x20
	bpfJEQ  = 0x10
	bpfJSET = 0x40
	bpfK    = 0x00

	retKillProcess = 0x80000000
	retErrno       = 0x00050000
	retAllow       = 0x7fff0000

	seccompSetModeFilter = 1
	seccompFilterTSYNC   = 1

	offNR   = 0
	offArch = 4
	offArg0 = 16
)

type sockFilter struct {
	Code uint16
	Jt   uint8
	Jf   uint8
	K    uint32
}

type sockFprog struct {
	Len    uint16
	_      [6]byte
	Filter *sockFilter
}

type bpfProg struct{ ins []sockFilter }

func (p *bpfProg) add(code uint16, k uint32) int {
	p.ins = append(p.ins, sockFilter{Code: code, K: k})
	return len(p.ins) - 1
}

// jeqRet: if A == nr return `ret`, else fall through.
func (p *bpfProg) jeqRet(nr, ret uint32) {
	p.ins = append(p.ins, sockFilter{Code: bpfJMP | bpfJEQ | bpfK, Jt: 0, Jf: 1, K: nr})
	p.add(bpfRET|bpfK, ret)
}

func sortedNums(m map[string]uint32) []uint32 {
	out := make([]uint32, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// buildSeccomp assembles the program (separate from installing it so that it can be inspected).
func buildSeccomp() []sockFilter {
	p := &bpfProg{}
	// wrong architecture (e.g. 32-bit syscalls on a 64-bit kernel): kill.
	p.add(bpfLD|bpfW|bpfABS, offArch)
	p.ins = append(p.ins, sockFilter{Code: bpfJMP | bpfJEQ | bpfK, Jt: 1, Jf: 0, K: seccompArch})
	p.add(bpfRET|bpfK, retKillProcess)
	p.add(bpfLD|bpfW|bpfABS, offNR)

	for _, nr := range sortedNums(seccompKill) {
		p.jeqRet(nr, retKillProcess)
	}
	for _, nr := range sortedNums(seccompErrno) {
		p.jeqRet(nr, retErrno|uint32(syscall.EPERM))
	}
	for _, nr := range sortedNums(seccompEnosys) {
		p.jeqRet(nr, retErrno|uint32(syscall.ENOSYS))
	}

	// socket(domain, ...): only AF_UNIX(1), AF_INET(2), AF_INET6(10).
	p.ins = append(p.ins, sockFilter{Code: bpfJMP | bpfJEQ | bpfK, Jt: 0, Jf: 9, K: sysSocket})
	p.add(bpfLD|bpfW|bpfABS, offArg0)
	p.ins = append(p.ins, sockFilter{Code: bpfJMP | bpfJEQ | bpfK, Jt: 0, Jf: 1, K: 1})
	p.add(bpfRET|bpfK, retAllow)
	p.ins = append(p.ins, sockFilter{Code: bpfJMP | bpfJEQ | bpfK, Jt: 0, Jf: 1, K: 2})
	p.add(bpfRET|bpfK, retAllow)
	p.ins = append(p.ins, sockFilter{Code: bpfJMP | bpfJEQ | bpfK, Jt: 0, Jf: 1, K: 10})
	p.add(bpfRET|bpfK, retAllow)
	p.add(bpfRET|bpfK, retErrno|uint32(syscall.EAFNOSUPPORT))
	// reload nr for the checks below (the jump above lands here when nr != socket)
	p.add(bpfLD|bpfW|bpfABS, offNR)

	// openat(dirfd, path, flags, ...): read-only opens only.
	// O_ACCMODE | O_CREAT | O_TRUNC | O_APPEND | __O_TMPFILE
	const writeFlags = 0x3 | 0x40 | 0x200 | 0x400 | 0x400000
	p.ins = append(p.ins, sockFilter{Code: bpfJMP | bpfJEQ | bpfK, Jt: 0, Jf: 4, K: sysOpenat})
	p.add(bpfLD|bpfW|bpfABS, offArg0+8*2)
	p.ins = append(p.ins, sockFilter{Code: bpfJMP | bpfJSET | bpfK, Jt: 0, Jf: 1, K: writeFlags})
	p.add(bpfRET|bpfK, retErrno|uint32(syscall.EACCES))
	p.add(bpfRET|bpfK, retAllow)
	p.add(bpfLD|bpfW|bpfABS, offNR)

	// clone(flags, ...): no new namespaces.
	const nsFlags = 0x00020000 | 0x02000000 | 0x04000000 | 0x08000000 | 0x10000000 | 0x20000000 | 0x40000000
	p.ins = append(p.ins, sockFilter{Code: bpfJMP | bpfJEQ | bpfK, Jt: 0, Jf: 4, K: sysClone})
	p.add(bpfLD|bpfW|bpfABS, offArg0)
	p.ins = append(p.ins, sockFilter{Code: bpfJMP | bpfJSET | bpfK, Jt: 0, Jf: 1, K: nsFlags})
	p.add(bpfRET|bpfK, retKillProcess)
	p.add(bpfRET|bpfK, retAllow)

	p.add(bpfRET|bpfK, retAllow)
	return p.ins
}

// installSeccomp applies the filter to every thread. It reports whether a
// filter is now active.
func installSeccomp() (bool, error) {
	ins := buildSeccomp()
	if len(ins) > 4096 {
		return false, fmt.Errorf("seccomp program too long (%d)", len(ins))
	}
	prog := sockFprog{Len: uint16(len(ins)), Filter: &ins[0]}
	if _, _, e := syscall.RawSyscall(317, seccompSetModeFilter, seccompFilterTSYNC, uintptr(unsafe.Pointer(&prog))); e != 0 {
		return false, fmt.Errorf("seccomp: %v", e)
	}
	return true, nil
}
