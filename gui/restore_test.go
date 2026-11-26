// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import "testing"

func TestShQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/usr/lib":        `'/usr/lib'`,
		"/tmp/it's here":  `'/tmp/it'\''s here'`,
		"/a b/$HOME/`x`":  "'/a b/$HOME/`x`'",
		"/tmp/;rm -rf /x": `'/tmp/;rm -rf /x'`,
	} {
		if got := shQuote(in); got != want {
			t.Errorf("shQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestParseSessions(t *testing.T) {
	l := parseSessions([]byte(`{"sessions":[{"name":"a","pid":1,"cwd":"/x"},{"name":"b"}],"jobs":[]}`))
	if len(l) != 2 || l[0].Cwd != "/x" || l[1].Name != "b" || l[1].Cwd != "" {
		t.Fatalf("%+v", l)
	}
	if parseSessions([]byte("garbage")) != nil {
		t.Fatal("garbage must give nothing")
	}
}

func TestWorkspacesSanitized(t *testing.T) {
	s := defaultSettings()
	s.Workspaces = []workspace{
		{Name: "ok", Tabs: []tabState{{Profile: localName, Session: "a", Label: "x"}, {Profile: "nope", Session: "b"}, {Profile: localName, Session: "bad name;"}}, Active: 9},
		{Name: "ok"}, // duplicate name
		{Name: ""},
		{Name: "lbl", Tabs: []tabState{{Profile: localName, Session: "c", Label: "a\nb"}}},
	}
	s = s.sanitized()
	if len(s.Workspaces) != 2 {
		t.Fatalf("workspaces: %+v", s.Workspaces)
	}
	if w := s.Workspaces[0]; len(w.Tabs) != 1 || w.Tabs[0].Session != "a" || w.Active != 0 {
		t.Fatalf("tabs not cleaned: %+v", w)
	}
	if s.Workspaces[1].Tabs[0].Label != "" {
		t.Fatal("a label with a newline must be dropped")
	}
}

func TestEnvVars(t *testing.T) {
	for _, n := range []string{"A", "_x", "PATH_EXTRA", "a1"} {
		if !validEnvName(n) {
			t.Errorf("%q should be valid", n)
		}
	}
	for _, n := range []string{"", "1a", "A-B", "A B", "A=B", "à", "A;rm"} {
		if validEnvName(n) {
			t.Errorf("%q should be invalid", n)
		}
	}
	got := cleanEnv([]envVar{{"A", "1"}, {"bad name", "x"}, {"B", "line\nbreak"}, {"A", "2"}, {"C", "it's"}})
	if len(got) != 2 || got[0] != (envVar{"A", "2"}) || got[1] != (envVar{"C", "it's"}) {
		t.Fatalf("cleanEnv: %+v", got)
	}
	if l := exportLines(got); l != " export A='2'\n export C='it'\\''s'\n" {
		t.Fatalf("exportLines: %q", l)
	}
	var many []envVar
	for i := 0; i < 300; i++ {
		many = append(many, envVar{"V" + string(rune('A'+i%26)) + string(rune('a'+i/26)), "x"})
	}
	if len(cleanEnv(many)) > 100 {
		t.Fatal("more than 100 variables kept")
	}
}
