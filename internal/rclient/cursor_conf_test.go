// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later

package rclient

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfCursor(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "client.conf")
	if err := os.WriteFile(p, []byte("cursor = Steady\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cc, warn := ReadConf([]string{p}, false)
	if cc.Cursor != "steady" || len(warn) != 0 {
		t.Fatalf("got %q %v", cc.Cursor, warn)
	}
	_ = os.WriteFile(p, []byte("cursor = wobble\n"), 0o600)
	cc, warn = ReadConf([]string{p}, false)
	if cc.Cursor != "" || len(warn) != 1 {
		t.Fatalf("bad value: got %q %v", cc.Cursor, warn)
	}
}
