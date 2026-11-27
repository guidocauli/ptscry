// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"fmt"
	"gioui.org/io/key"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type sink struct {
	mu  sync.Mutex
	buf []byte
}

func (s *sink) send(p []byte) { s.mu.Lock(); s.buf = append(s.buf, p...); s.mu.Unlock() }
func (s *sink) get() string   { s.mu.Lock(); defer s.mu.Unlock(); return string(s.buf) }
func (s *sink) wait(t *testing.T, want string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if strings.Contains(s.get(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%q never arrived; got %q", want, s.get())
}

func TestModelBasics(t *testing.T) {
	var s sink
	m := newTermModel(20, 4, 100, s.send)
	defer m.Close()
	m.Feed([]byte("ciao\r\nmondo \x1b[1;31mrosso\x1b[0m"))
	if got := m.Text(); got != "ciao\nmondo rosso" {
		t.Fatalf("text: %q", got)
	}
	// the emulator's answers go to the program
	m.Feed([]byte("\x1b[6n"))
	s.wait(t, "\x1b[2;12R")
	// a title
	var title string
	m.onTitle = func(x string) { title = x }
	m.Feed([]byte("\x1b]0;mio titolo\x07"))
	if title != "mio titolo" || m.Title() != "mio titolo" {
		t.Fatalf("title: %q", title)
	}
}

func TestModelScrollback(t *testing.T) {
	m := newTermModel(10, 3, 50, func([]byte) {})
	defer m.Close()
	for i := 1; i <= 20; i++ {
		m.Feed([]byte(fmt.Sprintf("riga%02d\r\n", i)))
	}
	if off, total := m.ScrollState(); off != 0 || total < 18 {
		t.Fatalf("state: %d %d", off, total)
	}
	m.Scroll(5)
	if !strings.Contains(m.Text(), "riga15") && !strings.Contains(m.Text(), "riga14") {
		t.Fatalf("scrolled view: %q", m.Text())
	}
	before := m.Text()
	m.Feed([]byte("nuova\r\n")) // new output must not move a view that is scrolled back
	if m.Text() != before {
		t.Fatalf("the view moved:\n%q\n%q", before, m.Text())
	}
	m.ScrollTo(false)
	if !strings.Contains(m.Text(), "nuova") {
		t.Fatalf("live view: %q", m.Text())
	}
	m.Scroll(1000)
	if off, total := m.ScrollState(); off != total {
		t.Fatalf("cannot go past the oldest line: %d %d", off, total)
	}
	m.SetScrollback(5)
	if _, total := m.ScrollState(); total > 5 {
		t.Fatalf("scrollback shrinks: %d", total)
	}
}

func TestModelSelectionAndCopy(t *testing.T) {
	m := newTermModel(20, 4, 100, func([]byte) {})
	defer m.Close()
	m.Feed([]byte("uno due tre\r\nquattro cinque\r\n"))
	m.SelectBegin(4, 0, false)
	m.SelectUpdate(6, 0)
	if !m.SelectEnd() {
		t.Fatal("selection expected")
	}
	if got := m.SelectedText(); got != "due" {
		t.Fatalf("selected: %q", got)
	}
	m.SelectBegin(8, 0, false)
	m.SelectUpdate(3, 1)
	m.SelectEnd()
	if got := m.SelectedText(); got != "tre\nquat" {
		t.Fatalf("multi-line: %q", got)
	}
	m.SelectWord(1, 1)
	if got := m.SelectedText(); got != "quattro" {
		t.Fatalf("word: %q", got)
	}
	m.SelectLine(0)
	if got := m.SelectedText(); got != "uno due tre" {
		t.Fatalf("line: %q", got)
	}
	m.SelectBegin(0, 0, true)
	m.SelectUpdate(2, 1)
	m.SelectEnd()
	if got := m.SelectedText(); got != "uno\nqua" {
		t.Fatalf("rectangle: %q", got)
	}
	m.SelectBegin(3, 3, false) // a click without a drag selects nothing
	if m.SelectEnd() || m.SelectedText() != "" {
		t.Fatal("a click is not a selection")
	}
	m.SelectLine(0)
	m.Feed([]byte("x\r\ny\r\nz\r\n")) // output that scrolls drops the selection
	if m.SelectedText() != "" {
		t.Fatal("selection must go when the text moves")
	}
}

func TestModelResizeAndAltScreen(t *testing.T) {
	m := newTermModel(20, 5, 100, func([]byte) {})
	defer m.Close()
	m.Feed([]byte("abc"))
	m.Resize(30, 8)
	if c, r := m.Size(); c != 30 || r != 8 {
		t.Fatalf("size %d %d", c, r)
	}
	m.Resize(1, 0) // nonsense is ignored
	if c, r := m.Size(); c != 30 || r != 8 {
		t.Fatalf("size after nonsense %d %d", c, r)
	}
	m.Feed([]byte("\x1b[?1049h")) // vim and friends
	if !m.AltScreen() {
		t.Fatal("alt screen expected")
	}
	m.Scroll(3) // no scrollback on the alt screen
	if off, _ := m.ScrollState(); off != 0 {
		t.Fatal("no scrolling on the alt screen")
	}
	m.Feed([]byte("\x1b[?1049l"))
	if m.AltScreen() {
		t.Fatal("back to the main screen")
	}
}

func TestModelMouseModes(t *testing.T) {
	m := newTermModel(20, 5, 100, func([]byte) {})
	defer m.Close()
	if m.MouseReporting() {
		t.Fatal("off by default")
	}
	m.Feed([]byte("\x1b[?1000h\x1b[?1006h"))
	if !m.MouseReporting() {
		t.Fatal("the program asked for the mouse")
	}
	m.Feed([]byte("\x1b[?1000l"))
	if m.MouseReporting() {
		t.Fatal("and then did not want it any more")
	}
}

func TestModelWideAndCombining(t *testing.T) {
	m := newTermModel(10, 2, 10, func([]byte) {})
	defer m.Close()
	m.Feed([]byte("a世界é\r\n"))
	if got := m.Text(); got != "a世界é" {
		t.Fatalf("wide text: %q", got)
	}
	m.SelectBegin(0, 0, false)
	m.SelectUpdate(5, 0)
	m.SelectEnd()
	if got := m.SelectedText(); got != "a世界é" {
		t.Fatalf("selected wide: %q", got)
	}
}

func TestWinButtonsSanitized(t *testing.T) {
	s := settings{WinButtons: "boh"}
	s = s.sanitized()
	if s.WinButtons != "auto" {
		t.Fatalf("WinButtons = %q", s.WinButtons)
	}
	s.WinButtons = "on"
	if s.sanitized().WinButtons != "on" {
		t.Fatal("on lost")
	}
}

func TestZoomKey(t *testing.T) {
	cases := []struct {
		n     string
		shift bool
		d     int
		ok    bool
	}{
		{"+", false, 1, true}, {"+", true, 1, true}, {"*", true, 1, true}, // Italian Shift++
		{"=", false, 1, true}, {"=", true, 0, true}, // Italian Shift+0
		{"-", false, -1, true}, {"_", true, -1, true}, {"0", false, 0, true}, {"A", false, 0, false},
	}
	for _, c := range cases {
		d, ok := zoomKey(key.Name(c.n), c.shift)
		if d != c.d || ok != c.ok {
			t.Errorf("zoomKey(%q,%v) = %d,%v", c.n, c.shift, d, ok)
		}
	}
}

func TestManageHelpers(t *testing.T) {
	if got := cleanOutput("\x1b[0mciao\x1b[1;32m ok\x1b[0m\r\nx\x00y"); got != "ciao ok\nxy" {
		t.Errorf("cleanOutput = %q", got)
	}
	var list, admin, svc, host int
	for _, a := range mgActions {
		if a.avail("remote", false) {
			list++
			if a.cap != capAll {
				t.Errorf("%q must not be available on a remote server", a.label)
			}
		}
		if a.avail("standalone", false) && a.cap == capAdmin {
			t.Errorf("%q must not be available without the monitor", a.label)
		}
		if a.avail("local", false) && a.cap == capAdmin {
			admin++
		}
		if a.avail("standalone", false) && a.cap == capStandby {
			host++
		}
		_ = svc
	}
	if list == 0 || admin == 0 || host == 0 {
		t.Errorf("actions: remote %d admin %d host %d", list, admin, host)
	}
	for _, a := range mgActions {
		if a.special == "" && a.args == nil {
			t.Errorf("%q has no command", a.label)
		}
	}
}

func TestMonoFonts(t *testing.T) {
	u := uniqFonts([]fontFace{{"b mono", "Bold", "/b-bold"}, {"b mono", "Regular", "/b"}, {"A Mono", "", "/a"}, {"x,y", "", "/x"}})
	if len(u) != 2 || u[0].Name != "A Mono" || u[1].Name != "b mono" || u[1].Path != "/b" {
		t.Fatalf("uniqFonts = %+v", u)
	}
	if f := filterFonts(u, " MONO"); len(f) != 2 {
		t.Fatalf("filter = %+v", f)
	}
	if f := filterFonts(u, "zzz"); len(f) != 0 {
		t.Fatalf("filter = %+v", f)
	}
	// a real file, when the machine has one
	const dv = "/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf"
	if n := monoFamilies(dv); len(n) > 0 && n[0] != "DejaVu Sans Mono" {
		t.Fatalf("monoFamilies = %q", n)
	}
	if n := monoFamilies("/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf"); len(n) != 0 {
		t.Fatalf("proportional font counted as mono: %q", n)
	}
	if _, err := os.Stat(dv); err == nil {
		faces, err := loadFontFile(dv)
		if err != nil || len(faces) == 0 || faces[0].Font.Typeface == "" {
			t.Fatalf("loadFontFile: %v %v", faces, err)
		}
	}
}

// Scrolling moves whole rows (patched ultraviolet): the screen and the
// scrollback must still be exactly what the program printed, also inside a
// scroll region and when feeding in tiny pieces.
func TestScrollRows(t *testing.T) {
	row := func(m *termModel, r int) string {
		var sb strings.Builder
		for c := 0; c < 12; c++ {
			if cell := m.em.CellAt(c, r); cell != nil && cell.Content != "" {
				sb.WriteString(cell.Content)
			}
		}
		return strings.TrimSpace(sb.String())
	}
	m := newTermModel(20, 5, 1000, func([]byte) {})
	defer m.Close()
	var in strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&in, "line%d\r\n", i)
	}
	b := []byte(in.String())
	for i := 0; i < len(b); i += 5 {
		m.Feed(b[i:min(i+5, len(b))])
	}
	if got := row(m, 3); got != "line39" {
		t.Fatalf("row 3 = %q, want line39", got)
	}
	if got := row(m, 0); got != "line36" {
		t.Fatalf("row 0 = %q, want line36", got)
	}
	if n := m.em.ScrollbackLen(); n != 36 {
		t.Fatalf("scrollback = %d, want 36", n)
	}
	// region of rows 2-4: the first row stays, the rest scroll
	m.Feed([]byte("\x1b[2J\x1b[H"))
	m.Feed([]byte("top\r\n\x1b[2;4r\x1b[2;1Hr1\r\nr2\r\nr3\r\nr4\r\nr5"))
	if row(m, 0) != "top" || row(m, 1) != "r3" || row(m, 2) != "r4" || row(m, 3) != "r5" {
		t.Fatalf("region: %q %q %q %q", row(m, 0), row(m, 1), row(m, 2), row(m, 3))
	}
}

