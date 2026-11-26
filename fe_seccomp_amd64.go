// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// fe_seccomp_amd64.go: syscall numbers for the front-end's seccomp filter
// (x86-64). TestSeccompTables checks every number against the kernel header.

//go:build linux && amd64

package main

const seccompArch = 0xC000003E // AUDIT_ARCH_X86_64

const (
	sysSocket = 41
	sysOpenat = 257
	sysClone  = 56
)

// seccompKill: the front-end has no business making these calls at all; one
// of them means it has been taken over, so the process is killed.
var seccompKill = map[string]uint32{
	"execve": 59, "execveat": 322, "ptrace": 101, "process_vm_readv": 310, "process_vm_writev": 311,
	"mount": 165, "umount2": 166, "pivot_root": 155, "chroot": 161, "reboot": 169,
	"kexec_load": 246, "kexec_file_load": 320, "init_module": 175, "finit_module": 313, "delete_module": 176,
	"swapon": 167, "swapoff": 168, "unshare": 272, "setns": 308, "bpf": 321, "perf_event_open": 298,
	"userfaultfd": 323, "keyctl": 250, "add_key": 248, "request_key": 249,
	"open_by_handle_at": 304, "name_to_handle_at": 303, "acct": 163, "iopl": 172, "ioperm": 173,
	"quotactl": 179, "settimeofday": 164, "clock_settime": 227, "clock_adjtime": 305, "adjtimex": 159,
	"sethostname": 170, "setdomainname": 171, "mount_setattr": 442, "move_mount": 429, "open_tree": 428,
	"fsopen": 430, "fsconfig": 431, "fsmount": 432, "fspick": 433, "pidfd_getfd": 438, "kcmp": 312,
	"mbind": 237, "set_mempolicy": 238, "migrate_pages": 256, "move_pages": 279,
}

// seccompErrno: calls that fail with EPERM instead (the front-end writes no
// files: it reads its configuration at start and talks to sockets).
var seccompErrno = map[string]uint32{
	"unlink": 87, "unlinkat": 263, "rename": 82, "renameat": 264, "renameat2": 316,
	"mkdir": 83, "mkdirat": 258, "rmdir": 84, "link": 86, "linkat": 265, "symlink": 88, "symlinkat": 266,
	"chmod": 90, "fchmod": 91, "fchmodat": 268, "chown": 92, "fchown": 93, "lchown": 94, "fchownat": 260,
	"truncate": 76, "mknod": 133, "mknodat": 259, "creat": 85, "open": 2,
	"setxattr": 188, "lsetxattr": 189, "fsetxattr": 190, "removexattr": 197, "lremovexattr": 198, "fremovexattr": 199,
}

// seccompEnosys: calls whose arguments live in memory, which a filter cannot
// inspect; the runtime falls back to the older call.
var seccompEnosys = map[string]uint32{"openat2": 437, "clone3": 435}
