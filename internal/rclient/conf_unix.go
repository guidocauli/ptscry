//go:build !windows

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later

package rclient

import (
	"errors"
	"os"
	"syscall"
)

// checkConfFile: the file must belong to the invoking user (or root) and
// not be writable by others, otherwise someone else could change the key.
func checkConfFile(fi os.FileInfo) error {
	if !fi.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return errors.New("writable by group or others")
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		if int(st.Uid) != os.Geteuid() && st.Uid != 0 {
			return errors.New("owned by another user")
		}
	}
	return nil
}
