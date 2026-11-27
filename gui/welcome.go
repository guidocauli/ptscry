// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// welcome.go: the "Benvenuto" page, as in RexDesk (what the program does,
// how to start, the useful keys, quick buttons), the quick guide (F1) and
// the keys of the window when no terminal has the keyboard.

package main

import (
	"image"
	"image/color"
	"strings"

	"gioui.org/font"
	"gioui.org/io/event"
	"gioui.org/io/key"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
	"golang.org/x/exp/shiny/materialdesign/icons"
)

type welcomeState struct {
	list                          widget.List
	addSrv, local, imp, guide, ns widget.Clickable
}

// rich is a line of text with **bold** parts.
func (g *gui) rich(s string, size float32) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		var spans []richSpan
		bold := false
		for i, part := range strings.Split(s, "**") {
			if i > 0 {
				bold = !bold
			}
			if part != "" {
				spans = append(spans, richSpan{part, bold})
			}
		}
		return g.richLayout(gtx, spans, size)
	}
}

type richSpan struct {
	text string
	bold bool
}

// richLayout lays out words with wrapping; good enough for the short
// paragraphs of the welcome page.
func (g *gui) richLayout(gtx layout.Context, spans []richSpan, size float32) layout.Dimensions {
	maxW := gtx.Constraints.Max.X
	x, y, lineH := 0, 0, 0
	for _, sp := range spans {
		words := strings.SplitAfter(sp.text, " ")
		for _, w := range words {
			if w == "" {
				continue
			}
			l := material.Label(g.th, dpSp(size), w)
			l.Color = theme.fg
			if sp.bold {
				l.Font.Weight = font.Bold
			}
			l.MaxLines = 1
			mg := gtx
			mg.Constraints = layout.Constraints{Max: image.Pt(maxW, gtx.Dp(100))}
			rec := recordWidget(mg, l.Layout)
			if x > 0 && x+rec.size.X > maxW {
				x, y = 0, y+lineH
				lineH = 0
			}
			rec.at(gtx, image.Pt(x, y))
			x += rec.size.X
			lineH = max(lineH, rec.size.Y)
		}
	}
	return layout.Dimensions{Size: image.Pt(maxW, y+lineH)}
}

func (g *gui) welcomePage(gtx layout.Context) {
	w := &g.welcome
	paint.FillShape(gtx.Ops, theme.chrome, clip.Rect{Max: gtx.Constraints.Max}.Op())
	if w.addSrv.Clicked(gtx) {
		g.openServerDialog("", "")
	}
	if w.local.Clicked(gtx) {
		g.home = false
		g.openTab("", "")
	}
	if w.imp.Clicked(gtx) {
		g.openImportDialog()
	}
	if w.guide.Clicked(gtx) {
		g.openGuide()
	}
	if w.ns.Clicked(gtx) {
		g.openNewTabDialog()
	}
	paras := []string{
		"Sessioni di terminale **persistenti e auditate**: chiudi la finestra o perdi la rete e i programmi continuano a girare sul server; li ritrovi da qui, da un altro computer o con `ptscry attach`.",
		"**Per cominciare**",
		"**1.** Aggiungi un server con **+** nella barra a sinistra, oppure **importa** quelli di RexDesk o di ~/.ssh/config.",
		"**2.** Organizza i server in **gruppi** (trascinali nell'albero) e segna i più usati come **Preferiti**; un **colore** per server o gruppo compare anche sui tab.",
		"**3.** Doppio clic su un server: si apre un tab con una **nuova sessione** (ptscry) o il terminale (SSH). Apri il server con la freccia per vedere le sue **sessioni**: doppio clic per ricollegarti, tasto destro per rinominarle, congelarle o terminarle.",
		"**4.** Dividi un tab in **pannelli** (Alt+Maiusc++ / Alt+Maiusc+-), anche di server diversi, con input sincronizzato; salva le disposizioni sul server.",
		"**5.** Il pannello **Attività** in basso registra connessioni, disconnessioni, errori e avvisi: un clic su una riga la copia.",
		"**Tasti utili**: Invio/doppio clic apre · F2 modifica · Canc elimina · Ctrl+F filtra l'elenco · Ctrl+Maiusc+T nuova sessione · Ctrl+Maiusc+S sessioni del server · Ctrl+Maiusc+W chiude il tab · Ctrl+Maiusc+B barra laterale · Ctrl+Maiusc+J attività · F11 schermo intero · F1 guida rapida (fuori dal terminale).",
	}
	w.list.Axis = layout.Vertical
	sz := gtx.Constraints.Max
	// the page: text that scrolls, quick buttons at the bottom (as in RexDesk)
	btnH := gtx.Dp(48)
	tg := gtx
	tg.Constraints = layout.Exact(image.Pt(sz.X, max(sz.Y-btnH, 1)))
	n := len(paras) + 3
	material.List(g.th, &w.list).Layout(tg, n, func(gtx layout.Context, i int) layout.Dimensions {
		return layout.Inset{Left: 14, Right: 14, Top: 4, Bottom: 4}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			switch {
			case i == 0:
				l := material.Label(g.th, 24, "ptscry")
				l.Font.Weight = font.Bold
				l.Color = theme.fg
				return layout.Inset{Top: 8}.Layout(gtx, l.Layout)
			case i == n-2:
				// a line, as in RexDesk
				paint.FillShape(gtx.Ops, theme.line, clip.Rect{Max: image.Pt(gtx.Constraints.Max.X, gtx.Dp(1))}.Op())
				return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, gtx.Dp(6))}
			case i == n-1:
				return g.rich("ptscry-gui "+guiVersion+" - Guido Cauli <guido.cauli@gmail.com> · software libero AGPL-3.0, **senza alcuna garanzia**", 12)(gtx)
			}
			return g.rich(strings.ReplaceAll(paras[i-1], "`", ""), 14)(gtx)
		})
	})
	// quick buttons
	bg := gtx
	bg.Constraints = layout.Exact(image.Pt(sz.X, btnH))
	st := opOffset(gtx, image.Pt(0, sz.Y-btnH))
	layout.Inset{Left: 12, Top: 8}.Layout(bg, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
			layout.Rigid(g.iconTextButton(&w.addSrv, "add", icons.ContentAdd, "Aggiungi server", true)),
			layout.Rigid(layout.Spacer{Width: 6}.Layout),
			layout.Rigid(g.iconTextButton(&w.imp, "import", icons.FileFileDownload, "Importa", true)),
			layout.Rigid(layout.Spacer{Width: 6}.Layout),
			layout.Rigid(g.iconTextButton(&w.ns, "open", icons.AVPlayArrow, "Nuova sessione…", false)),
			layout.Rigid(layout.Spacer{Width: 6}.Layout),
			layout.Rigid(g.iconTextButton(&w.guide, "help", icons.ActionHelp, "Guida rapida", false)),
		)
	})
	st()
}

