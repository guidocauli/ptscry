// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testGUI(s settings) *gui {
	s = s.sanitized()
	return &gui{set: s, hubs: map[string]*hub{}, wake: make(chan struct{}, 1), ctl: make(chan ctlReq, 16)}
}

func TestCleanGroups(t *testing.T) {
	in := []group{
		{Name: "A"}, {Name: "A"}, // duplicate
		{Name: ""}, {Name: "bad\nname"}, // invalid
		{Name: "B", Parent: "missing"},
		{Name: "C", Parent: "D"}, {Name: "D", Parent: "C"}, // a loop
		{Name: "E", Parent: "E"},
		{Name: "F", Parent: "A", Color: "red"},
	}
	out := cleanGroups(in)
	got := map[string]group{}
	for _, g := range out {
		got[g.Name] = g
	}
	if len(out) != 6 {
		t.Fatalf("want 6 groups, got %d: %+v", len(out), out)
	}
	if got["B"].Parent != "" || got["E"].Parent != "" {
		t.Errorf("parents not cleaned: %+v", out)
	}
	if got["C"].Parent != "" && got["D"].Parent != "" {
		t.Errorf("loop not cut: %+v", out)
	}
	if got["F"].Parent != "A" || got["F"].Color != "" {
		t.Errorf("F: %+v", got["F"])
	}
}

func TestSanitizedTreeFields(t *testing.T) {
	s := defaultSettings()
	s.Groups = []group{{Name: "prod", Color: "#AABBCC"}}
	s.Profiles = append(s.Profiles,
		profile{Name: "a", Mode: "remote", Target: "a:2222", Group: "nope", Color: "#zzzzzz", Description: "x\ny"},
		profile{Name: "b", Mode: "ssh", Target: "b:22", Group: "prod"})
	s.SidebarW, s.ActLogH, s.Sidebar = 5, 9999, "maybe"
	s = s.sanitized()
	a, b := s.profile("a"), s.profile("b")
	if a.Group != "" || a.Color != "" || a.Description != "x y" {
		t.Errorf("a: %+v", *a)
	}
	if b.Group != "prod" || s.colorOf(*b) != "#AABBCC" {
		t.Errorf("b: %+v color %q", *b, s.colorOf(*b))
	}
	if s.SidebarW != 140 || s.ActLogH != 600 || s.Sidebar != "on" {
		t.Errorf("sizes: %d %d %q", s.SidebarW, s.ActLogH, s.Sidebar)
	}
}

func TestParseSSHConfig(t *testing.T) {
	d := t.TempDir()
	inc := filepath.Join(d, "conf.d")
	os.MkdirAll(inc, 0o700)
	os.WriteFile(filepath.Join(inc, "extra"), []byte("Host extra1\n  HostName 10.1.1.1\n  Port 2200\n"), 0o600)
	main := filepath.Join(d, "config")
	os.WriteFile(main, []byte(`# comment
Include conf.d/*
Host web01 web01-alias
  HostName 10.0.0.11
  User admin
  ProxyJump jumper@bastion:2201,other
Host db*
  User postgres
Match host foo
  User nobody
Host plain
Host bad
  HostName %h.example
Host *
  User everybody
`), 0o600)
	set, err := parseSSHConfig(main, false)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]profile{}
	for _, it := range set.items {
		got[it.p.Name] = it.p
	}
	if len(got) != 4 {
		t.Fatalf("want extra1, web01, web01-alias, plain; got %v", got)
	}
	if p := got["web01"]; p.Target != "admin@10.0.0.11:22" || p.Jump != "jumper@bastion:2201" || p.Mode != "ssh" {
		t.Errorf("web01: %+v", p)
	}
	if p := got["extra1"]; p.Target != "10.1.1.1:2200" {
		t.Errorf("extra1 (Include): %+v", p)
	}
	if p := got["plain"]; p.Target != "plain:22" {
		t.Errorf("plain: %+v", p)
	}
	set, _ = parseSSHConfig(main, true)
	for _, it := range set.items {
		if it.p.Mode != "remote" || !strings.HasSuffix(it.p.Target, ":2222") || it.p.Jump != "" {
			t.Errorf("as ptscry: %+v", it.p)
		}
	}
}