func TestWithShell(t *testing.T) {
	if got := withShell([]string{"A=1", "SHELL=/bin/zsh"}); len(got) != 2 {
		t.Fatalf("SHELL already set: %q", got)
	}
	t.Setenv("SHELL", "/bin/fish")
	got := withShell([]string{"A=1"})
	if len(got) != 2 || got[1] != "SHELL=/bin/fish" {
		t.Fatalf("withShell = %q", got)
	}
}

func TestDefaultProfiles(t *testing.T) {
	ps := defaultProfiles()
	has := map[string]bool{}
	for _, p := range ps {
		has[p.Mode] = true
	}
	if !has["local"] || !has["shell"] || has["standalone"] != (runtime.GOOS != "windows") {
		t.Fatalf("profiles %v", ps)
	}
	s := defaultSettings()
	s.Default = "nonexistent"
	s = s.sanitized()
	if s.profile(s.Default) == nil {
		t.Fatalf("default %q has no profile", s.Default)
	}
}

func TestSplitCommand(t *testing.T) {
	cases := map[string][]string{
		`/bin/bash -l`:                           {"/bin/bash", "-l"},
		`"C:\Program Files\Git\bin\bash.exe" -i`: {`C:\Program Files\Git\bin\bash.exe`, "-i"},
		`  fish   --login `:                      {"fish", "--login"},
		`sh -c 'echo a b'`:                       {"sh", "-c", "echo a b"},
		``:                                       nil,
	}
	for in, want := range cases {
		got, err := splitCommand(in)
		if err != nil || len(got) != len(want) {
			t.Fatalf("%q: %q %v", in, got, err)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%q: %q", in, got)
			}
		}
	}
	if _, err := splitCommand(`"open`); err == nil {
		t.Fatal("unclosed quote accepted")
	}
	if validShellCmd("bash\nrm -rf /") || !validShellCmd("") || !validShellCmd("/bin/zsh") {
		t.Fatal("validShellCmd")
	}
	if runtime.GOOS != "windows" {
		for _, s := range availableShells() {
			if s.Cmd == "" || s.Name == "" {
				t.Fatalf("empty shell %+v", s)
			}
		}
	}
}

