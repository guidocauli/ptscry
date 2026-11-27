// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// winmenu.go: the main dropdown menu and the window buttons (minimize,
// maximize/restore, close) for windows without a system title bar.

package main

import (
	"image"
	"image/color"
	"runtime"
	"time"

	gioapp "gioui.org/app"
	"gioui.org/io/clipboard"
	"gioui.org/io/event"
	"gioui.org/io/pointer"
	"gioui.org/io/system"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// winState is what the window knows about itself.
type winState struct {
	frameless   bool // no system title bar
	mode        gioapp.WindowMode
	menuBtn     widget.Clickable
	minBtn      widget.Clickable
	maxBtn      widget.Clickable
	winCloseBtn widget.Clickable
	barBtn      []widget.Clickable // the labels of the menu bar
	menuOwner   int                // which label of the menu bar opened the menu (1-based), 0 = none
	menubarH    int
	menuHot     image.Rectangle // where a press must not close the menu (its own opener)
	dragTag     byte
	lastDrag    time.Time
}

// shortcut label: Command on macOS, Ctrl+Maiusc elsewhere.
func sc(k string) string {
	if runtime.GOOS == "darwin" {
		return "⌘" + k
	}
	return "Ctrl+Maiusc+" + k
}

// showWindowButtons tells whether our own minimize/maximize/close are drawn.
func (g *gui) showWindowButtons() bool {
	g.setMu.Lock()
	v := g.set.WinButtons
	g.setMu.Unlock()
	switch v {
	case "on":
		return true
	case "off":
		return false
	}
	return g.frameless
}

func (g *gui) toggleMaximize() {
	want := gioapp.Maximized
	if g.mode == gioapp.Maximized {
		want = gioapp.Windowed
		g.win.Perform(system.ActionUnmaximize)
	} else {
		g.win.Perform(system.ActionMaximize)
	}
	// the compositor has the last word (the window takes the mode it reports):
	// say so when it did not do it, instead of a button that does nothing
	time.AfterFunc(1200*time.Millisecond, func() {
		g.runOnUI(func() {
			if want == gioapp.Maximized && g.mode != gioapp.Maximized {
				msg := "il gestore delle finestre non ha ingrandito la finestra"
				if tilingWM() {
					msg += " (Niri lo fa dalla 25.11; prima: Mod+F allarga la colonna, Mod+Maiusc+F schermo intero)"
				}
				g.flash("%s", msg)
			}
		})
	})
}

func (g *gui) toggleFullscreen() {
	if g.mode == gioapp.Fullscreen {
		g.win.Option(gioapp.Windowed.Option())
	} else {
		g.win.Option(gioapp.Fullscreen.Option())
	}
}

type menuGroup struct {
	name  string
	items []menuItem
}

// menuGroups lists the functions of the program, in groups: they are the
// drop-down menus of the menu bar, and (joined) the content of the ☰ menu.
func (g *gui) menuGroups() []menuGroup {
	var cur *tab
	if len(g.tabs) > 0 && !g.home {
		cur = g.curTab()
	}
	have := cur != nil
	many := len(g.tabs) > 1
	sel := have && cur.model.SelectedText() != ""
	sep := menuItem{sep: true}
	maxLabel := "Ingrandisci finestra"
	if g.mode == gioapp.Maximized {
		maxLabel = "Ripristina finestra"
	}
	fsLabel := "Schermo intero"
	if g.mode == gioapp.Fullscreen {
		fsLabel = "Esci dallo schermo intero"
	}
	freezeLabel := "Congela la sessione"
	if have && cur.isFrozen() {
		freezeLabel = "Scongela la sessione"
	}
	barLabel := "Nascondi la barra dei menu"
	if !g.menuBarOn() {
		barLabel = "Mostra la barra dei menu"
	}
	tabs := []menuItem{
		{label: "Tab successivo", enabled: many, key: "Ctrl+PgGiù", do: func(layout.Context) { g.cycle(1) }},
		{label: "Tab precedente", enabled: many, key: "Ctrl+PgSu", do: func(layout.Context) { g.cycle(-1) }},
	}
	if len(g.tabs) > 0 {
		tabs = append(tabs, sep)
		for i, t := range g.tabs {
			i, t := i, t
			mark := "   "
			if i == g.active {
				mark = "● "
			}
			tabs = append(tabs, menuItem{label: mark + t.title(), enabled: true, do: func(layout.Context) { g.active = i; g.saveWorkspace() }})
		}
	}
	return []menuGroup{
		{"File", []menuItem{
			{label: "Nuova sessione…", enabled: true, key: sc("T"), do: func(layout.Context) { g.openNewTabDialog() }},
			{label: "Importa server…", enabled: true, do: func(layout.Context) { g.openImportDialog() }},
			{label: "Rinomina tab…", enabled: have, key: sc("R"), do: func(layout.Context) { g.openRenameDialog(cur) }},
			{label: "Chiudi tab", enabled: have, do: func(layout.Context) { g.closeTab(cur) }},
			sep,
			{label: "Gestione ptscry…", enabled: true, do: func(layout.Context) { g.openManageDialog() }},
			{label: "Chiavi private…", enabled: true, do: func(layout.Context) { g.openKeysDialog() }},
			{label: "Impostazioni…", enabled: true, do: func(layout.Context) { g.openSettingsDialog() }},
			sep,
			{label: "Esci", enabled: true, do: func(layout.Context) { g.win.Perform(system.ActionClose) }},
		}},
		{"Server", g.serverMenu()},
		{"Tab", tabs},
		{"Pannelli", g.paneMenu(cur)},
		{"Sessione", []menuItem{
			{label: "Sessioni del server…", enabled: true, key: sc("S"), do: func(layout.Context) { g.openSessionsDialog() }},
			sep,
			{label: "Disconnetti dalla sessione", enabled: have, key: sc("D"), do: func(layout.Context) { g.detachTab(cur) }},
			{label: freezeLabel, enabled: have, key: sc("F"), do: func(layout.Context) { g.toggleFreeze(cur) }},
			sep,
			{label: "Variabili d'ambiente…", enabled: have, key: sc("E"), do: func(layout.Context) { g.openEnvDialog(cur) }},
			{label: "Workspace…", enabled: true, key: sc("O"), do: func(layout.Context) { g.openWorkspacesDialog() }},
		}},
		{"Modifica", []menuItem{
			{label: "Copia", enabled: sel, key: sc("C"), do: func(gtx layout.Context) { cur.view.copySelection(gtx) }},
			{label: "Incolla", enabled: have, key: sc("V"), do: func(gtx layout.Context) {
				g.pasteTarget = func(s string) { cur.view.paste(s) }
				gtx.Execute(clipboard.ReadCmd{Tag: &g.menuTag})
			}},
		}},
		{"Vista", []menuItem{
			{label: "Ingrandisci il carattere del terminale", enabled: true, key: "Ctrl++", do: func(layout.Context) { g.zoom(1) }},
			{label: "Riduci il carattere del terminale", enabled: true, key: "Ctrl+-", do: func(layout.Context) { g.zoom(-1) }},
			{label: "Carattere del terminale normale", enabled: true, key: "Ctrl+0", do: func(layout.Context) { g.zoom(0) }},
			{label: "Scegli il carattere del terminale...", enabled: true, do: func(layout.Context) { g.chooseTermFont() }},
			sep,
			{label: "Ingrandisci l'interfaccia", enabled: true, do: func(layout.Context) { g.uiZoom(10) }},
			{label: "Riduci l'interfaccia", enabled: true, do: func(layout.Context) { g.uiZoom(-10) }},
			{label: "Interfaccia a dimensione normale", enabled: true, do: func(layout.Context) { g.uiZoom(0) }},
			sep,
			{label: map[bool]string{true: "Nascondi la barra dei server", false: "Mostra la barra dei server"}[g.sidebarOn()], enabled: true, key: sc("B"), do: func(layout.Context) { g.toggleSidebar() }},
			{label: map[bool]string{true: "Nascondi il pannello Attività", false: "Mostra il pannello Attività"}[g.activityOn()], enabled: true, key: sc("J"), do: func(layout.Context) { g.toggleActivity() }},
			{label: barLabel, enabled: true, do: func(layout.Context) { g.toggleMenuBar() }},
			{label: map[bool]string{true: "Nascondi la barra di stato", false: "Mostra la barra di stato"}[g.statusBarOn()], enabled: true, do: func(layout.Context) { g.toggleStatusBar() }},
			{label: fsLabel, enabled: true, key: "F11", do: func(layout.Context) { g.toggleFullscreen() }},
		}},
		{"Finestra", []menuItem{
			{label: maxLabel, enabled: true, do: func(layout.Context) { g.toggleMaximize() }},
			{label: "Riduci a icona", enabled: true, do: func(layout.Context) { g.win.Perform(system.ActionMinimize) }},
		}},
		{"Aiuto", []menuItem{
			{label: "Benvenuto", enabled: true, do: func(layout.Context) { g.home = true }},
			{label: "Guida rapida", enabled: true, key: "F1", do: func(layout.Context) { g.openGuide() }},
			{label: "Informazioni…", enabled: true, do: func(layout.Context) {
				g.flashInfo("ptscry-gui %s - Guido Cauli <guido.cauli@gmail.com> · licenza AGPL-3.0, software libero SENZA ALCUNA GARANZIA (LICENSE, NOTICE.md)", guiVersion)
			}},
		}},
	}
}

// mainMenu is the ☰ menu: every group, one after the other (when the menu bar is hidden).
func (g *gui) mainMenu() []menuItem {
	var out []menuItem
	for i, gr := range g.menuGroups() {
		if i > 0 {
			out = append(out, menuItem{sep: true})
		}
		out = append(out, menuItem{label: gr.name, head: true, enabled: false})
		out = append(out, gr.items...)
	}
	return out
}

func (g *gui) uiZoom(d int) {
	g.setMu.Lock()
	if d == 0 {
		g.set.UIScale = 100
	} else {
		g.set.UIScale = min(max(g.set.UIScale+d, 60), 300)
	}
	g.setMu.Unlock()
	g.saveWorkspace()
	g.invalidate()
}

func (g *gui) toggleMenuBar() {
	g.setMu.Lock()
	if g.set.MenuBar == "off" {
		g.set.MenuBar = "on"
	} else {
		g.set.MenuBar = "off"
	}
	g.setMu.Unlock()
	g.saveWorkspace()
	g.invalidate()
}

// menuBar draws the drop-down menus along the top: File, Tab, Sessione...
func (g *gui) menuBar(gtx layout.Context) {
	paint.FillShape(gtx.Ops, theme.chrome, clip.Rect{Max: gtx.Constraints.Max}.Op())
	groups := g.menuGroups()
	if len(g.barBtn) != len(groups) {
		g.barBtn = make([]widget.Clickable, len(groups))
	}
	x := 0
	for i, gr := range groups {
		i, gr := i, gr
		b := &g.barBtn[i]
		st := op.Offset(image.Pt(x, 0)).Push(gtx.Ops)
		bg := gtx
		bg.Constraints.Min = image.Pt(0, gtx.Constraints.Max.Y)
		clicked := b.Clicked(bg)
		open := g.menu.open && g.menuOwner == i+1
		d := b.Layout(bg, func(gtx layout.Context) layout.Dimensions {
			if open || b.Hovered() {
				paint.FillShape(gtx.Ops, theme.hover, clip.Rect{Max: gtx.Constraints.Max}.Op())
			}
			l := material.Label(g.th, 13, gr.name)
			l.Color = theme.fg
			dd := layout.Inset{Left: 10, Right: 10, Top: 4}.Layout(gtx, l.Layout)
			dd.Size.Y = gtx.Constraints.Max.Y
			return dd
		})
		st.Pop()
		switch {
		case clicked && open:
			g.menu.open = false
		case clicked, b.Hovered() && g.menu.open && g.menuOwner > 0 && !open:
			g.menu = ctxMenu{open: true, pos: image.Pt(x, gtx.Constraints.Max.Y), items: gr.items,
				clk: make([]widget.Clickable, len(gr.items)), width: 300}
			g.menuOwner = i + 1
			g.invalidate()
		}
		x += d.Size.X
	}
}

func (g *gui) openMainMenu(x, y int) {
	items := g.mainMenu()
	g.menu = ctxMenu{open: true, pos: image.Pt(x, y), items: items,
		clk: make([]widget.Clickable, len(items)), width: 300}
	g.menuOwner = 0
	g.invalidate()
}

// menuButton is the "☰" at the left of the bar.
func (g *gui) menuButton(gtx layout.Context) layout.Dimensions {
	clicked := g.menuBtn.Clicked(gtx)
	d := g.iconButton(gtx, &g.menuBtn, "☰")
	if clicked {
		if g.menu.open {
			g.menu.open = false
		} else {
			g.openMainMenu(g.menuHot.Min.X, g.menuHot.Max.Y) // below the button
		}
	}
	return d
}

// windowButton draws one of the three buttons: kind is "min", "max" or "close".
func (g *gui) windowButton(gtx layout.Context, b *widget.Clickable, kind string) layout.Dimensions {
	return b.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		sz := image.Pt(gtx.Dp(40), gtx.Constraints.Max.Y)
		gtx.Constraints.Min, gtx.Constraints.Max = sz, sz
		if b.Hovered() {
			col := theme.hover
			if kind == "close" {
				col = color.NRGBA{R: 0xc4, G: 0x2b, B: 0x1c, A: 0xff}
			}
			paint.FillShape(gtx.Ops, col, clip.Rect{Max: sz}.Op())
		}
		c := image.Pt(sz.X/2, sz.Y/2)
		u := gtx.Dp(5)
		line := func(r image.Rectangle) { paint.FillShape(gtx.Ops, theme.fg, clip.Rect(r).Op()) }
		w := max(gtx.Dp(1), 1)
		switch kind {
		case "min":
			line(image.Rect(c.X-u, c.Y+u-w, c.X+u, c.Y+u))
		case "max":
			if g.mode == gioapp.Maximized {
				o := gtx.Dp(2)
				// two overlapped squares: restore
				box := func(r image.Rectangle) {
					line(image.Rect(r.Min.X, r.Min.Y, r.Max.X, r.Min.Y+w))
					line(image.Rect(r.Min.X, r.Max.Y-w, r.Max.X, r.Max.Y))
					line(image.Rect(r.Min.X, r.Min.Y, r.Min.X+w, r.Max.Y))
					line(image.Rect(r.Max.X-w, r.Min.Y, r.Max.X, r.Max.Y))
				}
				box(image.Rect(c.X-u, c.Y-u+o, c.X+u-o, c.Y+u))
				line(image.Rect(c.X-u+o, c.Y-u, c.X+u, c.Y-u+w))
				line(image.Rect(c.X+u-w, c.Y-u, c.X+u, c.Y+u-o))
			} else {
				line(image.Rect(c.X-u, c.Y-u, c.X+u, c.Y-u+w))
				line(image.Rect(c.X-u, c.Y+u-w, c.X+u, c.Y+u))
				line(image.Rect(c.X-u, c.Y-u, c.X-u+w, c.Y+u))
				line(image.Rect(c.X+u-w, c.Y-u, c.X+u, c.Y+u))
			}
		case "close":
			// an X drawn with small squares along the diagonals
			for i := -u; i <= u; i++ {
				line(image.Rect(c.X+i, c.Y+i, c.X+i+w, c.Y+i+w))
				line(image.Rect(c.X+i, c.Y-i-w, c.X+i+w, c.Y-i))
			}
		}
		return layout.Dimensions{Size: sz}
	})
}