// iconTextButton is a button with an icon, blue for the main actions (as in RexDesk).
func (g *gui) iconTextButton(c *widget.Clickable, name string, data []byte, label string, primary bool) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return c.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			bg := theme.input
			if primary {
				bg = theme.accent
			}
			if c.Hovered() {
				bg = mix(bg, theme.fg, 0.12)
			}
			return layout.Background{}.Layout(gtx,
				func(gtx layout.Context) layout.Dimensions {
					paint.FillShape(gtx.Ops, bg, clip.UniformRRect(image.Rectangle{Max: gtx.Constraints.Min}, gtx.Dp(4)).Op(gtx.Ops))
					return layout.Dimensions{Size: gtx.Constraints.Min}
				},
				func(gtx layout.Context) layout.Dimensions {
					return layout.Inset{Left: 8, Right: 10, Top: 6, Bottom: 6}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								s := gtx.Dp(16)
								gtx.Constraints = layout.Exact(image.Pt(s, s))
								if ic := g.side.icon(name, data); ic != nil {
									return ic.Layout(gtx, theme.accentFg)
								}
								return layout.Dimensions{Size: image.Pt(s, s)}
							}),
							layout.Rigid(layout.Spacer{Width: 6}.Layout),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								l := material.Label(g.th, 13, label)
								l.MaxLines = 1
								l.Color = theme.accentFg
								l.Font.Weight = font.SemiBold
								return layout.Inset{Top: 2}.Layout(gtx, l.Layout) // on the line of the icon
							}),
						)
					})
				})
		})
	}
}

// ---- the quick guide ----

type guideDialog struct {
	close widget.Clickable
}

func (g *gui) openGuide() { g.setDialog(&guideDialog{}) }

func (d *guideDialog) Cancel() {}

