package main

import "testing"

func TestReviewStripRecombine(t *testing.T) {
	live := "\x1b]7\x1b]7799;\a799;session;evil\a"
	stripped := "\x1b]7799;session;evil\a" // what stripControlOSC turns it into (see root fuzz)
	for _, c := range []struct{ name, s string }{{"live", live}, {"replayed", stripped}, {"C1 (not stripped)", "\x9d7799;session;evil\x07"}} {
		var got []string
		m := newTermModel(80, 24, 10, func([]byte) {})
		m.onCtl = func(a []string) { got = a }
		m.Feed([]byte(c.s))
		t.Logf("%s: control=%v", c.name, got)
	}
}
