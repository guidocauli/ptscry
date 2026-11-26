// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// combo.go: a searchable combo box: a text field that filters a list shown
// under it (the ▾ button opens it), so that a long list of connections,
// sessions or shells is not a column of radio buttons. Free text is allowed
// where it makes sense (a shell command, a key path).

package main

import (
	"strings"

	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

type comboItem struct {
	Label  string // shown and searched
	Detail string // shown smaller, searched too
	Value  string // what the choice means
}

type combo struct {
	ed      widget.Editor
	toggle  widget.Clickable
	list    widget.List
	clicks  []widget.Clickable
	items   []comboItem
	open    bool
	chosen  *comboItem // the item whose label is in the field
	hint    string
	empty   string // shown when there is nothing to list
	onPick  func(comboItem)
	lastTxt string
}

func newCombo(hint, empty string) *combo {
	c := &combo{hint: hint, empty: empty}
	c.ed.SingleLine = true
	c.ed.Submit = true
	c.list.Axis = layout.Vertical
	return c
}

func (c *combo) SetItems(items []comboItem) {
	c.items = items
	if len(c.clicks) < len(items) {
		c.clicks = make([]widget.Clickable, len(items))
	}
	if c.chosen != nil {
		c.chosen = nil
		for i := range c.items {
			if c.items[i].Label == strings.TrimSpace(c.ed.Text()) {
				c.chosen = &c.items[i]
			}
		}
	}
}

// Choose puts an item in the field as if it was clicked (without onPick).
func (c *combo) Choose(value string) bool {
	for i := range c.items {
		if c.items[i].Value == value {
			c.chosen = &c.items[i]
			c.ed.SetText(c.items[i].Label)
			c.lastTxt = c.ed.Text()
			return true
		}
	}
	return false
}

// Value is the chosen item's value, or the typed text when it is not an item.
func (c *combo) Value() string {
	t := strings.TrimSpace(c.ed.Text())
	if c.chosen != nil && c.chosen.Label == t {
		return c.chosen.Value
	}
	for _, it := range c.items {
		if it.Label == t {
			return it.Value
		}
	}
	return t
}

// Chosen tells whether the field holds one of the items.
func (c *combo) Chosen() (comboItem, bool) {
	t := strings.TrimSpace(c.ed.Text())
	for _, it := range c.items {
		if it.Label == t {
			return it, true
		}
	}
	return comboItem{}, false
}

func (c *combo) Clear() {
	c.ed.SetText("")
	c.lastTxt = ""
	c.chosen = nil
}

func (c *combo) filtered() []int {
	t := strings.ToLower(strings.TrimSpace(c.ed.Text()))
	if c.chosen != nil && strings.ToLower(c.chosen.Label) == t {
		t = "" // the field shows the choice: list everything
	}
	var out []int
	for i, it := range c.items {
		if t == "" || strings.Contains(strings.ToLower(it.Label+" "+it.Detail), t) {
			out = append(out, i)
		}
	}
	return out
}

func (c *combo) pick(g *gui, i int) {
	it := c.items[i]
	c.chosen = &c.items[i]
	c.ed.SetText(it.Label)
	c.lastTxt = c.ed.Text()
	c.open = false
	if c.onPick != nil {
		c.onPick(it)
	}
	g.invalidate()
}

func (c *combo) Layout(gtx layout.Context, g *gui) layout.Dimensions {
	for {
		ev, ok := c.ed.Update(gtx)
		if !ok {
			break
		}
		switch ev.(type) {
		case widget.ChangeEvent:
			if c.ed.Text() != c.lastTxt {
				c.lastTxt = c.ed.Text()
				c.open = true
			}
		case widget.SubmitEvent:
			if f := c.filtered(); c.open && len(f) > 0 {
				c.pick(g, f[0])
			}
		}
	}
	if c.toggle.Clicked(gtx) {
		c.open = !c.open
	}
	shown := c.filtered()
	for _, i := range shown {
		if c.clicks[i].Clicked(gtx) {
			c.pick(g, i)
			shown = c.filtered()
			break
		}
	}
	arrow := "▾"
	if c.open {
		arrow = "▴"
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				layout.Flexed(1, g.field(&c.ed, c.hint)),
				layout.Rigid(layout.Spacer{Width: 4}.Layout),
				layout.Rigid(g.smallButton(&c.toggle, " "+arrow+" ", theme.bg)),
			)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if !c.open {
				return layout.Dimensions{}
			}
			return layout.Inset{Top: 2}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				if len(shown) == 0 {
					l := material.Label(g.th, 13, c.empty)
					if len(c.items) > 0 {
						l.Text = "nessuna corrispondenza"
					}
					l.Color = theme.disabled
					return layout.UniformInset(6).Layout(gtx, l.Layout)
				}
				box := gtx
				box.Constraints.Max.Y = min(gtx.Constraints.Max.Y, gtx.Dp(unit.Dp(190)))
				box.Constraints.Min.Y = 0
				// a darker box, so that the list reads as a list
				macro := material.List(g.th, &c.list)
				return layout.Background{}.Layout(box,
					func(gtx layout.Context) layout.Dimensions {
						paint.FillShape(gtx.Ops, theme.bg, clip.Rect{Max: gtx.Constraints.Min}.Op())
						return layout.Dimensions{Size: gtx.Constraints.Min}
					},
					func(gtx layout.Context) layout.Dimensions {
						return macro.Layout(gtx, len(shown), func(gtx layout.Context, k int) layout.Dimensions {
							i := shown[k]
							it := c.items[i]
							return c.clicks[i].Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								hot := c.clicks[i].Hovered()
								return layout.Stack{}.Layout(gtx,
									layout.Expanded(func(gtx layout.Context) layout.Dimensions {
										if hot {
											paint.FillShape(gtx.Ops, theme.selection, clip.Rect{Max: gtx.Constraints.Min}.Op())
										}
										return layout.Dimensions{Size: gtx.Constraints.Min}
									}),
									layout.Stacked(func(gtx layout.Context) layout.Dimensions {
										gtx.Constraints.Min.X = gtx.Constraints.Max.X
										return layout.Inset{Left: 8, Right: 8, Top: 4, Bottom: 4}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
											return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Baseline}.Layout(gtx,
												layout.Rigid(func(gtx layout.Context) layout.Dimensions {
													l := material.Label(g.th, 14, it.Label)
													l.Color = theme.fg
													l.MaxLines = 1
													return l.Layout(gtx)
												}),
												layout.Rigid(layout.Spacer{Width: 10}.Layout),
												layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
													l := material.Label(g.th, 12, it.Detail)
													l.Color = theme.chromeText
													l.MaxLines = 1
													return l.Layout(gtx)
												}),
											)
										})
									}),
								)
							})
						})
					})
			})
		}),
	)
}
