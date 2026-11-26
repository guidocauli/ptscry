//go:build !windows

package main

import (
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
)

func TestTreePlaceAndRemove(t *testing.T) {
	a, b, c := &pane{name: "a"}, &pane{name: "b"}, &pane{name: "c"}
	root := leaf(a)
	root.split(b, true)
	root.fix(nil)
	root.find(b).split(c, false)
	root.fix(nil)
	root.place(uv.Rect(0, 0, 81, 25))
	ra, rb, rc := root.find(a).rect, root.find(b).rect, root.find(c).rect
	if ra.Dx()+rb.Dx()+1 != 81 || rb.Dy()+rc.Dy()+1 != 25 || ra.Max.X+1 != rb.Min.X {
		t.Errorf("rects %v %v %v", ra, rb, rc)
	}
	// tiny areas do not panic and every pane keeps a cell
	root.place(uv.Rect(0, 0, 3, 3))
	root = remove(root, b)
	if got := names(root.leaves()); got != "a c" {
		t.Errorf("after remove: %q", got)
	}
	root = remove(root, a)
	root = remove(root, c)
	if root != nil {
		t.Error("last pane removed: no tree")
	}
}

func TestLayoutRoundTrip(t *testing.T) {
	a, b := &pane{name: "web"}, &pane{name: "db"}
	root := leaf(a)
	root.split(b, false)
	root.fix(nil)
	data, err := encodeLayout(root, b)
	if err != nil {
		t.Fatal(err)
	}
	pl, err := parseLayout(data)
	if err != nil || pl.a == nil || pl.a.session != "web" || !pl.b.focus || pl.side {
		t.Fatalf("%s -> %+v %v", data, pl, err)
	}
	// panes of another connection and bad names are left out
	pl, err = parseLayout([]byte(`{"version":1,"root":{"split":"lr","a":{"session":"x","host":"other"},"b":{"session":"../y"}}}`))
	if err == nil {
		t.Errorf("accepted %+v", pl)
	}
	pl, err = parseLayout([]byte(`{"version":1,"root":{"split":"lr","ratio":7,"a":{"session":"x","host":"other"},"b":{"session":"ok"}}}`))
	if err != nil || pl.session != "ok" {
		t.Errorf("%+v %v", pl, err)
	}
	deep := strings.Repeat(`{"split":"lr","a":{"session":"s"},"b":`, 40) + `{"session":"z"}` + strings.Repeat("}", 40)
	if pl, err := parseLayout([]byte(`{"version":1,"root":` + deep + `}`)); err != nil || len(planSessions(pl)) > maxPanes {
		t.Errorf("deep layout: %v %d", err, len(planSessions(pl)))
	}
}

func planSessions(p *plan) []string {
	if p == nil {
		return nil
	}
	if p.a != nil {
		return append(planSessions(p.a), planSessions(p.b)...)
	}
	return []string{p.session}
}

func TestParseControlTUI(t *testing.T) {
	if a := parseControl([]byte("7799;session;abc")); len(a) != 2 {
		t.Error(a)
	}
	if parseControl([]byte("7799;split;$(id)")) != nil {
		t.Error("accepted a strange argument")
	}
}

func TestPresetsTUI(t *testing.T) {
	var ps []*pane
	for _, n := range []string{"1", "2", "3", "4", "5", "6", "7"} {
		ps = append(ps, &pane{name: n})
	}
	for k := range presetNames {
		r := preset(ps, k)
		r.fix(nil)
		r.place(uv.Rect(0, 0, 100, 40))
		if got := names(r.leaves()); got != "1 2 3 4 5 6 7" {
			t.Errorf("%d: %q", k, got)
		}
	}
}

func names(ps []*pane) string {
	var s []string
	for _, p := range ps {
		s = append(s, p.name)
	}
	return strings.Join(s, " ")
}
