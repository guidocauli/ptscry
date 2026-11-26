//go:build linux

package main

import (
	"strings"
	"testing"
)

func TestProcessAct(t *testing.T) {
	st := inState{}
	out, act, rest := st.processAct([]byte("ab\x02sxy"))
	if string(out) != "ab" || act != actChoose || string(rest) != "xy" {
		t.Errorf("got %q %v %q", out, act, rest)
	}
	for k, want := range map[string]keyAct{"(": actPrev, ")": actNext, "L": actLast, "c": actNew, "$": actRename, "f": actFreeze, "m": actWatchAct, "M": actWatchSil} {
		st := inState{}
		if _, a, _ := st.processAct([]byte("\x02" + k)); a != want {
			t.Errorf("%s: %v", k, a)
		}
	}
	// the prefix split across two reads
	st = inState{}
	if _, a, _ := st.processAct([]byte{0x02}); a != actNone {
		t.Error("half a command")
	}
	if _, a, _ := st.processAct([]byte("s")); a != actChoose {
		t.Error("command after a split prefix")
	}
	// the old API ignores the new commands and still detaches
	st = inState{}
	if out, d := st.process([]byte("a\x02sb\x02d")); string(out) != "ab" || !d {
		t.Errorf("process: %q %v", out, d)
	}
}

func TestChooser(t *testing.T) {
	ss := []sessionInfo{{Name: "alpha"}, {Name: "beta", Cmd: "vim\x1b]0;x\x07"}, {Name: "gamma", Frozen: true}}
	c := newChooser(ss, "beta")
	if c.sel != 1 {
		t.Errorf("selection starts on the current session: %d", c.sel)
	}
	d := c.draw(10, 40)
	if strings.Contains(d, "\x1b]0;") {
		t.Error("a command line drew an escape sequence")
	}
	if done, _ := c.feed([]byte("\x1b[B")); done || c.sel != 2 {
		t.Errorf("down: %d", c.sel)
	}
	if done, _ := c.feed([]byte("al")); done || len(c.shown()) != 1 {
		t.Errorf("filter: %v", c.shown())
	}
	done, rest := c.feed([]byte("\recho x"))
	if !done || c.picked != "alpha" || string(rest) != "echo x" {
		t.Errorf("enter: %v %q %q", done, c.picked, rest)
	}
	c = newChooser(ss, "beta")
	if done, _ := c.feed([]byte{0x1b}); !done || c.picked != "" {
		t.Error("Esc cancels")
	}
	c = newChooser(ss, "beta")
	if done, _ := c.feed([]byte("\x1b[1;5Czz\x7f")); done || c.filter != "z" {
		t.Errorf("unknown sequence skipped, backspace: %q", c.filter)
	}
	_ = c.draw(3, 5) // tiny terminal: no panic
	c = newChooser(nil, "")
	if done, _ := c.feed([]byte("\r")); !done || c.picked != "" {
		t.Error("empty list")
	}
}

func TestPrompt(t *testing.T) {
	p := newPrompt("name: ", "old")
	p.feed([]byte("\x15new-1\x1b[D/é"))
	if done, rest := p.feed([]byte("\rls")); !done || !p.ok || p.text != "new-1/" || string(rest) != "ls" {
		t.Errorf("%v %q %q", p.ok, p.text, rest)
	}
	p = newPrompt("name: ", "x")
	if done, _ := p.feed([]byte{0x03}); !done || p.ok {
		t.Error("Ctrl-C cancels")
	}
	p = newPrompt("n", "")
	p.feed([]byte(strings.Repeat("a", 200)))
	if len(p.text) != 64 {
		t.Errorf("prompt length %d", len(p.text))
	}
}
