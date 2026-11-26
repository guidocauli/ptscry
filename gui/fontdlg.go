// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// fontdlg.go: the choice of the terminal font among the monospaced fonts of
// the system, with a preview drawn with the font file itself (so it is the
// real thing on Linux, Windows and macOS alike).

package main

import (
	"image/color"
	"sync"

	"gioui.org/font"
	"gioui.org/font/gofont"
	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

const fontSample = "AaBbCc 0O1lI| {}[]() <= != -> ~/$ #\n" +
	"àèéìòù ÀÉ € £ ✓ ✗ … «» ─│┌┐└┘├┤ ▁▃▅▇█\n" +
	"$ ls -la /etc/ptscry && echo \"ok\""

// fontPick is what the dialog gives back: the family and its file.
type fontPick struct{ Name, Path string }

type fontDialog struct {
	filter   widget.Editor
	list     widget.List
	rows     []widget.Clickable
	use      widget.Clickable
	can      widget.Clickable
	dflt     widget.Clickable
	pick     func(fontPick)
	mu       sync.Mutex
	all      []monoFont
	ready    bool
	scanning bool
	stale    bool
	selName  string // the selected font, kept across a new list
	current  string
	sel      int // index in all, -1 = Go Mono

	prevFor    string
	prevShaper *text.Shaper
	prevFace   font.Typeface
	prevErr    string
}

// openFontDialog shows the list over the open dialog and goes back to it.
func (g *gui) openFontDialog(current string, pick func(fontPick)) {
	d := &fontDialog{pick: pick, current: current, sel: -1, selName: current}
	d.filter.SingleLine = true
	d.list.Axis = layout.Vertical
	set := func(l []monoFont, done bool) {
		d.mu.Lock()
		d.all, d.ready, d.scanning = l, true, !done
		d.stale = true // the catalog and the selection follow the new list
		d.mu.Unlock()
		g.invalidate()
	}
	monoMu.Lock()
	cached := monoCache
	monoMu.Unlock()
	if cached != nil {
		set(cached, false) // at once, then the fresh list
	}
	go func() { set(rescanMonoFonts(), true) }()
	g.dlgMu.Lock()
	if g.dlg != nil {
		g.dlgStack = append(g.dlgStack, g.dlg)
	}
	g.dlgMu.Unlock()
	g.setDialog(d)
}

func (d *fontDialog) Cancel() {}

// preview picks the face of the selected font in the catalog shaper: one
// shaper for the whole dialog, holding every listed font file (and Go Mono
// for what a font lacks).
func (d *fontDialog) preview(f monoFont) {
	key := f.Name + "\x00" + f.Path
	if d.prevFor == key {
		return
	}
	d.prevFor, d.prevErr, d.prevFace = key, "", ""
	if f.Path == "" {
		d.prevErr = "file del carattere sconosciuto: l'anteprima usa la ricerca per nome del sistema"
		return
	}
	faces, err := loadFontFile(f.Path)
	if err != nil {
		d.prevErr = "anteprima non disponibile: " + err.Error()
		return
	}
	d.prevFace = faces[0].Font.Typeface
}

// catalog builds (once) the shaper with all the fonts of the list.
func (d *fontDialog) catalog(all []monoFont) *text.Shaper {
	if d.prevShaper != nil {
		return d.prevShaper
	}
	coll := gofont.Collection()
	for _, f := range all {
		if f.Path == "" {
			continue
		}
		if faces, err := loadFontFile(f.Path); err == nil {
			coll = append(coll, faces...)
		}
	}
	d.prevShaper = text.NewShaper(text.NoSystemFonts(), text.WithCollection(coll))
	return d.prevShaper
}

func (d *fontDialog) Layout(gtx layout.Context, g *gui) layout.Dimensions {
	d.mu.Lock()
	all, ready, scanning, stale := d.all, d.ready, d.scanning, d.stale
	d.stale = false
	d.mu.Unlock()
	if stale {
		d.prevShaper, d.prevFor = nil, ""
		d.sel = -1
		for i, f := range all {
			if f.Name == d.selName {
				d.sel = i
			}
		}
	}
	if d.can.Clicked(gtx) {
		g.closeDialog()
		return layout.Dimensions{}
	}
	if d.dflt.Clicked(gtx) {
		d.pick(fontPick{})
		g.closeDialog()
		return layout.Dimensions{}
	}
	if d.use.Clicked(gtx) && d.sel >= 0 && d.sel < len(all) {
		f := all[d.sel]
		d.preview(f)
		name := f.Name
		if d.prevFace != "" {
			name = string(d.prevFace) // the name the font file gives itself
		}
		d.pick(fontPick{Name: name, Path: f.Path})
		g.closeDialog()
		return layout.Dimensions{}
	}
	shown := filterFonts(all, d.filter.Text())
	idx := map[string]int{}
	for i, f := range all {
		idx[f.Name] = i
	}
	if len(d.rows) < len(all) {
		d.rows = make([]widget.Clickable, len(all))
	}
	for _, f := range shown {
		i := idx[f.Name]
		if d.rows[i].Clicked(gtx) {
			d.sel, d.selName = i, f.Name
		}
	}
	selName := "Go Mono (incluso)"
	if d.sel >= 0 && d.sel < len(all) {
		d.preview(all[d.sel])
		selName = all[d.sel].Name
	}
	box := gtx
	box.Constraints.Max.Y = min(gtx.Constraints.Max.Y, gtx.Dp(unit.Dp(200)))
	box.Constraints.Min.Y = 0
	kids := []layout.FlexChild{
		g.row(gtx, g.label("Carattere del terminale: font a spaziatura fissa installati nel sistema")),
		g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			if !scanning {
				return layout.Dimensions{}
			}
			return small(g, "aggiornamento dell'elenco (font installati di recente)…")(gtx)
		}),
		g.row(gtx, g.field(&d.filter, "filtra per nome")),
		g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			switch {
			case !ready:
				return g.label("ricerca dei font in corso...")(gtx)
			case len(shown) == 0:
				return g.label("nessun font trovato (resta Go Mono)")(gtx)
			}
			return material.List(g.th, &d.list).Layout(box, len(shown), func(gtx layout.Context, k int) layout.Dimensions {
				f := shown[k]
				i := idx[f.Name]
				on := i == d.sel
				return d.rows[i].Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.Stack{}.Layout(gtx,
						layout.Expanded(func(gtx layout.Context) layout.Dimensions {
							if on {
								paint.FillShape(gtx.Ops, theme.selection, clip.Rect{Max: gtx.Constraints.Min}.Op())
							} else if d.rows[i].Hovered() {
								paint.FillShape(gtx.Ops, theme.bg, clip.Rect{Max: gtx.Constraints.Min}.Op())
							}
							return layout.Dimensions{Size: gtx.Constraints.Min}
						}),
						layout.Stacked(func(gtx layout.Context) layout.Dimensions {
							gtx.Constraints.Min.X = gtx.Constraints.Max.X
							return layout.Inset{Left: 8, Top: 3, Bottom: 3}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								l := material.Label(g.th, 14, f.Name)
								l.Color = theme.fg
								if f.Name == d.current {
									l.Text = "● " + f.Name + "   (in uso)"
								}
								return l.Layout(gtx)
							})
						}),
					)
				})
			})
		}),
		g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			l := material.Label(g.th, 13, "Anteprima: "+selName+"   (se resta vuota, questo carattere non si puo' usare qui)")
			l.Color = theme.accent
			l.Font.Weight = font.Bold
			return l.Layout(gtx)
		}),
		g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			// the sample on the terminal's own colours
			return layout.Stack{}.Layout(gtx,
				layout.Expanded(func(gtx layout.Context) layout.Dimensions {
					paint.FillShape(gtx.Ops, theme.bg, clip.Rect{Max: gtx.Constraints.Min}.Op())
					return layout.Dimensions{Size: gtx.Constraints.Min}
				}),
				layout.Stacked(func(gtx layout.Context) layout.Dimensions {
					gtx.Constraints.Min.X = gtx.Constraints.Max.X
					return layout.UniformInset(8).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						l := material.Label(g.th, unit.Sp(g.set.FontSize), fontSample)
						l.Color = theme.fg
						l.Font.Typeface = "Go Mono"
						if d.sel >= 0 && d.prevFace != "" {
							l.Shaper = d.catalog(all)
							l.Font.Typeface = d.prevFace + ", Go Mono"
						} else if d.sel >= 0 {
							l.Font.Typeface = font.Typeface(selName + ", Go Mono")
						}
						return l.Layout(gtx)
					})
				}),
			)
		}),
		g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			if d.prevErr == "" || d.sel < 0 {
				return layout.Dimensions{}
			}
			l := material.Label(g.th, 12, d.prevErr)
			l.Color = color.NRGBA{R: 0xc0, G: 0xa0, B: 0x60, A: 0xff}
			return l.Layout(gtx)
		}),
		g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
				layout.Rigid(g.smallButton(&d.dflt, "Go Mono (predefinito)", theme.bg)),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions { return layout.Dimensions{Size: gtx.Constraints.Min} }),
				layout.Rigid(g.smallButton(&d.can, "Annulla", theme.bg)),
				layout.Rigid(layout.Spacer{Width: 6}.Layout),
				layout.Rigid(g.smallButton(&d.use, "Usa questo carattere", theme.accent)),
			)
		}),
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
}
