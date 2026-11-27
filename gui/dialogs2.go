// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// dialogs2.go: session manager, named workspaces, tab rename.

package main

import (
	"fmt"
	"image/color"
	"sort"
	"strings"
	"sync"
	"time"

	"gioui.org/io/key"
	"gioui.org/layout"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

func (g *gui) smallButton(c *widget.Clickable, label string, bg color.NRGBA) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		b := material.Button(g.th, c, label)
		b.Background, b.Color = bg, theme.fg
		// dark text on a light button (the amber of the main action)
		if int(bg.R)*299+int(bg.G)*587+int(bg.B)*114 > 140*1000 {
			b.Color = theme.chrome
		}
		b.TextSize = 12
		b.Inset = layout.UniformInset(5)
		return b.Layout(gtx)
	}
}

// ---- session manager ----

type sessionsDialog struct {
	g       *gui
	profile widget.Enum
	names   []string
	refresh widget.Clickable
	close   widget.Clickable

	mu      sync.Mutex
	list    []sessInfo
	status  string
	loading bool
	loaded  string // profile the list belongs to

	open, ro, kill, frz []widget.Clickable
	confirm             int // row waiting for the second click on "Termina", -1 none
}

func (g *gui) openSessionsDialog() { g.openSessionsDialogFor("") }

// openSessionsDialogFor opens the sessions of a server ("" = the current tab's).
func (g *gui) openSessionsDialogFor(prof string) {
	d := &sessionsDialog{g: g, confirm: -1}
	g.setMu.Lock()
	for _, p := range g.set.Profiles {
		d.names = append(d.names, p.Name)
	}
	cur := g.set.Default
	g.setMu.Unlock()
	if g.active >= 0 && g.active < len(g.tabs) && !g.home {
		cur = g.curTab().profile
	}
	if prof != "" {
		cur = prof
	}
	d.profile.Value = cur
	g.setDialog(d)
	d.reload()
}

func (d *sessionsDialog) Cancel() {}

// reload asks the server for the sessions, without blocking the window.
func (d *sessionsDialog) reload() {
	prof := d.profile.Value
	d.mu.Lock()
	d.loading, d.status = true, ""
	d.mu.Unlock()
	go func() {
		list, err := d.g.hubFor(prof).sessions(true)
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.profile.Value != prof {
			return // the person has chosen another profile meanwhile
		}
		d.loading, d.loaded, d.list = false, prof, list
		d.open, d.ro, d.kill, d.frz = make([]widget.Clickable, len(list)), make([]widget.Clickable, len(list)), make([]widget.Clickable, len(list)), make([]widget.Clickable, len(list))
		d.confirm = -1
		if err != nil {
			d.status = "impossibile leggere le sessioni: " + err.Error()
		} else if len(list) == 0 {
			d.status = "nessuna sessione su questo server"
		}
		d.g.invalidate()
	}()
}

