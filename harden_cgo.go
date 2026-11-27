// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// harden_cgo.go: the per-user host becomes non-dumpable before anything else
// runs in the process (before the Go runtime starts).
//
// The monitor starts the host as the user: between the exec and the
// prctl(PR_SET_DUMPABLE, 0) of hardenSelf, a process of the same user could
// attach with ptrace (where kernel.yama.ptrace_scope is 0) and take over the
// host, which holds audit data. An ELF constructor runs right after the
// dynamic loader, so that window shrinks to the loader's own work.

//go:build cgo && linux

package main

/*
#include <fcntl.h>
#include <stdlib.h>
#include <string.h>
#include <sys/prctl.h>
#include <unistd.h>

// argv[1] from /proc/self/cmdline (not the constructor's arguments: only
// glibc passes them).
__attribute__((constructor)) static void ptscry_early_harden(void) {
	char b[512];
	int fd = open("/proc/self/cmdline", O_RDONLY | O_CLOEXEC);
	if (fd < 0) {
		return;
	}
	ssize_t n = read(fd, b, sizeof(b) - 1);
	close(fd);
	if (n <= 0) {
		return;
	}
	b[n] = 0;
	size_t l = strlen(b);
	if ((ssize_t)l + 1 >= n) {
		return;
	}
	const char *a1 = b + l + 1;
	if ((strcmp(a1, "host") == 0 && getenv("PTSCRY_CTL_FD") != NULL) ||
	    strcmp(a1, "launch") == 0 || strcmp(a1, "monitor") == 0) {
		prctl(PR_SET_DUMPABLE, 0, 0, 0, 0);
	}
}
*/
import "C"
