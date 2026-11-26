//go:build windows

// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later

package rclient

import "os"

// checkConfFile: %APPDATA% is private to the user; there are no Unix
// permission bits to check.
func checkConfFile(fi os.FileInfo) error { return nil }