func TestParseRexDesk(t *testing.T) {
	data := []byte(`{"version":1,"groups":[
	 {"name":"prod","defaults":{"ssh_user":"ops","ssh_port":2201,"auth_method":"key","key_file":"~/.ssh/prod","color":"#e05252"}},
	 {"name":"web","parent":"prod","description":"front"}],
	"servers":[
	 {"id":"1","name":"web1","host":"10.0.0.1","groups":["web"],"favorite":true,"jump_host":"bastion"},
	 {"id":"2","name":"bastion","host":"bastion.example.org","ssh_user":"jump"},
	 {"id":"3","name":"desk","host":"10.0.0.9","protocol":"rdp"},
	 {"id":"4","name":"old","host":"10.0.0.8","disabled":true}]}`)
	set, err := parseRexDesk(data, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.items) != 2 || len(set.warn) != 2 {
		t.Fatalf("items %d warn %v", len(set.items), set.warn)
	}
	w := set.items[0].p
	if w.Name != "web1" || w.Target != "ops@10.0.0.1:2201" || w.Auth != "key" || !strings.HasSuffix(w.Identity, filepath.Join(".ssh", "prod")) ||
		w.Group != "web" || !w.Favorite || w.Jump != "jump@bastion.example.org:22" {
		t.Errorf("web1: %+v", w)
	}
	if len(set.groups) != 2 || set.groups[0].Color != "#e05252" || set.groups[1].Parent != "prod" {
		t.Errorf("groups: %+v", set.groups)
	}
	if _, err := parseRexDesk([]byte("not json"), false); err == nil {
		t.Error("a broken file must be an error")
	}
}

func TestMarkAndApplyImport(t *testing.T) {
	g := testGUI(defaultSettings())
	g.set.Profiles = append(g.set.Profiles, profile{Name: "web1", Mode: "ssh", Target: "a@h1:22", Auth: "agent"})
	set := importSet{
		items: []*importItem{
			{p: profile{Name: "same", Mode: "ssh", Target: "a@h1:22", Auth: "agent"}},               // already there
			{p: profile{Name: "web1", Mode: "ssh", Target: "a@h2:22", Auth: "agent", Group: "web"}}, // name taken
			{p: profile{Name: "twin", Mode: "ssh", Target: "a@h3:22", Auth: "agent"}},               // new
			{p: profile{Name: "twin2", Mode: "ssh", Target: "a@h3:22", Auth: "agent"}},              // same address twice in the file
		},
		groups: []group{{Name: "web", Parent: "prod"}, {Name: "prod", Color: "#123456"}},
	}
	markImport(&set, g.set.Profiles)
	if set.items[0].status != "già presente" || set.items[0].take.Value {
		t.Errorf("duplicate: %+v", set.items[0])
	}
	if set.items[1].p.Name != "web1 (2)" || !set.items[1].take.Value {
		t.Errorf("renamed: %+v", set.items[1].p)
	}
	if set.items[3].status != "già presente" {
		t.Errorf("twin twice: %+v", set.items[3])
	}
	d := &importDialog{set: set}
	n, err := d.apply(g)
	if err != nil || n != 2 {
		t.Fatalf("apply: %d %v", n, err)
	}
	if g.set.profile("web1 (2)") == nil || g.set.profile("twin") == nil {
		t.Error("not imported")
	}
	if gr := g.set.groupByName("web"); gr == nil || gr.Parent != "prod" {
		t.Errorf("group web: %+v", gr)
	}
	if gr := g.set.groupByName("prod"); gr == nil || gr.Color != "#123456" {
		t.Errorf("group prod (parent of web): %+v", gr)
	}
	// importing again changes nothing
	markImport(&d.set, g.set.Profiles)
	if n, _ := d.apply(g); n != 0 {
		t.Errorf("second import added %d", n)
	}
}

func TestTreeRows(t *testing.T) {
	s := defaultSettings()
	s.Groups = []group{{Name: "prod"}, {Name: "web", Parent: "prod"}, {Name: "lab"}}
	s.Profiles = append(s.Profiles,
		profile{Name: "w1", Mode: "remote", Target: "w1:2222", Group: "web", Favorite: true},
		profile{Name: "db", Mode: "ssh", Target: "db:22", Group: "prod", Description: "postgres"},
		profile{Name: "nas", Mode: "ssh", Target: "nas:22"})
	s.Open = []string{"g:prod", "g:web", "f:"}
	g := testGUI(s)
	keys := func() []string {
		var k []string
		for _, r := range g.buildRows() {
			k = append(k, r.key)
		}
		return k
	}
	got := strings.Join(keys(), " ")
	want := "c: f: f/p:w1 g:lab g:prod g:web p:w1 p:db p:nas"
	if got != want {
		t.Errorf("rows:\n got %s\nwant %s", got, want)
	}
	// the filter: a description, a group name (all its content)
	g.side.filter.SetText("postgres")
	if got := strings.Join(keys(), " "); got != "g:prod p:db" {
		t.Errorf("filter postgres: %s", got)
	}
	g.side.filter.SetText("WEB")
	if got := strings.Join(keys(), " "); got != "g:prod g:web p:w1" {
		t.Errorf("filter web: %s", got)
	}
}