func TestTabKindAndShellName(t *testing.T) {
	mk := func(mode, target, shell string) *tab {
		return &tab{h: &hub{p: profile{Mode: mode, Target: target}}, shell: shell, session: "main"}
	}
	if k := mk("ssh", "a@b:22", "").kind(); k != "ssh" {
		t.Fatal(k)
	}
	if k := mk("remote", "a@b", "").kind(); k != "ptscry" {
		t.Fatal(k)
	}
	if k := mk("local", "", "").kind(); k != "ptscry" {
		t.Fatal(k)
	}
	if n := mk("shell", "", "/usr/bin/fish -l").shellName(); n != "fish" {
		t.Fatalf("shellName = %q", n)
	}
	if n := mk("shell", "/bin/zsh", "").shellName(); n != "zsh" {
		t.Fatalf("shellName = %q", n)
	}
	tb := mk("shell", "", "/bin/dash")
	if s := tb.title(); s != "dash" {
		t.Fatalf("title = %q", s)
	}
	tb = mk("ssh", "a@b:22", "")
	tb.label = "b"
	if s := tb.title(); s != "[SSH] b" {
		t.Fatalf("title = %q", s)
	}
}

func TestManagePages(t *testing.T) {
	pages := map[string]bool{}
	for _, p := range mgPages {
		pages[p.key] = true
	}
	for _, a := range mgActions {
		if !pages[a.page] || a.section == "" || a.about == "" {
			t.Errorf("%q: page %q section %q about %q", a.label, a.page, a.section, a.about)
		}
	}
	if !pageAvail("sess", "remote") || !pageAvail("host", "remote") || pageAvail("admin", "remote") || pageAvail("svc", "remote") {
		t.Error("pages of a remote server")
	}
	if pageAvail("admin", "standalone") || !pageAvail("host", "standalone") {
		t.Error("pages without server")
	}
}