// windowButtons is the group at the right end of the bar.
func (g *gui) windowButtons(gtx layout.Context) layout.Dimensions {
	if g.minBtn.Clicked(gtx) {
		g.win.Perform(system.ActionMinimize)
	}
	if g.maxBtn.Clicked(gtx) {
		g.toggleMaximize()
	}
	if g.winCloseBtn.Clicked(gtx) {
		g.win.Perform(system.ActionClose)
	}
	return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return g.windowButton(gtx, &g.minBtn, "min") }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return g.windowButton(gtx, &g.maxBtn, "max") }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return g.windowButton(gtx, &g.winCloseBtn, "close") }),
	)
}

// dragArea fills the free space of the bar: dragging moves the window (when it
// has no system title bar), a double click maximizes it.
func (g *gui) dragArea(gtx layout.Context) layout.Dimensions {
	sz := gtx.Constraints.Max
	if !g.frameless {
		return layout.Dimensions{Size: sz}
	}
	a := clip.Rect{Max: sz}.Push(gtx.Ops)
	system.ActionInputOp(system.ActionMove).Add(gtx.Ops)
	event.Op(gtx.Ops, &g.dragTag)
	a.Pop()
	for {
		ev, ok := gtx.Event(pointer.Filter{Target: &g.dragTag, Kinds: pointer.Press})
		if !ok {
			break
		}
		if e, ok := ev.(pointer.Event); ok && e.Kind == pointer.Press && e.Buttons.Contain(pointer.ButtonPrimary) {
			if time.Since(g.lastDrag) < 400*time.Millisecond {
				g.toggleMaximize()
				g.lastDrag = time.Time{}
			} else {
				g.lastDrag = time.Now()
			}
		}
	}
	return layout.Dimensions{Size: sz}
}

