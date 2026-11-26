//go:build linux

package main

import "testing"

func TestReviewStripOutputs(t *testing.T) {
	for _, s := range []string{"\x1b]7\x1b]7799;\a799;session;evil\a", "\x9d7799;session;evil\x07"} {
		t.Logf("%q -> %q", s, cleanReplay([]byte(s), false))
	}
}
