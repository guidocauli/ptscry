// ptscry - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"testing"
)

func TestDetachKey(t *testing.T) {
	var st inState // default prefix is Ctrl-B
	out, d := st.process([]byte("ab\x02d"))
	if string(out) != "ab" || !d {
		t.Fatalf("got %q %v", out, d)
	}
	st = inState{}
	out, d = st.process([]byte("\x02\x02x\x02q"))
	if !bytes.Equal(out, []byte{0x02, 'x', 0x02, 'q'}) || d {
		t.Fatalf("got %q %v", out, d)
	}
	// Ctrl-] is an ordinary key now
	st = inState{}
	if out, d = st.process([]byte("\x1dd")); !bytes.Equal(out, []byte{0x1d, 'd'}) || d {
		t.Fatalf("old prefix must pass through: %q %v", out, d)
	}
	// prefix split across reads
	st = inState{}
	if out, d = st.process([]byte{0x02}); len(out) != 0 || d {
		t.Fatal("prefix alone must be held")
	}
	if _, d = st.process([]byte("d")); !d {
		t.Fatal("detach after split prefix")
	}
	// custom prefix
	st = inState{prefix: 0x1d}
	if out, d = st.process([]byte("a\x1dd")); string(out) != "a" || !d {
		t.Fatalf("custom prefix: %q %v", out, d)
	}
}
