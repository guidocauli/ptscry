//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// ident.go: user identity resolution through NSS (files, SSSD/FreeIPA, ...).
//
// Needs a cgo build (os/user then calls libc, which loads the NSS modules).
// A static CGO_ENABLED=0 binary only reads /etc/passwd and /etc/group and
// cannot see FreeIPA users: `ptscry check` warns about it.

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type identity struct {
	u      *user.User
	gid    uint32
	groups []string // group names
	gids   []uint32
}

// lookupIdentity resolves a uid, with a deadline so that a stuck SSSD can
// never hang the monitor (a timeout is a denial).
func lookupIdentity(uid int) (*identity, error) {
	type res struct {
		id  *identity
		err error
	}
	ch := make(chan res, 1)
	go func() {
		u, err := user.LookupId(strconv.Itoa(uid))
		if err != nil {
			ch <- res{nil, err}
			return
		}
		g, err := strconv.ParseUint(u.Gid, 10, 32)
		if err != nil {
			ch <- res{nil, err}
			return
		}
		ids, err := u.GroupIds()
		if err != nil {
			ch <- res{nil, err}
			return
		}
		id := &identity{u: u, gid: uint32(g)}
		for _, s := range ids {
			n, err := strconv.ParseUint(s, 10, 32)
			if err != nil {
				continue
			}
			id.gids = append(id.gids, uint32(n))
			if gr, err := user.LookupGroupId(s); err == nil {
				id.groups = append(id.groups, gr.Name)
			}
		}
		ch <- res{id, nil}
	}()
	select {
	case r := <-ch:
		return r.id, r.err
	case <-time.After(5 * time.Second):
		return nil, errors.New("user lookup timed out")
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// lookupShell returns the login shell of uid through `getent passwd UID`
// (numeric argument: nothing to inject). Empty when unknown.
func lookupShell(uid int) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	getent := "/usr/bin/getent"
	if _, err := os.Stat(getent); err != nil {
		getent = "/bin/getent"
	}
	cmd := exec.CommandContext(ctx, getent, "passwd", strconv.Itoa(uid))
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	cmd.Dir = "/"
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	f := strings.Split(strings.TrimSpace(string(out)), ":")
	if len(f) < 7 || !strings.HasPrefix(f[6], "/") {
		return ""
	}
	return f[6]
}

// interactiveShell reports whether a shell path can run an interactive session.
func interactiveShell(sh string) bool {
	switch filepath.Base(sh) {
	case "nologin", "false", "true", "sync", "halt", "shutdown":
		return false
	}
	return strings.HasPrefix(sh, "/")
}

// lookupUserName returns the uid (as a string) of a user name through NSS.
func lookupUserName(name string) (string, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return "", err
	}
	return u.Uid, nil
}

// lookupGID resolves a group name (NSS, with a deadline: a stuck SSSD must not
// hang the audit).
func lookupGID(name string) (int, error) {
	type res struct {
		gid int
		err error
	}
	ch := make(chan res, 1)
	go func() {
		g, err := user.LookupGroup(name)
		if err != nil {
			ch <- res{0, err}
			return
		}
		n, err := strconv.Atoi(g.Gid)
		ch <- res{n, err}
	}()
	select {
	case r := <-ch:
		return r.gid, r.err
	case <-time.After(3 * time.Second):
		return 0, errors.New("group lookup timed out")
	}
}
