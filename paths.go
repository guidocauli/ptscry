//go:build linux

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// paths.go: socket, audit and recording locations.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func resolveSocketDir(cfg *config, uid int) string {
	if cfg.SocketDir != "" {
		return strings.ReplaceAll(cfg.SocketDir, "%u", strconv.Itoa(uid))
	}
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" && uid != 0 {
		return filepath.Join(x, "ptscry")
	}
	if uid == 0 {
		return "/run/ptscry/0"
	}
	return fmt.Sprintf("/tmp/ptscry-%d", uid)
}

// ensureSecureDir creates dir (0700) and verifies it is a real directory owned
// by uid and not accessible to others, so nobody can pre-create or swap it.
func ensureSecureDir(dir string, uid int) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != uid {
		return fmt.Errorf("%s is not owned by uid %d", dir, uid)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s has unsafe permissions %v (expected 0700)", dir, fi.Mode().Perm())
	}
	return nil
}

func stateDir(uid int, home string) string {
	if uid == 0 {
		return "/var/log/ptscry"
	}
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "ptscry")
	}
	return filepath.Join(home, ".local", "state", "ptscry")
}

func defaultAuditPath(uid int, home string) string {
	return filepath.Join(stateDir(uid, home), "audit.log")
}

func defaultRecordDir(uid int, home string) string {
	return filepath.Join(stateDir(uid, home), "rec")
}