var _ = material.Label

// ---- disconnect and freeze ----

// detachTab disconnects the tab from its session (the session goes on).
func (g *gui) detachTab(t *tab) {
	if !t.h.p.persistent() {
		g.flashInfo("questo tab non e' una sessione ptscry persistente: chiuderlo termina la shell")
		return
	}
	if !t.Detach() {
		g.flashInfo("il tab non e' agganciato a nessuna sessione")
		return
	}
	g.flashInfo("disconnesso: la sessione %q continua a vivere", t.session)
}

// markFrozen records the state on the open tabs of that session.
func (g *gui) markFrozen(profile, session string, on bool) {
	for _, t := range g.allTabs() {
		if t.profile == profile && t.session == session {
			t.mu.Lock()
			t.frozen = on
			t.mu.Unlock()
		}
	}
}

// freezeSession stops (on) or resumes every process of a session.
func (g *gui) freezeSession(profile, session string, on bool) {
	go func() {
		if err := g.hubFor(profile).freeze(session, on); err != nil {
			g.flash("%v", err)
			return
		}
		g.markFrozen(profile, session, on)
		if on {
			g.flashInfo("sessione %q congelata: i programmi sono fermi finche' non la scongeli", session)
		} else {
			g.flashInfo("sessione %q scongelata", session)
		}
		g.invalidate()
	}()
}

func (g *gui) toggleFreeze(t *tab) {
	if !t.h.p.persistent() {
		g.flashInfo("questo tab non e' una sessione ptscry: non c'e' niente da congelare")
		return
	}
	g.freezeSession(t.profile, t.session, !t.isFrozen())
}

// chooseTermFont lets the user pick the terminal font among the system
// monospace ones; the choice applies to every tab and is saved.
func (g *gui) chooseTermFont() {
	g.setMu.Lock()
	cur := g.set.FontFace
	g.setMu.Unlock()
	g.openFontDialog(cur, func(f fontPick) {
		g.setTermFont(f.Name, f.Path)
		g.saveWorkspace()
	})
}

// setTermFont makes the terminals use a font: by name, and from its file when
// known (loaded into the shaper, so that it works whatever the system font
// lookup finds). An empty name is Go Mono.
func (g *gui) setTermFont(name, path string) {
	g.setMu.Lock()
	g.set.FontFace, g.set.FontFile = name, path
	g.setMu.Unlock()
	g.termShaper = newTermShaper(path)
	for _, t := range g.allTabs() {
		t.view.shaper = g.terminalShaper()
		t.view.fontSet, t.view.face = false, name
	}
	g.invalidate()
}
