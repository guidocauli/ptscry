// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// checkTrustedBy: the owners accepted (a single-user installation), and the
// rights of the others still refused.
func TestCheckTrustedBy(t *testing.T) {
	d := t.TempDir()
	if err := os.Chmod(d, 0o700); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(d, "ptscry")
	if err := os.WriteFile(f, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	me := uint32(os.Getuid())
	if err := checkTrustedBy(f, true, 0, me); err != nil && !strings.Contains(err.Error(), "writable") {
		// a directory above the temp dir may be another user's on some systems
		t.Logf("above the temp dir: %v", err)
	}
	if err := checkTrustedBy(f, true, me+12345); err == nil || !strings.Contains(err.Error(), "only root or you") {
		t.Errorf("an owner not in the list: %v", err)
	}
	if err := os.Chmod(f, 0o757); err != nil {
		t.Fatal(err)
	}
	if err := checkTrustedBy(f, true, 0, me); err == nil || !strings.Contains(err.Error(), "writable by everyone") {
		t.Errorf("a file writable by everyone: %v", err)
	}
	if err := checkTrusted(filepath.Join(d, "missing"), false); err != nil {
		t.Errorf("missing and not required: %v", err)
	}
}
