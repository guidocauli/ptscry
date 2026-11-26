package main

import (
	"encoding/json"
	"image"
	"strings"
	"testing"
)

func mkTab(g *gui, name string) *tab {
	t := &tab{session: name, profile: "p", redo: make(chan struct{}, 1), app: g}
	t.model = newTermModel(80, 24, 100, func([]byte) {})
	t.h = &hub{p: profile{Name: "p", Mode: "remote"}}
	return t
}

func TestPaneTree(t *testing.T) {
	g := &gui{}
	a, b, c := mkTab(g, "a"), mkTab(g, "b"), mkTab(g, "c")
	g.tabs = []*tab{a}
	g.insertPane(a, a, b, true, 0.5)
	g.insertPane(a, b, c, false, 0.5)
	if got := names(a.panes()); got != "a b c" {
		t.Fatalf("panes %q", got)
	}
	if a.focused() != c || c.lead != a || b.lead != a {
		t.Fatal("focus / leader")
	}
	// geometry: a | (b over c)
	a.root.rect = image.Rect(0, 0, 100, 100)
	a.root.a.rect = image.Rect(0, 0, 50, 100)
	a.root.b.a.rect = image.Rect(50, 0, 100, 50)
	a.root.b.b.rect = image.Rect(50, 50, 100, 100)
	g.focusPane(0, -1)
	if a.focused() != b {
		t.Errorf("up from c: %s", a.focused().session)
	}
	g.focusPane(-1, 0)
	if a.focused() != a {
		t.Errorf("left from b: %s", a.focused().session)
	}
	g.focusPane(-1, 0) // nothing there
	if a.focused() != a {
		t.Error("moved out of the window")
	}
	g.resizePane(1, 0)
	if r := a.root.ratio; r < 0.54 || r > 0.56 {
		t.Errorf("resize: %v", r)
	}
	// the layout for the server
	b2, err := g.layoutOf(a)
	if err != nil || !strings.Contains(string(b2), `"split":"lr"`) || !strings.Contains(string(b2), `"session":"c"`) {
		t.Errorf("layout: %s %v", b2, err)
	}
	var sl serverLayout
	if json.Unmarshal(b2, &sl) != nil || sl.Version != 1 {
		t.Error("layout JSON")
	}
	// closing the leader hands the tab to another pane
	g.closePane(a)
	l := g.tabs[0]
	if l == a || l.lead != nil || names(l.panes()) != "b c" {
		t.Fatalf("after closing the leader: %q", names(l.panes()))
	}
	for _, p := range l.panes() {
		if p != l && p.lead != l {
			t.Error("lead not updated")
		}
	}
	g.closePane(c)
	if l.root != nil || len(l.panes()) != 1 {
		t.Error("one pane left: no tree")
	}
}

func TestPresets(t *testing.T) {
	g := &gui{}
	var ps []*tab
	for _, n := range []string{"1", "2", "3", "4", "5"} {
		ps = append(ps, mkTab(g, n))
	}
	for k := range presetNames {
		root := presetTree(ps, k)
		if got := names(root.leaves()); got != "1 2 3 4 5" {
			t.Errorf("preset %d: %q", k, got)
		}
	}
}

func TestParseControl(t *testing.T) {
	if a := parseControl([]byte("7799;split;tb")); len(a) != 2 || a[0] != "split" {
		t.Errorf("%v", a)
	}
	for _, bad := range []string{"7799", "7798;split", "7799;split;a b", "7799;" + strings.Repeat("x", 300), "7799;layout;../x/y"} {
		if parseControl([]byte(bad)) != nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestPaneStateRoundTrip(t *testing.T) {
	g := &gui{}
	a, b := mkTab(g, "a"), mkTab(g, "b")
	g.tabs = []*tab{a}
	g.insertPane(a, a, b, false, 0.3)
	ps := a.paneState()
	if ps == nil || ps.A == nil || !ps.A.Lead || ps.B.Tab == nil || ps.B.Tab.Session != "b" || !ps.B.Focus {
		t.Fatalf("%+v", ps)
	}
}

func names(ts []*tab) string {
	var s []string
	for _, t := range ts {
		s = append(s, t.session)
	}
	return strings.Join(s, " ")
}