func TestDropAndDeleteGroup(t *testing.T) {
	s := defaultSettings()
	s.Groups = []group{{Name: "prod"}, {Name: "web", Parent: "prod"}}
	s.Profiles = append(s.Profiles, profile{Name: "w1", Mode: "remote", Target: "w1:2222"})
	s.Open = []string{"g:prod"}
	g := testGUI(s)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	g.side.rows = g.buildRows()
	g.dropOn("p:w1", "g:web")
	if g.set.profile("w1").Group != "web" {
		t.Fatal("not moved")
	}
	g.side.rows = g.buildRows()
	g.dropOn("p:w1", "f:")
	if !g.set.profile("w1").Favorite {
		t.Fatal("not a favourite")
	}
	g.deleteGroup("web")
	if g.set.profile("w1").Group != "prod" || g.set.groupByName("web") != nil {
		t.Errorf("after deleting web: %+v %+v", *g.set.profile("w1"), g.set.Groups)
	}
	// a built-in profile cannot be dragged
	g.side.rows = g.buildRows()
	g.dropOn("p:"+localName, "g:prod")
	if g.set.profile(localName).Group != "" {
		t.Error("built-in moved")
	}
}

func TestRenameServer(t *testing.T) {
	s := defaultSettings()
	s.Profiles = append(s.Profiles, profile{Name: "old", Mode: "remote", Target: "h:2222"})
	s.Default = "old"
	s.Open = []string{"p:old", "f/p:old"}
	s.Workspaces = []workspace{{Name: "w", Tabs: []tabState{{Profile: "old", Session: "main"}}}}
	g := testGUI(s)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	g.openServerDialog("old", "")
	d := g.dlg.(*serverDialog)
	d.name.SetText("new")
	if _, err := d.save(g); err != nil {
		t.Fatal(err)
	}
	if g.set.profile("new") == nil || g.set.profile("old") != nil || g.set.Default != "new" ||
		g.set.Open[0] != "p:new" || g.set.Open[1] != "f/p:new" || g.set.Workspaces[0].Tabs[0].Profile != "new" {
		t.Errorf("after rename: %+v", g.set)
	}
	// a name already used is refused
	g.openServerDialog("", "")
	d = g.dlg.(*serverDialog)
	d.host.SetText("x")
	d.name.SetText(localName)
	if _, err := d.save(g); err == nil {
		t.Error("a taken name was accepted")
	}
	// a group typed in the field is created
	d.name.SetText("x1")
	d.groupCombo.ed.SetText("nuovo")
	if _, err := d.save(g); err != nil || g.set.groupByName("nuovo") == nil || g.set.profile("x1").Group != "nuovo" {
		t.Errorf("new group: %v %+v", err, g.set.Groups)
	}
}

func TestSetOpenKeys(t *testing.T) {
	g := testGUI(defaultSettings())
	g.setOpen("g:ap:x", true) // a group whose name holds "p:" is not a server
	if len(g.side.sess) != 0 {
		t.Errorf("a group opened a server's sessions: %v", g.side.sess)
	}
	g.setOpen("f/p:web", true)
	if g.side.sess["web"] == nil || !g.side.sess["web"].want {
		t.Errorf("server under Preferiti: %v", g.side.sess)
	}
}

func TestValidSessions(t *testing.T) {
	in := []sessInfo{{Name: "ok"}, {Name: "-x"}, {Name: "a b"}, {Name: "main", Cwd: "/home/x\x1b[31m"}}
	out := validSessions(in)
	if len(out) != 2 || out[0].Name != "ok" || out[1].Name != "main" || strings.ContainsRune(out[1].Cwd, 0x1b) {
		t.Errorf("validSessions = %+v", out)
	}
}
