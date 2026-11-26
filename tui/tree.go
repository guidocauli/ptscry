//go:build !windows

// ptscry-tui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// tree.go: the panes as a tree of splits, the preset layouts and the layout
// format kept on the server (the same as ptscry-gui).

package main

import (
	"encoding/json"
	"errors"
	"math"
	"regexp"

	uv "github.com/charmbracelet/ultraviolet"
)

var nameRe = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._-]{0,63}$`) // not "-...": an option on a command line

const maxPanes = 16

type node struct {
	p      *pane
	side   bool // a | b side by side, else a over b
	ratio  float32
	a, b   *node
	parent *node
	rect   uv.Rectangle // last laid out
	div    uv.Rectangle // the divider (splits)
}

func leaf(p *pane) *node { return &node{p: p} }

func (n *node) leaves() []*pane {
	if n == nil {
		return nil
	}
	if n.p != nil {
		return []*pane{n.p}
	}
	return append(n.a.leaves(), n.b.leaves()...)
}

func (n *node) find(p *pane) *node {
	if n == nil {
		return nil
	}
	if n.p == p {
		return n
	}
	if n.p != nil {
		return nil
	}
	if f := n.a.find(p); f != nil {
		return f
	}
	return n.b.find(p)
}

func (n *node) fix(parent *node) {
	if n == nil {
		return
	}
	n.parent = parent
	n.a.fix(n)
	n.b.fix(n)
}

// split puts q next to the pane of n (n becomes a split).
func (n *node) split(q *pane, side bool) {
	old := &node{p: n.p, a: n.a, b: n.b, side: n.side, ratio: n.ratio}
	*n = node{side: side, ratio: 0.5, a: old, b: leaf(q), parent: n.parent}
}

// remove takes the pane p out; the root may change.
func remove(root *node, p *pane) *node {
	n := root.find(p)
	if n == nil {
		return root
	}
	if n.parent == nil {
		return nil
	}
	par := n.parent
	other := par.a
	if other == n {
		other = par.b
	}
	*par = node{p: other.p, a: other.a, b: other.b, side: other.side, ratio: other.ratio, parent: par.parent}
	root.fix(nil)
	return root
}

// place lays the tree out in r: each pane gets its rectangle, the splits a
// divider of one cell.
func (n *node) place(r uv.Rectangle) {
	n.rect = r
	if n.p != nil {
		return
	}
	ra, rb := r, r
	if n.side && r.Dx() < 3 || !n.side && r.Dy() < 3 {
		// no room for two panes and a divider: the first one gets it all
		n.div = uv.Rectangle{}
		rb = uv.Rectangle{Min: r.Max, Max: r.Max}
		n.a.place(ra)
		n.b.place(rb)
		return
	}
	if n.side {
		w := r.Dx() - 1
		x := r.Min.X + max(1, min(w-1, int(math.Round(float64(float32(w)*n.ratio)))))
		ra.Max.X = x
		n.div = uv.Rect(x, r.Min.Y, 1, r.Dy())
		rb.Min.X = x + 1
	} else {
		h := r.Dy() - 1
		y := r.Min.Y + max(1, min(h-1, int(math.Round(float64(float32(h)*n.ratio)))))
		ra.Max.Y = y
		n.div = uv.Rect(r.Min.X, y, r.Dx(), 1)
		rb.Min.Y = y + 1
	}
	n.a.place(ra)
	n.b.place(rb)
}

var presetNames = []string{"side by side", "stacked", "main left", "main top", "tiled"}

func even(ps []*pane, side bool) *node {
	if len(ps) == 1 {
		return leaf(ps[0])
	}
	return &node{side: side, ratio: 1 / float32(len(ps)), a: leaf(ps[0]), b: even(ps[1:], side)}
}

func preset(ps []*pane, k int) *node {
	switch k {
	case 0:
		return even(ps, true)
	case 1:
		return even(ps, false)
	case 2:
		return &node{side: true, ratio: 0.6, a: leaf(ps[0]), b: even(ps[1:], false)}
	case 3:
		return &node{side: false, ratio: 0.6, a: leaf(ps[0]), b: even(ps[1:], true)}
	}
	per := int(math.Ceil(math.Sqrt(float64(len(ps)))))
	var rows []*node
	for i := 0; i < len(ps); i += per {
		rows = append(rows, even(ps[i:min(i+per, len(ps))], true))
	}
	var stack func(r []*node) *node
	stack = func(r []*node) *node {
		if len(r) == 1 {
			return r[0]
		}
		return &node{side: false, ratio: 1 / float32(len(r)), a: r[0], b: stack(r[1:])}
	}
	return stack(rows)
}

// ---------------------------------------------------------------- the layout on the server

type serverLayout struct {
	Version int        `json:"version"`
	Root    *layoutDoc `json:"root"`
}

type layoutDoc struct {
	Split   string     `json:"split,omitempty"` // "lr" side by side, "tb" one over the other
	Ratio   float32    `json:"ratio,omitempty"`
	A       *layoutDoc `json:"a,omitempty"`
	B       *layoutDoc `json:"b,omitempty"`
	Session string     `json:"session,omitempty"`
	Host    string     `json:"host,omitempty"` // a pane of another connection (ptscry-gui): skipped here
	Focus   bool       `json:"focus,omitempty"`
}

func docOf(n *node, focus *pane) *layoutDoc {
	if n.p != nil {
		return &layoutDoc{Session: n.p.session(), Focus: n.p == focus}
	}
	s := "tb"
	if n.side {
		s = "lr"
	}
	return &layoutDoc{Split: s, Ratio: n.ratio, A: docOf(n.a, focus), B: docOf(n.b, focus)}
}

func encodeLayout(root *node, focus *pane) ([]byte, error) {
	b, err := json.Marshal(serverLayout{Version: 1, Root: docOf(root, focus)})
	if err == nil && len(b) > 16*1024 {
		err = errors.New("layout too large")
	}
	return b, err
}

// plan is a layout read from the server, before its panes are opened.
type plan struct {
	side    bool
	ratio   float32
	a, b    *plan
	session string
	focus   bool
}

// parseLayout reads a layout; panes of other connections and bad names are
// left out, and at most maxPanes are kept.
func parseLayout(data []byte) (*plan, error) {
	var sl serverLayout
	if err := json.Unmarshal(data, &sl); err != nil || sl.Root == nil {
		return nil, errors.New("not a valid layout")
	}
	count := 0
	var conv func(d *layoutDoc, depth int) *plan
	conv = func(d *layoutDoc, depth int) *plan {
		if d == nil || depth > 20 {
			return nil
		}
		if d.A != nil && d.B != nil {
			a, b := conv(d.A, depth+1), conv(d.B, depth+1)
			switch {
			case a == nil:
				return b
			case b == nil:
				return a
			}
			r := d.Ratio
			if r <= 0.05 || r >= 0.95 {
				r = 0.5
			}
			return &plan{side: d.Split == "lr", ratio: r, a: a, b: b}
		}
		if d.Host != "" || !nameRe.MatchString(d.Session) || count >= maxPanes {
			return nil
		}
		count++
		return &plan{session: d.Session, focus: d.Focus}
	}
	p := conv(sl.Root, 0)
	if p == nil {
		return nil, errors.New("the layout has no session that can be opened here")
	}
	return p, nil
}
