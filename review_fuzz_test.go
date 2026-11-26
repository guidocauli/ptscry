//go:build linux

package main

import (
	"bytes"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func FuzzReviewCleanNotice(f *testing.F) {
	f.Add("hello\x1b[31m‮ x")
	f.Fuzz(func(t *testing.T, s string) {
		c := cleanNotice(s)
		if len(c) > maxNotice || !utf8.ValidString(c) {
			t.Fatalf("bad %q", c)
		}
		for _, r := range c {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				t.Fatalf("control %q", c)
			}
		}
		out, _ := renderNotice(append([]byte{1}, s...))
		_ = out
	})
}

func FuzzReviewStrip(f *testing.F) {
	f.Add([]byte("a\x1b]7799;split;lr\x07b"))
	f.Fuzz(func(t *testing.T, b []byte) {
		out := stripControlOSC(b)
		if bytes.Contains(out, ctlOSCPrefixes[0]) || bytes.Contains(out, ctlOSCPrefixes[1]) {
			t.Fatalf("left %q -> %q", b, out)
		}
	})
}

func FuzzReviewParseOp(f *testing.F) {
	f.Add("send-keys", "x C-c M-x Enter", "abc")
	f.Add("layout", "save x", "{}")
	f.Fuzz(func(t *testing.T, cmd, args, in string) {
		hl, _, err := parseOp(cmd, strings.Fields(args), func(max int) (string, error) {
			if len(in) > max {
				return in[:max], nil
			}
			return in, nil
		})
		if err != nil {
			return
		}
		if e := validOpFields(&hl); e != nil && hl.Op != "" {
			// what the client accepts the monitor should accept
			if validName(hl.Name) {
				t.Logf("client accepted, monitor refuses: %+v: %v", hl, e)
			}
		}
	})
}
