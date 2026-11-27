// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// envedit.go: editor of the environment variables of a session (a scrolling
// list of name/value rows) and the dialog that changes them on an open tab.

package main

import (
	"errors"
	"fmt"
	"strings"

	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

type envRow struct {
	name, value widget.Editor
	del         widget.Clickable
}

type envEditor struct {
	rows []*envRow
	list widget.List
	add  widget.Clickable
}

func newEnvEditor(vars []envVar) *envEditor {
	e := &envEditor{}
	e.list.Axis = layout.Vertical
	for _, v := range vars {
		r := e.newRow()
		r.name.SetText(v.Name)
		r.value.SetText(v.Value)
	}
	return e
}

func (e *envEditor) newRow() *envRow {
	r := &envRow{}
	r.name.SingleLine, r.value.SingleLine = true, true
	e.rows = append(e.rows, r)
	return r
}

// Values returns the variables typed in; rows with no name are ignored, a bad
// name is an error.
func (e *envEditor) Values() ([]envVar, error) {
	var out []envVar
	for _, r := range e.rows {
		n, v := strings.TrimSpace(r.name.Text()), r.value.Text()
		if n == "" && v == "" {
			continue
		}
		if !validEnvName(n) {
			return nil, fmt.Errorf("nome di variabile non valido: %q (lettere, cifre e _, senza cifra iniziale)", n)
		}
		if len(v) > 4096 || strings.ContainsAny(v, "\x00\r\n") {
			return nil, fmt.Errorf("valore non valido per %s (una sola riga, al massimo 4096 caratteri)", n)
		}
		out = append(out, envVar{n, v})
	}
	return cleanEnv(out), nil
}

// Layout draws the list in a box of limited height that scrolls.
func (e *envEditor) Layout(gtx layout.Context, g *gui) layout.Dimensions {
	for i := 0; i < len(e.rows); i++ {
		if e.rows[i].del.Clicked(gtx) {
			e.rows = append(e.rows[:i], e.rows[i+1:]...)
			i--
		}
	}
	if e.add.Clicked(gtx) {
		e.newRow()
		e.list.Position.First = max(len(e.rows)-1, 0) // show the new row
		e.list.Position.Offset = 0
	}
	box := gtx
	box.Constraints.Max.Y = min(gtx.Constraints.Max.Y, gtx.Dp(unit.Dp(150)))
	box.Constraints.Min.Y = 0
	var list layout.Dimensions
	if len(e.rows) == 0 {
		l := material.Label(g.th, 13, "nessuna variabile: la sessione usa quelle standard")
		l.Color = theme.chromeText
		list = l.Layout(gtx)
	} else {
		list = material.List(g.th, &e.list).Layout(box, len(e.rows), func(gtx layout.Context, i int) layout.Dimensions {
			r := e.rows[i]
			return layout.Inset{Bottom: 6}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(0.4, g.field(&r.name, "NOME")),
					layout.Rigid(layout.Spacer{Width: 6}.Layout),
					layout.Flexed(0.6, g.field(&r.value, "valore")),
					layout.Rigid(layout.Spacer{Width: 6}.Layout),
					layout.Rigid(g.smallButton(&r.del, "×", theme.input)),
				)
			})
		})
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return list }),
		layout.Rigid(layout.Spacer{Height: 6}.Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X = 0
			return layout.W.Layout(gtx, g.smallButton(&e.add, "+ Aggiungi variabile", theme.input))
		}),
	)
}

// ---- dialog on an open tab ----

type envDialog struct {
	t         *tab
	ed        *envEditor
	save, now widget.Clickable
	can       widget.Clickable
}

func (g *gui) openEnvDialog(t *tab) {
	g.setDialog(&envDialog{t: t, ed: newEnvEditor(t.getEnv())})
}

func (d *envDialog) Cancel() {}

func (d *envDialog) Layout(gtx layout.Context, g *gui) layout.Dimensions {
	if d.can.Clicked(gtx) {
		g.closeDialog()
	}
	apply := d.now.Clicked(gtx)
	if d.save.Clicked(gtx) || apply {
		vars, err := d.ed.Values()
		if err != nil {
			g.flash("%v", err)
		} else {
			d.t.mu.Lock()
			d.t.env = vars
			d.t.mu.Unlock()
			if apply {
				if d.t.ro {
					g.flash("la sessione e' in sola lettura")
				} else if err := d.t.typeText(exportLines(vars)); err != nil {
					g.flash("%v", err)
				}
			}
			g.saveWorkspace()
			g.closeDialog()
		}
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		g.row(gtx, g.label("Variabili d'ambiente della sessione \""+d.t.session+"\"")),
		g.row(gtx, g.label("Vengono impostate quando la sessione viene creata (anche di nuovo, dopo un riavvio del server). \"Applica ora\" le scrive anche nella shell aperta: usalo al prompt.")),
		g.row(gtx, func(gtx layout.Context) layout.Dimensions { return d.ed.Layout(gtx, g) }),
		g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Spacing: layout.SpaceStart}.Layout(gtx,
				layout.Rigid(g.smallButton(&d.can, "Annulla", theme.input)),
				layout.Rigid(layout.Spacer{Width: 8}.Layout),
				layout.Rigid(g.smallButton(&d.save, "Salva", theme.input)),
				layout.Rigid(layout.Spacer{Width: 8}.Layout),
				layout.Rigid(g.smallButton(&d.now, "Salva e applica ora", theme.accent)),
			)
		}),
	)
}

// typeText writes s to the session as if typed.
func (t *tab) typeText(s string) error {
	t.mu.Lock()
	term := t.term
	t.mu.Unlock()
	if term == nil {
		return errors.New("la sessione non e' collegata")
	}
	if s != "" {
		t.queue([]byte(s))
	}
	return nil
}
