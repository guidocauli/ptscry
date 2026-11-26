// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// launch_cgo.go: the session launcher (cgo build).
//
// A launcher is a short-lived ROOT process, one per session, started by the
// monitor. It plays the part sshd's privileged session process plays:
//
//  1. runs the PAM account and session stacks for the user (access rules such
//     as pam_access/pam_nologin/sssd HBAC, then limits, pam_loginuid,
//     pam_systemd ...: whatever the administrator configured in
//     /etc/pam.d/<pam_service>),
//  2. forks the user's shell on the pseudoterminal: new session, controlling
//     terminal, audit login uid, supplementary groups, uid/gid, umask,
//  3. stays around as the shell's parent: reports its exit status to the
//     monitor, closes the PAM session, and kills the shell if the monitor
//     goes away.
//
// PAM is loaded with dlopen: no libpam headers are needed to build, and a
// host without libpam fails cleanly at start-up instead of at link time.
// The fork is done in C because Go's own fork path restores the
// RLIMIT_NOFILE it found at start-up, which would undo what pam_limits set.

//go:build cgo && linux

package main

/*
#cgo LDFLAGS: -ldl
#define _GNU_SOURCE
#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <grp.h>
#include <signal.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <unistd.h>

// ---- the part of the Linux-PAM ABI we use (declared here: no headers needed)
typedef struct pam_handle pam_handle_t;
struct pam_message  { int msg_style; const char *msg; };
struct pam_response { char *resp; int resp_retcode; };
struct pam_conv {
	int (*conv)(int, const struct pam_message **, struct pam_response **, void *);
	void *appdata_ptr;
};
#define P_SUCCESS    0
#define P_CONV_ERR   19
#define P_SILENT     0x8000
#define P_ESTABLISH  0x0002
#define P_DELETE     0x0004
#define P_ITEM_TTY   3

static void *pamlib;
static int  (*f_start)(const char *, const char *, const struct pam_conv *, pam_handle_t **);
static int  (*f_end)(pam_handle_t *, int);
static int  (*f_acct)(pam_handle_t *, int);
static int  (*f_setcred)(pam_handle_t *, int);
static int  (*f_open)(pam_handle_t *, int);
static int  (*f_close)(pam_handle_t *, int);
static int  (*f_setitem)(pam_handle_t *, int, const void *);
static char **(*f_getenvlist)(pam_handle_t *);
static const char *(*f_strerror)(pam_handle_t *, int);

static int ptscry_pam_load(void) {
	if (pamlib) return 0;
	void *h = dlopen("libpam.so.0", RTLD_NOW | RTLD_LOCAL);
	if (!h) return -1;
	f_start      = dlsym(h, "pam_start");
	f_end        = dlsym(h, "pam_end");
	f_acct       = dlsym(h, "pam_acct_mgmt");
	f_setcred    = dlsym(h, "pam_setcred");
	f_open       = dlsym(h, "pam_open_session");
	f_close      = dlsym(h, "pam_close_session");
	f_setitem    = dlsym(h, "pam_set_item");
	f_getenvlist = dlsym(h, "pam_getenvlist");
	f_strerror   = dlsym(h, "pam_strerror");
	if (!f_start || !f_end || !f_acct || !f_setcred || !f_open || !f_close ||
	    !f_setitem || !f_getenvlist || !f_strerror) { dlclose(h); return -2; }
	pamlib = h;
	return 0;
}

// Nothing in these stacks may ask the user anything.
static int ptscry_conv(int n, const struct pam_message **m, struct pam_response **r, void *d) {
	(void)n; (void)m; (void)d;
	*r = NULL;
	return P_CONV_ERR;
}
static struct pam_conv conv_none = { ptscry_conv, NULL };

static int ptscry_pam_begin(const char *svc, const char *user, const char *tty, pam_handle_t **h) {
	int rc = f_start(svc, user, &conv_none, h);
	if (rc != P_SUCCESS) return rc;
	if (tty && *tty) {
		rc = f_setitem(*h, P_ITEM_TTY, tty);
		if (rc != P_SUCCESS) { f_end(*h, rc); *h = NULL; }
	}
	return rc;
}
static int ptscry_pam_acct(pam_handle_t *h)    { return f_acct(h, P_SILENT); }
static int ptscry_pam_cred(pam_handle_t *h)    { return f_setcred(h, P_SILENT | P_ESTABLISH); }
static int ptscry_pam_open(pam_handle_t *h)    { return f_open(h, P_SILENT); }
static int ptscry_pam_close(pam_handle_t *h)   { int rc = f_close(h, P_SILENT); f_setcred(h, P_SILENT | P_DELETE); return rc; }
static void ptscry_pam_end(pam_handle_t *h, int rc) { f_end(h, rc); }
static const char *ptscry_pam_err(pam_handle_t *h, int rc) { return f_strerror(h, rc); }
static char **ptscry_pam_env(pam_handle_t *h)  { return f_getenvlist(h); }

// ---- fork + exec of the user's process, entirely in C (see the header comment)
//
// Returns the child's pid, or -errno if anything up to and including the
// exec failed. In the child only async-signal-safe calls are made, and raw
// system calls are used for the credential changes.
extern char **environ;

static int ptscry_spawn(int slave, unsigned uid, unsigned gid, const unsigned *groups, int ngroups,
                        const char *dir, char **argv, char **envp, unsigned loginuid) {
	int ep[2];
	if (pipe2(ep, O_CLOEXEC) < 0) return -errno;
	pid_t pid = fork();
	if (pid < 0) { int e = errno; close(ep[0]); close(ep[1]); return -e; }
	if (pid == 0) {
		int err = 0;
		struct sigaction sa;
		sigset_t none;
		memset(&sa, 0, sizeof sa);
		sa.sa_handler = SIG_DFL;
		for (int s = 1; s < 65; s++) { if (s != SIGKILL && s != SIGSTOP) sigaction(s, &sa, NULL); }
		sigemptyset(&none);
		sigprocmask(SIG_SETMASK, &none, NULL);

		if (setsid() < 0) { err = errno; goto fail; }
		// 0 = do not steal: the terminal must be a fresh one with no session.
		if (ioctl(slave, TIOCSCTTY, 0) < 0) { err = errno; goto fail; }
		if (dup2(slave, 0) < 0 || dup2(slave, 1) < 0 || dup2(slave, 2) < 0) { err = errno; goto fail; }
		if (slave > 2) close(slave);
		// Whatever else we hold (control socket, descriptors of PAM modules) must not reach the shell. The flag 4 is CLOSE_RANGE_CLOEXEC.
		if (syscall(SYS_close_range, 3, ~0U, 4) < 0) {
			long max = sysconf(_SC_OPEN_MAX);
			if (max > 65536) max = 65536;
			for (long fd = 3; fd < max; fd++) fcntl((int)fd, F_SETFD, FD_CLOEXEC);
		}

		// Audit login uid: set once, by us, before privileges are dropped. A kernel
		// without audit support has no such file; anything else is fatal.
		if (loginuid != (unsigned)-1) {
			int fd = open("/proc/self/loginuid", O_RDONLY | O_CLOEXEC);
			if (fd >= 0) {
				char cur[32]; ssize_t n = read(fd, cur, sizeof cur - 1);
				close(fd);
				if (n < 0) { err = errno; goto fail; }
				cur[n] = 0;
				// parsed by hand: strtoul would tie the binary to glibc 2.38
				// (__isoc23_strtoul) and it need not be async-signal-safe
				unsigned long curv = 0;
				for (char *q = cur; *q >= '0' && *q <= '9'; q++) curv = curv * 10 + (unsigned long)(*q - '0');
				if (curv != loginuid) {
					char buf[32]; int l = 0; unsigned v = loginuid; char tmp[16]; int t = 0;
					do { tmp[t++] = '0' + v % 10; v /= 10; } while (v);
					while (t) buf[l++] = tmp[--t];
					fd = open("/proc/self/loginuid", O_WRONLY | O_CLOEXEC);
					if (fd < 0 || write(fd, buf, l) != l) { err = errno ? errno : EIO; goto fail; }
					close(fd);
				}
			} else if (errno != ENOENT) { err = errno; goto fail; }
		}

		if (syscall(SYS_setgroups, ngroups, groups) < 0) { err = errno; goto fail; }
		if (syscall(SYS_setresgid, gid, gid, gid) < 0)   { err = errno; goto fail; }
		if (syscall(SYS_setresuid, uid, uid, uid) < 0)   { err = errno; goto fail; }
		// Make certain root cannot be regained.
		if (uid != 0 && syscall(SYS_setresuid, 0, 0, 0) == 0) { err = EPERM; goto fail; }

		umask(022);
		if (dir && chdir(dir) < 0) { if (chdir("/") < 0) { err = errno; goto fail; } }
		environ = envp;
		execvp(argv[0], argv);
		err = errno;
	fail:
		(void)!write(ep[1], &err, sizeof err);
		_exit(127);
	}
	close(ep[1]);
	int e = 0; ssize_t n;
	do { n = read(ep[0], &e, sizeof e); } while (n < 0 && errno == EINTR);
	close(ep[0]);
	if (n == (ssize_t)sizeof e) { int st; waitpid(pid, &st, 0); return -e; }
	return pid;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

// launcherAvailable tells the rest of the program that this build can run
// session launchers (and therefore PAM sessions).
const launcherAvailable = true

// pamAvailable checks that libpam can be loaded.
func pamAvailable() error {
	if C.ptscry_pam_load() != 0 {
		return errors.New("libpam.so.0 cannot be loaded")
	}
	return nil
}

type pamHandle struct{ h *C.pam_handle_t }

func (p *pamHandle) errString(rc C.int) string {
	return C.GoString(C.ptscry_pam_err(p.h, rc))
}

// pamStage names the step that failed, for the audit log.
type pamError struct {
	Stage string
	Msg   string
}

func (e *pamError) Error() string { return e.Stage + ": " + e.Msg }

// pamOpen runs account management, credentials and session opening, and
// returns the handle (to close later) and the environment PAM wants set.
func pamOpen(service, user, tty string) (*pamHandle, []string, error) {
	if err := pamAvailable(); err != nil {
		return nil, nil, &pamError{"load", err.Error()}
	}
	cs, cu, ct := C.CString(service), C.CString(user), C.CString(tty)
	defer C.free(unsafe.Pointer(cs))
	defer C.free(unsafe.Pointer(cu))
	defer C.free(unsafe.Pointer(ct))
	p := &pamHandle{}
	if rc := C.ptscry_pam_begin(cs, cu, ct, &p.h); rc != C.P_SUCCESS {
		return nil, nil, &pamError{"start", fmt.Sprintf("pam_start failed (%d)", int(rc))}
	}
	step := func(stage string, rc C.int) error {
		if rc != C.P_SUCCESS {
			msg := p.errString(rc)
			C.ptscry_pam_end(p.h, rc)
			return &pamError{stage, msg}
		}
		return nil
	}
	if err := step("account", C.ptscry_pam_acct(p.h)); err != nil {
		return nil, nil, err
	}
	if err := step("credentials", C.ptscry_pam_cred(p.h)); err != nil {
		return nil, nil, err
	}
	if rc := C.ptscry_pam_open(p.h); rc != C.P_SUCCESS {
		msg := p.errString(rc)
		C.ptscry_pam_cred(p.h) // best effort
		C.ptscry_pam_end(p.h, rc)
		return nil, nil, &pamError{"session", msg}
	}
	var env []string
	if lst := C.ptscry_pam_env(p.h); lst != nil {
		for i := 0; ; i++ {
			e := *(**C.char)(unsafe.Pointer(uintptr(unsafe.Pointer(lst)) + uintptr(i)*unsafe.Sizeof(uintptr(0))))
			if e == nil {
				break
			}
			env = append(env, C.GoString(e))
			C.free(unsafe.Pointer(e))
		}
		C.free(unsafe.Pointer(lst))
	}
	return p, env, nil
}

func (p *pamHandle) close() {
	rc := C.ptscry_pam_close(p.h)
	C.ptscry_pam_end(p.h, rc)
	p.h = nil
}

// spawnUser forks the user's process on the terminal whose slave is slaveFD.
// It returns the pid of the new session leader.
func spawnUser(slaveFD int, uid, gid uint32, groups []uint32, dir string, argv, env []string, loginuid uint32) (int, error) {
	cArray := func(ss []string) (**C.char, func()) {
		n := len(ss)
		arr := (**C.char)(C.calloc(C.size_t(n+1), C.size_t(unsafe.Sizeof(uintptr(0)))))
		strs := make([]*C.char, n)
		for i, s := range ss {
			strs[i] = C.CString(s)
			*(**C.char)(unsafe.Pointer(uintptr(unsafe.Pointer(arr)) + uintptr(i)*unsafe.Sizeof(uintptr(0)))) = strs[i]
		}
		return arr, func() {
			for _, c := range strs {
				C.free(unsafe.Pointer(c))
			}
			C.free(unsafe.Pointer(arr))
		}
	}
	cargv, freeArgv := cArray(argv)
	defer freeArgv()
	cenv, freeEnv := cArray(env)
	defer freeEnv()
	cdir := C.CString(dir)
	defer C.free(unsafe.Pointer(cdir))
	var cg *C.uint
	if len(groups) > 0 {
		cg = (*C.uint)(C.calloc(C.size_t(len(groups)), 4))
		defer C.free(unsafe.Pointer(cg))
		for i, g := range groups {
			*(*C.uint)(unsafe.Pointer(uintptr(unsafe.Pointer(cg)) + uintptr(i)*4)) = C.uint(g)
		}
	}
	pid := C.ptscry_spawn(C.int(slaveFD), C.uint(uid), C.uint(gid), cg, C.int(len(groups)), cdir, cargv, cenv, C.uint(loginuid))
	if pid < 0 {
		return 0, fmt.Errorf("starting the session process: errno %d", int(-pid))
	}
	return int(pid), nil
}