func (d *sessionsDialog) Layout(gtx layout.Context, g *gui) layout.Dimensions {
	if d.close.Clicked(gtx) {
		g.closeDialog()
	}
	if d.refresh.Clicked(gtx) {
		d.reload()
	}
	d.mu.Lock()
	loaded, loading, status := d.loaded, d.loading, d.status
	list := append([]sessInfo(nil), d.list...)
	d.mu.Unlock()
	if d.profile.Update(gtx) && d.profile.Value != loaded {
		d.mu.Lock()
		d.list, d.loaded = nil, ""
		d.mu.Unlock()
		d.reload()
	}
	kids := []layout.FlexChild{g.row(gtx, g.label("Sessioni del server"))}
	for _, n := range d.names {
		n := n
		kids = append(kids, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return material.RadioButton(g.th, &d.profile, n, n).Layout(gtx)
		}))
	}
	kids = append(kids, layout.Rigid(layout.Spacer{Height: 8}.Layout))
	if loading {
		kids = append(kids, g.row(gtx, g.label("lettura in corso...")))
	}
	if status != "" && !loading {
		kids = append(kids, g.row(gtx, g.label(status)))
	}
	d.mu.Lock()
	for i := range list {
		if i >= len(d.open) {
			break
		}
		i, s := i, list[i]
		open, ro, kill, frz := &d.open[i], &d.ro[i], &d.kill[i], &d.frz[i]
		if frz.Clicked(gtx) {
			g.freezeSession(loaded, s.Name, !s.Frozen)
			go func() {
				time.Sleep(900 * time.Millisecond)
				d.reload()
			}()
		}
		frzLabel := "Congela"
		if s.Frozen {
			frzLabel = "Scongela"
		}
		if open.Clicked(gtx) {
			g.openSession(loaded, s.Name, false)
		}
		if ro.Clicked(gtx) {
			g.openSession(loaded, s.Name, true)
		}
		if kill.Clicked(gtx) {
			if d.confirm == i {
				d.confirm = -1
				name, prof := s.Name, loaded
				go func() {
					if err := g.hubFor(prof).kill(name); err != nil {
						g.flash("sessione non terminata: %v", err)
					} else {
						g.flashInfo("sessione %q terminata", name)
					}
					time.Sleep(600 * time.Millisecond) // the host takes a moment to forget it
					d.reload()
				}()
			} else {
				d.confirm = i
			}
		}
		killLabel, killBg := "Termina", theme.input
		if d.confirm == i {
			killLabel, killBg = "Sicuro?", theme.warn
		}
		info := fmt.Sprintf("%s    %dx%d", s.Name, s.Cols, s.Rows)
		if s.Attached > 0 {
			info += fmt.Sprintf("    agganciata da %d", s.Attached)
		}
		if s.Frozen {
			info += "    [congelata]"
		}
		if s.Cwd != "" {
			info += "    " + s.Cwd
		}
		kids = append(kids, g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					l := material.Label(g.th, 13, info)
					l.MaxLines = 2
					l.Color = theme.fg
					return l.Layout(gtx)
				}),
				layout.Rigid(layout.Spacer{Height: 4}.Layout),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
						layout.Rigid(g.smallButton(open, "Apri", theme.accent)),
						layout.Rigid(layout.Spacer{Width: 8}.Layout),
						layout.Rigid(g.smallButton(ro, "Sola lettura", theme.input)),
						layout.Rigid(layout.Spacer{Width: 8}.Layout),
						layout.Rigid(g.smallButton(frz, frzLabel, theme.input)),
						layout.Rigid(layout.Spacer{Width: 8}.Layout),
						layout.Rigid(g.smallButton(kill, killLabel, killBg)),
					)
				}),
			)
		}))
	}
	d.mu.Unlock()
	kids = append(kids, g.row(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Spacing: layout.SpaceStart}.Layout(gtx,
			layout.Rigid(g.smallButton(&d.refresh, "Aggiorna", theme.input)),
			layout.Rigid(layout.Spacer{Width: 8}.Layout),
			layout.Rigid(g.smallButton(&d.close, "Chiudi", theme.input)),
		)
	}))
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
}

// openSession shows the tab of a session, or opens one for it.
func (g *gui) openSession(profile, name string, ro bool) {
	g.closeDialog()
	g.tabMu.Lock()
	tabs := append([]*tab(nil), g.tabs...)
	g.tabMu.Unlock()
	for i, t := range tabs {
		if t.profile == profile && t.session == name && t.ro == ro {
			g.active = i
			g.home, g.kbdSide = false, false
			g.saveWorkspace()
			return
		}
	}
	g.openSpec(tabSpec{profile: profile, session: name, ro: ro})
}

// ---- named workspaces ----

type workspacesDialog struct {
	name    widget.Editor
	save    widget.Clickable
	close   widget.Clickable
	open    []widget.Clickable
	del     []widget.Clickable
	confirm int
	focused bool
}

func (g *gui) openWorkspacesDialog() {
	d := &workspacesDialog{confirm: -1}
	d.name.SingleLine, d.name.Submit = true, true
	g.setDialog(d)
}

func (d *workspacesDialog) Cancel() {}

