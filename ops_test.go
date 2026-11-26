//go:build linux

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanNotice(t *testing.T) {
	cases := map[string]string{
		"build done":                   "build done",
		"a\x1b]0;evil\x07b":            "a]0;evilb",
		"line1\nline2\r\n\tx":          "line1 line2 x",
		"‮evil‬ ok":                    "evil ok",
		"\u009b31m red":                "31m red",
		"  ":                           "",
		string([]byte{0xff, 'a'}):      "?a",
		strings.Repeat("é", maxNotice): strings.Repeat("é", maxNotice/2),
	}
	for in, want := range cases {
		if got := cleanNotice(in); got != want {
			t.Errorf("cleanNotice(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderNotice(t *testing.T) {
	out, redraw := renderNotice(append([]byte{0}, "hi\x1b[2J"...))
	if redraw || !strings.Contains(string(out), "[ptscry] hi[2J") || strings.Contains(string(out), "\x1b[2J") {
		t.Errorf("normal screen: %q %v", out, redraw)
	}
	out, redraw = renderNotice(append([]byte{noticeAlt}, "hi"...))
	if !redraw || !strings.HasPrefix(string(out), "\x1b7") || !strings.Contains(string(out), "\x1b8") {
		t.Errorf("alt screen: %q %v", out, redraw)
	}
	if out, _ := renderNotice(nil); out != nil {
		t.Error("empty payload drew something")
	}
	if out, _ := renderNotice([]byte{0, 0x1b}); out != nil {
		t.Error("control-only text drew something")
	}
}

func TestSendKeysText(t *testing.T) {
	got, err := sendKeysText([]string{"ls -l", "Enter", "C-c", "M-x", "^D", "Up", "F5", "C-?", "C-xx"}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := "ls -l\r\x03\x1bx\x04\x1b[A\x1b[15~\x7fC-xx"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
	if got, _ := sendKeysText([]string{"Enter"}, true); got != "Enter" {
		t.Errorf("literal: %q", got)
	}
	if _, err := sendKeysText([]string{strings.Repeat("a", maxSendKeys+1)}, false); err == nil {
		t.Error("too long accepted")
	}
	if _, err := sendKeysText(nil, false); err == nil {
		t.Error("empty accepted")
	}
}

func TestCheckLayout(t *testing.T) {
	good := []string{`{}`, `{"version":1,"panes":{"split":"h","a":{"session":"x"}}}`}
	bad := []string{"", `[]`, `"x"`, `{`, "{\"a\":\"\x00\"}", `{"a":1}` + strings.Repeat(" ", maxLayout), string([]byte{'{', 0xff, '}'})}
	for _, s := range good {
		if err := checkLayout(s); err != nil {
			t.Errorf("%q rejected: %v", s, err)
		}
	}
	for _, s := range bad {
		if checkLayout(s) == nil {
			t.Errorf("%.40q accepted", s)
		}
	}
	for _, n := range []string{"..", ".x", "a/b", "", strings.Repeat("a", 65)} {
		if validLayoutName(n) {
			t.Errorf("layout name %q accepted", n)
		}
	}
}

func TestParseOpAndValidHello(t *testing.T) {
	in := func(s string) func(int) (string, error) {
		return func(max int) (string, error) { return readLimited(strings.NewReader(s), max) }
	}
	hl, _, err := parseOp("notify", []string{"w", "build", "done"}, in(""))
	if err != nil || hl.Text != "build done" || validHello(&hl) != nil {
		t.Fatalf("notify: %+v %v", hl, err)
	}
	hl, _, err = parseOp("notify", []string{"w"}, in("from\nstdin\x1b"))
	if err != nil || hl.Text != "from stdin" {
		t.Fatalf("notify stdin: %+v %v", hl, err)
	}
	if _, _, err := parseOp("layout", []string{"save", "x"}, in(`[1]`)); err == nil {
		t.Error("non-object layout accepted")
	}
	if _, _, err := parseOp("layout", []string{"save", "x"}, in(strings.Repeat("a", maxLayout+1))); err == nil {
		t.Error("oversized input accepted")
	}
	if _, _, err := parseOp("rename", []string{"a", "b/c"}, in("")); err == nil {
		t.Error("bad new name accepted")
	}
	hl, out, err := parseOp("layout", []string{"list", "-j"}, in(""))
	if err != nil || hl.Op != "layout-list" || out != outNamesJSON {
		t.Errorf("layout list -j: %+v %v", hl, err)
	}
	for _, h := range []hello{
		{Op: "rename", Name: "a"},
		{Op: "rename", Name: "a", NewName: "../x"},
		{Op: "kill", Name: "a", NewName: "b"},
		{Op: "kill", Name: "a", Text: "x"},
		{Op: "notify", Name: "a"},
		{Op: "send-keys", Name: "a", Text: "x", Cmd: []string{"sh"}},
		{Op: "notify", Name: "a", Text: strings.Repeat("x", maxOpText+1)},
		{Op: "layout-get", Name: ".hidden"},
		{Op: "layout-set", Name: "x"},
		{Op: "send-keys", Name: "a", Text: "x", ReadOnly: true},
	} {
		if validHello(&h) == nil {
			t.Errorf("accepted: %+v", h)
		}
	}
}

func TestRenameKeepsKey(t *testing.T) {
	h := &host{sessions: map[string]*session{}, results: map[string]*jobResult{}}
	a := &session{h: h, key: "a", name: "a"}
	b := &session{h: h, key: "b", name: "b"}
	h.sessions["a"], h.sessions["b"] = a, b
	if _, err := h.rename("a", "b"); err == nil {
		t.Error("rename onto an existing session")
	}
	if _, err := h.rename("a", "c"); err != nil || a.Name() != "c" || a.key != "a" || h.sessions["c"] != a {
		t.Fatalf("rename a->c: %v", err)
	}
	if !h.nameTaken("a") {
		t.Error("the creation name of a renamed session must stay taken")
	}
	if _, err := h.rename("b", "a"); err == nil {
		t.Error("another session took the creation name of a renamed one")
	}
	if _, err := h.rename("c", "a"); err != nil || h.sessions["a"] != a {
		t.Errorf("back to its own creation name: %v", err)
	}
	if _, err := h.rename("zz", "y"); err == nil {
		t.Error("rename of a missing session")
	}
	h.results["job"] = &jobResult{}
	if _, err := h.rename("b", "job"); err == nil {
		t.Error("rename onto a finished job")
	}
	if n := h.autoName(); n != "s1" {
		t.Errorf("autoName = %q", n)
	}
}

func TestLayoutFiles(t *testing.T) {
	home := t.TempDir()
	h := &host{home: home, uid: os.Getuid()}
	if err := h.saveLayout("web", `{"v":1}`); err != nil {
		t.Fatal(err)
	}
	if s, err := h.loadLayout("web"); err != nil || s != `{"v":1}` {
		t.Fatalf("load: %q %v", s, err)
	}
	fi, _ := os.Stat(h.layoutDir())
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", fi.Mode().Perm())
	}
	// a symlink planted in place of a layout is not followed
	secret := filepath.Join(home, "secret")
	_ = os.WriteFile(secret, []byte(`{"secret":1}`), 0o600)
	_ = os.Symlink(secret, filepath.Join(h.layoutDir(), "evil.json"))
	if _, err := h.loadLayout("evil"); err == nil {
		t.Error("followed a symlink")
	}
	if names, _ := h.listLayouts(); len(names) != 1 || names[0] != "web" {
		t.Errorf("list: %v", names)
	}
	if err := h.saveLayout("evil", `{"x":2}`); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(secret); string(b) != `{"secret":1}` {
		t.Error("saving over a symlink wrote through it")
	}
	if err := h.deleteLayout("web"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.loadLayout("web"); err == nil {
		t.Error("deleted layout still there")
	}
	if err := h.saveLayout("../x", `{}`); err == nil {
		t.Error("path in a layout name")
	}
	// a directory that is a symlink is refused
	h2 := &host{home: t.TempDir(), uid: os.Getuid()}
	_ = os.MkdirAll(filepath.Join(h2.home, ".local", "state", "ptscry"), 0o700)
	_ = os.Symlink(home, h2.layoutDir())
	if err := h2.saveLayout("x", `{}`); err == nil {
		t.Error("layout dir symlink accepted")
	}
}

func TestStripControlOSC(t *testing.T) {
	in := "a\x1b]7799;split;lr\x07b\x1b]7799;layout;x\x1b\\c\x1b]0;title\x07d\x1b]7799;unterminated"
	if got := string(stripControlOSC([]byte(in))); got != "abc\x1b]0;title\x07d" {
		t.Errorf("%q", got)
	}
}

func TestStripControlOSCRecombine(t *testing.T) {
	for _, in := range []string{
		"\x1b]7\x1b]7799;\a799;session;evil\a",
		"\x9d7799;session;evil\a",
		"\x1b]\x1b]7799;x\a7799;split;lr\x9c",
		"\x1b]77\x9d7799;a\x9c99;split\a",
	} {
		out := stripControlOSC([]byte(in))
		if bytes.Contains(out, []byte("7799;")) && (bytes.Contains(out, []byte("\x1b]7799;")) || bytes.Contains(out, []byte("\x9d7799;"))) {
			t.Errorf("%q -> %q", in, out)
		}
	}
}

func TestReviewFixes(t *testing.T) {
	for _, n := range []string{"-d", "-r", "-"} {
		if nameRe.MatchString(n) {
			t.Errorf("session name %q accepted", n)
		}
	}
	st := &sessState{env: map[string]string{}, noPTYKey: true,
		input: func(int) (string, error) { return "id\n", nil }}
	if _, _, err := parseCommand("ptscry send-keys s1 id Enter", "main", st); err == nil {
		t.Error("send-keys allowed for a no-pty key")
	}
	st.noPTYKey = false
	if _, _, err := parseCommand("ptscry send-keys s1 id Enter", "main", st); err != nil {
		t.Errorf("send-keys refused: %v", err)
	}
}