var guideText = []string{
	"**Barra laterale**",
	"Doppio clic o Invio su un server: nuovo tab (sessione nuova, shell o login SSH). Freccia o → apre il server e mostra le sue sessioni; doppio clic su una sessione per ricollegarti (se è già in un tab, ci va).",
	"Tasto destro: menu del server (nuova sessione con nome, sessioni, Preferiti, modifica, duplica, elimina), della sessione (apri, sola lettura, nuovo pannello, rinomina, congela, termina) e del gruppo.",
	"Trascina un server su un gruppo per spostarlo, su Preferiti per aggiungerlo, nello spazio vuoto per toglierlo dai gruppi. F2 modifica, Canc elimina, F5 aggiorna le sessioni, Ctrl+F filtra, Esc svuota il filtro.",
	"Stato: ● verde collegato, ● arancione in connessione, ○ tab aperto ma non collegato; per le sessioni ○ verde = collegata da un altro client.",
	"**Tab e pannelli**",
	"Ctrl+Maiusc+T nuova sessione · Ctrl+Maiusc+W chiude il tab (la sessione resta sul server) · Ctrl+PgSu/PgGiù cambia tab · Ctrl+Maiusc+R rinomina il tab · Ctrl+Maiusc+D si scollega · Ctrl+Maiusc+F congela.",
	"Alt+Maiusc++ / - divide · Alt+frecce cambia pannello · Alt+Maiusc+frecce ridimensiona · Alt+Maiusc+Z zoom · Alt+Maiusc+S input sincronizzato · Alt+Maiusc+L disposizioni.",
	"**Finestra**",
	"Ctrl+Maiusc+B barra laterale · Ctrl+Maiusc+J pannello Attività · Ctrl+Maiusc+N aggiungi server · F11 schermo intero · Ctrl +/-/0 carattere del terminale · Ctrl+Maiusc+C / V copia e incolla.",
	"Il manuale completo è in docs/GUI.md e in man ptscry-gui.",
}

func (d *guideDialog) Layout(gtx layout.Context, g *gui) layout.Dimensions {
	if d.close.Clicked(gtx) {
		g.closeDialog()
	}
	kids := []layout.FlexChild{g.row(gtx, g.title("Guida rapida"))}
	for _, p := range guideText {
		kids = append(kids, g.row(gtx, g.rich(p, 13)))
	}
	kids = append(kids, g.row(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Spacing: layout.SpaceStart}.Layout(gtx,
			layout.Rigid(g.smallButton(&d.close, "Chiudi", theme.accent)))
	}))
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
}

// ---- keys of the window ----

// appKey handles the keys of the window: from a terminal (which asks first)
// and when no terminal has the keyboard. true = done.
func (g *gui) appKey(name key.Name, mods key.Modifiers) bool {
	cs := mods&key.ModCtrl != 0 && mods&key.ModShift != 0 || mods&key.ModCommand != 0
	plain := mods&(key.ModCtrl|key.ModAlt|key.ModShift|key.ModCommand|key.ModSuper) == 0
	switch {
	case plain && name == key.NameF11:
		g.toggleFullscreen()
	case cs && name == "B":
		g.toggleSidebar()
	case cs && name == "J":
		g.toggleActivity()
	case cs && name == "N":
		g.openServerDialog("", "")
	default:
		return false
	}
	return true
}

// windowKeys: the keys that reach the window when no terminal (nor the
// tree, nor a field) takes them.
func (g *gui) windowKeys(gtx layout.Context, dialogOpen bool) {
	var fs []event.Filter
	for _, n := range []key.Name{key.NameF1, key.NameF11} {
		fs = append(fs, key.Filter{Name: n})
	}
	for _, n := range []key.Name{"B", "J", "N", "T", "S", "O", "W"} {
		fs = append(fs, key.Filter{Name: n, Required: key.ModShortcut | key.ModShift}, key.Filter{Name: n, Required: key.ModCommand})
	}
	for {
		ev, ok := gtx.Event(fs...)
		if !ok {
			break
		}
		e, ok := ev.(key.Event)
		if !ok || e.State != key.Press {
			continue
		}
		if dialogOpen && e.Name != key.NameF11 {
			continue // the keys of a dialog's fields must not open something else under it
		}
		if g.appKey(e.Name, e.Modifiers) {
			continue
		}
		switch e.Name {
		case key.NameF1:
			g.openGuide()
		case "T":
			g.openNewTabDialog()
		case "S":
			g.openSessionsDialog()
		case "O":
			g.openWorkspacesDialog()
		case "W":
			if len(g.tabs) > 0 && !g.home {
				g.closeTab(g.curTab())
			}
		}
	}
}

// opOffset moves the origin; the returned function restores it.
func opOffset(gtx layout.Context, p image.Point) func() { return op_offset(gtx, p) }

func dpSp(v float32) unit.Sp { return unit.Sp(v) }

type recorded struct {
	call op.CallOp
	size image.Point
}

// recordWidget lays w out without drawing it yet.
func recordWidget(gtx layout.Context, w layout.Widget) recorded {
	m := op.Record(gtx.Ops)
	d := w(gtx)
	return recorded{call: m.Stop(), size: d.Size}
}

func (r recorded) at(gtx layout.Context, p image.Point) {
	st := op.Offset(p).Push(gtx.Ops)
	r.call.Add(gtx.Ops)
	st.Pop()
}

// mix is a + (b - a) * t.
func mix(a, b color.NRGBA, t float32) color.NRGBA {
	f := func(x, y uint8) uint8 { return uint8(float32(x) + (float32(y)-float32(x))*t) }
	return color.NRGBA{R: f(a.R, b.R), G: f(a.G, b.G), B: f(a.B, b.B), A: a.A}
}