func (d *workspacesDialog) Layout(gtx layout.Context, g *gui) layout.Dimensions {
	if d.close.Clicked(gtx) {
		g.closeDialog()
	}
	saveNow := d.save.Clicked(gtx)
	for {
		ev, ok := d.name.Update(gtx)
		if !ok {
			break
		}
		if _, ok := ev.(widget.SubmitEvent); ok {
			saveNow = true
		}
	}
	if saveNow {
		if n := strings.TrimSpace(d.name.Text()); n == "" || len(n) > 64 || strings.ContainsAny(n, "\x00\r\n") {
			g.flash("dai al workspace un nome (al massimo 64 caratteri)")
		} else {
			g.saveNamedWorkspace(n)
			d.name.SetText("")
			g.flashInfo("workspace %q salvato", n)
		}
	}
	g.setMu.Lock()
	ws := append([]workspace(nil), g.set.Workspaces...)
	g.setMu.Unlock()
	if len(d.open) != len(ws) {
		d.open, d.del, d.confirm = make([]widget.Clickable, len(ws)), make([]widget.Clickable, len(ws)), -1
	}
	kids := []layout.FlexChild{g.row(gtx, g.label("Workspace salvati: i tab aperti ora si staccano (le sessioni restano vive)"))}
	if len(ws) == 0 {
		kids = append(kids, g.row(gtx, g.label("nessun workspace salvato")))
	}
	for i, w := range ws {
		i, w := i, w
		if d.open[i].Clicked(gtx) {
			g.closeDialog()
			g.loadWorkspace(w)
			return layout.Dimensions{}
		}
		if d.del[i].Clicked(gtx) {
			if d.confirm == i {
				g.deleteWorkspace(w.Name)
				d.confirm = -1
				continue
			}
			d.confirm = i
		}
		delLabel, delBg := "Elimina", theme.input
		if d.confirm == i {
			delLabel, delBg = "Sicuro?", theme.warn
		}
		kids = append(kids, g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					l := material.Label(g.th, 14, fmt.Sprintf("%s  (%d tab)", w.Name, len(w.Tabs)))
					l.Color = theme.fg
					l.MaxLines = 1
					return l.Layout(gtx)
				}),
				layout.Rigid(g.smallButton(&d.open[i], "Apri", theme.accent)),
				layout.Rigid(layout.Spacer{Width: 8}.Layout),
				layout.Rigid(g.smallButton(&d.del[i], delLabel, delBg)),
			)
		}))
	}
	if !d.focused {
		d.focused = true
		focusEditor(gtx, &d.name)
	}
	kids = append(kids,
		layout.Rigid(layout.Spacer{Height: 8}.Layout),
		g.row(gtx, g.label("Salva i tab attuali con il nome")),
		g.row(gtx, g.field(&d.name, "lavoro")),
		g.row(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Spacing: layout.SpaceStart}.Layout(gtx,
				layout.Rigid(g.smallButton(&d.close, "Chiudi", theme.input)),
				layout.Rigid(layout.Spacer{Width: 8}.Layout),
				layout.Rigid(g.smallButton(&d.save, "Salva", theme.accent)),
			)
		}),
	)
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, kids...)
}

// saveNamedWorkspace stores the open tabs under a name (replacing one of the same name).
func (g *gui) saveNamedWorkspace(name string) {
	g.tabMu.Lock()
	tabs := append([]*tab(nil), g.tabs...)
	g.tabMu.Unlock()
	w := workspace{Name: name, Active: g.active}
	for _, t := range tabs {
		w.Tabs = append(w.Tabs, t.state())
	}
	g.setMu.Lock()
	replaced := false
	for i := range g.set.Workspaces {
		if g.set.Workspaces[i].Name == name {
			g.set.Workspaces[i], replaced = w, true
		}
	}
	if !replaced {
		if len(g.set.Workspaces) >= 100 {
			g.setMu.Unlock()
			g.flash("troppi workspace: eliminane qualcuno")
			return
		}
		g.set.Workspaces = append(g.set.Workspaces, w)
	}
	g.setMu.Unlock()
	g.saveWorkspace()
}

func (g *gui) deleteWorkspace(name string) {
	g.setMu.Lock()
	var keep []workspace
	for _, w := range g.set.Workspaces {
		if w.Name != name {
			keep = append(keep, w)
		}
	}
	g.set.Workspaces = keep
	g.setMu.Unlock()
	g.saveWorkspace()
}

// ---- rename ----

type renameDialog struct {
	t       *tab
	ed      widget.Editor
	ok, can widget.Clickable
	focus   bool
}

func (g *gui) openRenameDialog(t *tab) {
	d := &renameDialog{t: t}
	d.ed.SingleLine, d.ed.Submit = true, true
	t.mu.Lock()
	d.ed.SetText(t.label)
	t.mu.Unlock()
	g.setDialog(d)
}

func (d *renameDialog) Cancel() {}

func (d *renameDialog) Layout(gtx layout.Context, g *gui) layout.Dimensions {
	done := d.ok.Clicked(gtx)
	for {
		ev, ok := d.ed.Update(gtx)
		if !ok {
			break
		}
		if _, ok := ev.(widget.SubmitEvent); ok {
			done = true
		}
	}
	if d.can.Clicked(gtx) {
		g.closeDialog()
	}
	if !d.focus {
		d.focus = true
		focusEditor(gtx, &d.ed)
	}
	if done {
		l := strings.TrimSpace(d.ed.Text())
		if len(l) > 80 || strings.ContainsAny(l, "\x00\r\n") {
			g.flash("nome troppo lungo")
		} else {
			d.t.mu.Lock()
			d.t.label = l
			d.t.mu.Unlock()
			g.saveWorkspace()
			g.closeDialog()
		}
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		g.row(gtx, g.label("Nome del tab (vuoto = quello della sessione). La sessione sul server non cambia nome.")),
		g.row(gtx, g.field(&d.ed, d.t.session)),
		g.row(gtx, g.buttons(&d.ok, &d.can, "OK")),
	)
}

func focusEditor(gtx layout.Context, ed *widget.Editor) { gtx.Execute(key.FocusCmd{Tag: ed}) }
